package main

// KOReader progress sync ("kosync"), served under /kosync.
//
// KOReader's Progress sync plugin talks to a small JSON API: it stores one
// reading position per (user, document), where the document is an id the
// device derives from the book (kosync_digest.go). CalibreFast serves that
// API so the user can enter https://<server>/kosync as KOReader's custom
// sync server, and joins device positions with the web readers' positions
// for books it can match to the library: whichever was saved last wins, in
// both directions (kosync_position.go converts EPUB positions).
//
// The protocol: KOReader's plugins/kosync.koplugin and
// github.com/koreader/koreader-sync-server; also described in
// github.com/pid1/kosync-conformance. Notably: an unknown document is
// 200 {}, not 404; progress is a string, round-tripped exactly; timestamps
// are server-generated Unix seconds; only a 401 stops KOReader retrying a
// failed push.
//
// Devices don't use the account password: each user generates a sync key on
// their account page and enters it in KOReader as the password. KOReader
// sends the key's MD5 (x-auth-key); only a SHA-256 of that MD5 is stored.
// Accounts come from CalibreFast, so registering from KOReader is refused.

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kosyncPrefix      = "/kosync"
	maxDeviceNameLen  = 200
	webReaderDevice   = "CalibreFast" // shown by KOReader: "Sync to latest location 42% from device 'CalibreFast'?"
	webReaderDeviceID = "calibrefast-web"
)

// KOReader's document ids are 32 hex digits; allow a little more for other
// clients, but nothing a GET path couldn't carry back.
var kosyncDocumentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Error codes are those of the reference server (KOReader shows the message).
const (
	kosyncErrUnauthorized   = 2001
	kosyncErrInvalidRequest = 2003
	kosyncErrDocument       = 2004
	kosyncErrNoRegistration = 2005
)

func kosyncError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{"code": code, "message": message})
}

// ---------------------------------------------------------------------------
// Sync keys
// ---------------------------------------------------------------------------

// Sync keys are typed on e-reader keyboards: 16 lower-case letters (no l or
// o, which look like 1 and 0), about 73 bits.
const (
	syncKeyLen      = 16
	syncKeyAlphabet = "abcdefghijkmnpqrstuvwxyz"
)

func randomSyncKey() string {
	out := make([]byte, 0, syncKeyLen)
	buf := make([]byte, 32)
	for len(out) < syncKeyLen {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			// Reject the top of the byte range so each letter is equally likely.
			if int(b) < 256/len(syncKeyAlphabet)*len(syncKeyAlphabet) && len(out) < syncKeyLen {
				out = append(out, syncKeyAlphabet[int(b)%len(syncKeyAlphabet)])
			}
		}
	}
	return string(out)
}

// syncKeyHash is what is stored for a key: the SHA-256 of what KOReader
// sends as x-auth-key, the lower-case hex MD5 of the key.
func syncKeyHash(authKey string) string {
	sum := sha256.Sum256([]byte(authKey))
	return hex.EncodeToString(sum[:])
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// newSyncKey replaces the user's sync key (devices using the old one stop
// syncing) and returns the new key, which is never stored.
func newSyncKey(userID int64) (string, error) {
	key := randomSyncKey()
	_, err := appDB.Exec(`INSERT INTO sync_keys (user_id, key_hash, created_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET key_hash = excluded.key_hash, created_at = excluded.created_at, last_used_at = NULL`,
		userID, syncKeyHash(md5Hex(key)), nowUnix())
	return key, err
}

func revokeSyncKey(userID int64) error {
	_, err := appDB.Exec(`DELETE FROM sync_keys WHERE user_id = ?`, userID)
	return err
}

var dummySyncKeyHash = syncKeyHash(md5Hex("calibrefast-timing-dummy"))

// syncKeyUser checks a device's credentials: a CalibreFast username and the
// MD5 of that user's sync key. Unknown users, wrong or revoked keys and
// disabled accounts all fail the same way.
func syncKeyUser(username, authKey string) *User {
	var u User
	var hash sql.NullString
	stored := dummySyncKeyHash
	err := appDB.QueryRow(`SELECT u.id, u.username, u.password_hash, u.is_admin, u.disabled, k.key_hash
		FROM users u JOIN sync_keys k ON k.user_id = u.id WHERE u.username = ?`, username).
		Scan(&u.ID, &u.Username, &hash, &u.IsAdmin, &u.Disabled, &stored)
	match := subtle.ConstantTimeCompare([]byte(syncKeyHash(authKey)), []byte(stored)) == 1
	if err != nil || !match || u.Disabled || username == "" || authKey == "" {
		return nil
	}
	u.HasPass = hash.Valid && hash.String != ""
	now := nowUnix()
	_, _ = appDB.Exec(`UPDATE sync_keys SET last_used_at = ? WHERE user_id = ? AND (last_used_at IS NULL OR last_used_at < ?)`,
		now, u.ID, now-60)
	return &u
}

// guessLimiter stops a client address guessing keys: it may try a limited
// number of different wrong (username, key) pairs per window. A device that
// keeps sending the same stale key (KOReader pushes every 25 seconds while
// reading) counts once, so it can't lock out other devices behind the same
// address.
type guessLimiter struct {
	mu     sync.Mutex
	fails  map[string]map[string]time.Time // address → pair → first failure
	max    int
	window time.Duration
	now    func() time.Time
}

var kosyncGuesses = &guessLimiter{fails: map[string]map[string]time.Time{}, max: 10, window: 15 * time.Minute, now: time.Now}

func (g *guessLimiter) prune(addr string) {
	cut := g.now().Add(-g.window)
	for k, t := range g.fails[addr] {
		if t.Before(cut) {
			delete(g.fails[addr], k)
		}
	}
	if len(g.fails[addr]) == 0 {
		delete(g.fails, addr)
	}
}

func (g *guessLimiter) blocked(addr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(addr)
	return len(g.fails[addr]) >= g.max
}

func (g *guessLimiter) fail(addr, pair string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.fails) > 10000 {
		for a := range g.fails {
			g.prune(a)
		}
	}
	if g.fails[addr] == nil {
		g.fails[addr] = map[string]time.Time{}
	}
	if _, seen := g.fails[addr][pair]; !seen {
		g.fails[addr][pair] = g.now()
	}
}

const kosyncBadCredentials = "Wrong username or sync key. Use your CalibreFast username, and the sync key from your account page as the password."

// kosyncAuth checks a device's x-auth-user / x-auth-key headers.
func kosyncAuth(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := clientIP(r)
		if kosyncGuesses.blocked(addr) {
			// Not 401: KOReader keeps a push that failed this way and retries it.
			kosyncError(w, http.StatusTooManyRequests, kosyncErrUnauthorized, "Too many failed sign-ins. Try again in a few minutes.")
			return
		}
		name := strings.TrimSpace(r.Header.Get("x-auth-user"))
		key := r.Header.Get("x-auth-key")
		u := syncKeyUser(name, key)
		if u == nil {
			kosyncGuesses.fail(addr, strings.ToLower(name)+"\x00"+syncKeyHash(key))
			kosyncError(w, http.StatusUnauthorized, kosyncErrUnauthorized, kosyncBadCredentials)
			return
		}
		h(w, r, u)
	}
}

// ---------------------------------------------------------------------------
// Device positions, and joining them with the web readers'
// ---------------------------------------------------------------------------

// devicePosition is a position stored by a KOReader device.
type devicePosition struct {
	Document   string
	Progress   string
	Percentage float64
	Device     string
	DeviceID   sql.NullString
	UpdatedAt  int64 // ms
}

const devicePositionCols = `document, progress, percentage, device, device_id, updated_at`

func scanDevicePosition(row interface{ Scan(...any) error }) (*devicePosition, error) {
	var d devicePosition
	if err := row.Scan(&d.Document, &d.Progress, &d.Percentage, &d.Device, &d.DeviceID, &d.UpdatedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// newestDevicePosition is the latest position stored under any of ids.
func newestDevicePosition(userID int64, ids []string) *devicePosition {
	if len(ids) == 0 {
		return nil
	}
	d, err := scanDevicePosition(appDB.QueryRow(`SELECT `+devicePositionCols+` FROM device_progress
		WHERE user_id = ? AND document IN (`+placeholders(len(ids))+`) ORDER BY updated_at DESC LIMIT 1`,
		append([]any{userID}, toArgs(ids)...)...))
	if err != nil {
		return nil
	}
	return d
}

func clamp01(f float64) float64 { return math.Max(0, math.Min(1, f)) }

// webPosition converts a device position in a book format for the web reader.
func webPosition(b bookFile, d *devicePosition) (*progressRec, bool) {
	rec := &progressRec{Format: b.Format, Percent: clamp01(d.Percentage), UpdatedAt: d.UpdatedAt}
	switch b.Format {
	case "pdf":
		page, err := strconv.Atoi(strings.TrimSpace(d.Progress))
		if err != nil || page < 1 {
			return nil, false
		}
		rec.Position = strconv.Itoa(page)
	case "epub":
		file, err := bookFilePath(b.UUID, b.Format)
		if err != nil {
			return nil, false
		}
		if rec.Position, err = xpointerToCFI(file, d.Progress); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	return rec, true
}

// devicePositionFromWeb converts a web reader position for a device.
func devicePositionFromWeb(b bookFile, p *progressRec) (string, bool) {
	switch b.Format {
	case "pdf":
		page, err := strconv.Atoi(p.Position)
		if err != nil || page < 1 {
			return "", false
		}
		return strconv.Itoa(page), true
	case "epub":
		file, err := bookFilePath(b.UUID, b.Format)
		if err != nil {
			return "", false
		}
		xp, err := cfiToXPointer(file, p.Position)
		return xp, err == nil
	}
	return "", false
}

// deviceProgressForWeb is the newest position a device stored for a book
// format, converted for the web reader, or nil. (It also caches the file's
// partial MD5, so devices asking about this book later are recognised.)
func deviceProgressForWeb(userID int64, uuid, format string) *progressRec {
	d := newestDevicePosition(userID, bookDocumentIDs(uuid, format))
	if d == nil {
		return nil
	}
	rec, ok := webPosition(bookFile{UUID: uuid, Format: format}, d)
	if !ok {
		return nil
	}
	return rec
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func kosyncHealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "OK"})
}

// kosyncRegisterHandler refuses KOReader's "Register": accounts are
// CalibreFast's. 402 is the only failure KOReader expects here, and it shows
// the message.
func kosyncRegisterHandler(w http.ResponseWriter, r *http.Request) {
	kosyncError(w, http.StatusPaymentRequired, kosyncErrNoRegistration,
		"Accounts can't be registered from KOReader. Use Login with your CalibreFast username, and the sync key from your account page as the password.")
}

func kosyncAuthorizeHandler(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, http.StatusOK, map[string]string{"authorized": "OK"})
}

// kosyncPutHandler stores a device's position (last writer wins, as the
// reference server does) and, for a book in the library, marks it as being
// read or finished.
func kosyncPutHandler(w http.ResponseWriter, r *http.Request, u *User) {
	var in map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil || in == nil {
		kosyncError(w, http.StatusBadRequest, kosyncErrInvalidRequest, "The body must be a JSON object.")
		return
	}
	doc, _ := in["document"].(string)
	if !kosyncDocumentRe.MatchString(doc) {
		kosyncError(w, http.StatusForbidden, kosyncErrDocument, "Field 'document' not provided.")
		return
	}
	percentage, okPct := jsonFloat(in["percentage"])
	progress, okProg := jsonString(in["progress"])
	device, okDev := in["device"].(string)
	var deviceID sql.NullString
	if s, ok := in["device_id"].(string); ok {
		deviceID = sql.NullString{String: s, Valid: true}
	}
	if !okPct || !okProg || !okDev || len(progress) > maxPositionLen ||
		len(device) > maxDeviceNameLen || len(deviceID.String) > maxDeviceNameLen {
		kosyncError(w, http.StatusForbidden, kosyncErrInvalidRequest, "Invalid request")
		return
	}
	now := time.Now().UnixMilli()
	if _, err := appDB.Exec(`INSERT INTO device_progress (user_id, document, progress, percentage, device, device_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, document) DO UPDATE SET progress = excluded.progress, percentage = excluded.percentage,
			device = excluded.device, device_id = excluded.device_id, updated_at = excluded.updated_at`,
		u.ID, doc, progress, percentage, device, deviceID, now); err != nil {
		log.Println("kosync: save progress:", err)
		kosyncError(w, http.StatusBadGateway, 2000, "Unknown server error.")
		return
	}
	// Opening a book (0%) isn't reading it yet, as in the web readers.
	if b, ok := resolveDocument(u.ID, doc); ok && percentage > 0 {
		status := "reading"
		if percentage >= finishedPercent {
			status = "finished"
		}
		_ = setStatus(u.ID, b.UUID, status, "auto")
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": doc, "timestamp": now / 1000})
}

// kosyncGetHandler returns the latest position for a document: the device
// position stored under it or under another id of the same book format, or
// the web reader's when that is newer. Unknown documents are 200 {}.
func kosyncGetHandler(w http.ResponseWriter, r *http.Request, u *User) {
	doc := r.PathValue("document")
	if !kosyncDocumentRe.MatchString(doc) {
		kosyncError(w, http.StatusForbidden, kosyncErrDocument, "Field 'document' not provided.")
		return
	}
	ids := []string{doc}
	b, known := resolveDocument(u.ID, doc)
	if known {
		ids = append(ids, bookDocumentIDs(b.UUID, b.Format)...)
	}
	d := newestDevicePosition(u.ID, ids)
	if known && validFormat(b.Format) {
		if web, err := getProgress(u.ID, b.UUID, b.Format); err == nil && (d == nil || web.UpdatedAt > d.UpdatedAt) {
			if progress, ok := devicePositionFromWeb(b, web); ok {
				writeJSON(w, http.StatusOK, map[string]any{
					"document":   doc,
					"progress":   progress,
					"percentage": web.Percent,
					"device":     webReaderDevice,
					"device_id":  webReaderDeviceID,
					"timestamp":  web.UpdatedAt / 1000,
				})
				return
			}
		}
	}
	if d == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	out := map[string]any{
		"document":   doc,
		"progress":   d.Progress,
		"percentage": d.Percentage,
		"device":     d.Device,
		"timestamp":  d.UpdatedAt / 1000,
	}
	if d.DeviceID.Valid {
		out["device_id"] = d.DeviceID.String
	}
	writeJSON(w, http.StatusOK, out)
}

// kosyncWrongURLHandler answers KOReader when the server address was entered
// without /kosync, instead of letting the library's own pages answer.
func kosyncWrongURLHandler(w http.ResponseWriter, r *http.Request) {
	msg := "This is CalibreFast: set the custom sync server to " + kosyncServerURL(r) + " (ending in /kosync)."
	if r.URL.Path == "/users/create" {
		kosyncError(w, http.StatusPaymentRequired, kosyncErrNoRegistration, msg)
		return
	}
	kosyncError(w, http.StatusUnauthorized, kosyncErrUnauthorized, msg)
}

// jsonFloat accepts a JSON number or a numeric string (as the reference
// server does).
func jsonFloat(v any) (float64, bool) {
	var s string
	switch t := v.(type) {
	case json.Number:
		s = t.String()
	case string:
		s = strings.TrimSpace(t)
	default:
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// jsonString accepts a string, or a number (stored as written).
func jsonString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// isKosyncPath: the sync API authenticates devices itself (no session).
func isKosyncPath(p string) bool {
	return strings.HasPrefix(p, kosyncPrefix+"/") || p == "/users/auth" || p == "/users/create" ||
		p == "/syncs/progress" || strings.HasPrefix(p, "/syncs/progress/")
}

func registerKosyncRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+kosyncPrefix+"/healthcheck", kosyncHealthHandler)
	mux.HandleFunc("POST "+kosyncPrefix+"/users/create", kosyncRegisterHandler)
	mux.HandleFunc("GET "+kosyncPrefix+"/users/auth", kosyncAuth(kosyncAuthorizeHandler))
	mux.HandleFunc("PUT "+kosyncPrefix+"/syncs/progress", kosyncAuth(kosyncPutHandler))
	mux.HandleFunc("GET "+kosyncPrefix+"/syncs/progress/{document}", kosyncAuth(kosyncGetHandler))
	mux.HandleFunc("POST /users/create", kosyncWrongURLHandler)
	mux.HandleFunc("GET /users/auth", kosyncWrongURLHandler)
	mux.HandleFunc("PUT /syncs/progress", kosyncWrongURLHandler)
	mux.HandleFunc("GET /syncs/progress/{document}", kosyncWrongURLHandler)
	mux.HandleFunc("POST /account/sync-key", requireUser(syncKeyHandler))
}

// ---------------------------------------------------------------------------
// Account page: generate, replace and revoke the sync key
// ---------------------------------------------------------------------------

// syncKeyInfo describes the user's sync key on the account page.
type syncKeyInfo struct {
	Created    string
	LastUsed   string
	LastDevice string // device of the latest synced position
}

// kosyncServerURL is the address to enter in KOReader.
func kosyncServerURL(r *http.Request) string {
	return baseURL(r) + kosyncPrefix
}

func formatDay(unix int64) string { return time.Unix(unix, 0).Format("2 Jan 2006 15:04") }

// fillSyncKeyData adds the device sync section's data to the account page.
func fillSyncKeyData(data *AuthPageData, r *http.Request) {
	data.SyncURL = kosyncServerURL(r)
	if r.URL.Query().Get("sync") == "revoked" {
		data.SyncNotice = "Sync key revoked. Devices using it can no longer sync."
	}
	var created int64
	var used sql.NullInt64
	if appDB.QueryRow(`SELECT created_at, last_used_at FROM sync_keys WHERE user_id = ?`, data.User.ID).Scan(&created, &used) != nil {
		return
	}
	info := &syncKeyInfo{Created: formatDay(created)}
	if used.Valid {
		info.LastUsed = formatDay(used.Int64)
	}
	_ = appDB.QueryRow(`SELECT device FROM device_progress WHERE user_id = ? ORDER BY updated_at DESC LIMIT 1`, data.User.ID).Scan(&info.LastDevice)
	data.SyncKey = info
}

// syncKeyHandler: POST /account/sync-key (action=generate|revoke). A new key
// is shown on the page that answers the POST, once; it is never stored.
func syncKeyHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	switch r.FormValue("action") {
	case "generate":
		key, err := newSyncKey(u.ID)
		if err != nil {
			http.Error(w, "could not create a sync key", http.StatusInternalServerError)
			return
		}
		log.Printf("user %s created a device sync key", u.Username)
		data := AuthPageData{User: u, Identities: identitiesFor(u.ID), NewSyncKey: key}
		fillSyncKeyData(&data, r)
		renderAuthPage(w, "account.html", data, http.StatusOK)
	case "revoke":
		if err := revokeSyncKey(u.ID); err != nil {
			http.Error(w, "could not revoke the sync key", http.StatusInternalServerError)
			return
		}
		log.Printf("user %s revoked their device sync key", u.Username)
		http.Redirect(w, r, "/account?sync=revoked#device-sync", http.StatusSeeOther)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}
