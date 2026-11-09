package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// kosyncTestMux serves the sync API and the web readers' progress API.
func kosyncTestMux() *http.ServeMux {
	mux := progressMux()
	registerKosyncRoutes(mux)
	return mux
}

// resetKosyncCaches forgets in-memory state tied to a database (user ids,
// library files), for tests that start with fresh ones.
func resetKosyncCaches() {
	webScans.Lock()
	clear(webScans.done)
	webScans.Unlock()
	libraryNames.mu.Lock()
	libraryNames.ids = nil
	libraryNames.mu.Unlock()
}

func freshGuessLimiter(t *testing.T) {
	orig := kosyncGuesses
	kosyncGuesses = &guessLimiter{fails: map[string]map[string]time.Time{}, max: 10, window: 15 * time.Minute, now: time.Now}
	t.Cleanup(func() { kosyncGuesses = orig })
}

// device sends a request as KOReader does; key is the sync key as typed
// (KOReader sends its MD5).
func device(t *testing.T, h http.Handler, method, path, user, key, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Accept", "application/vnd.koreader.v1+json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.Header.Set("x-auth-user", user)
		req.Header.Set("x-auth-key", md5Hex(key))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: not JSON (%d): %s", method, path, rec.Code, rec.Body)
	}
	return rec, out
}

func TestKosyncAuth(t *testing.T) {
	u, _ := setupLibraryAndUser(t, 0)
	freshGuessLimiter(t)
	mux := kosyncTestMux()

	if rec, out := device(t, mux, "GET", "/kosync/healthcheck", "", "", ""); rec.Code != 200 || out["state"] != "OK" {
		t.Fatalf("healthcheck: %d %v", rec.Code, out)
	}
	// Accounts come from CalibreFast: 402 is what KOReader expects, and it
	// shows the message.
	rec, out := device(t, mux, "POST", "/kosync/users/create", "", "", `{"username":"new","password":"`+md5Hex("pw")+`"}`)
	if rec.Code != http.StatusPaymentRequired || out["code"] != 2005.0 || !strings.Contains(out["message"].(string), "sync key") {
		t.Fatalf("register: %d %v", rec.Code, out)
	}
	if n := countUsers(t); n != 1 {
		t.Fatalf("register created an account: %d users", n)
	}

	auth := func(user, key string) int {
		rec, out := device(t, mux, "GET", "/kosync/users/auth", user, key, "")
		if rec.Code == 200 && out["authorized"] != "OK" {
			t.Fatalf("auth body: %v", out)
		}
		if rec.Code == 401 && out["code"] != 2001.0 {
			t.Fatalf("401 code: %v", out)
		}
		return rec.Code
	}
	// The account password is not a sync key.
	if code := auth("reader", "longenough-pw"); code != 401 {
		t.Fatalf("account password accepted: %d", code)
	}
	key, err := newSyncKey(u.ID)
	if err != nil || !regexp.MustCompile(`^[a-z]{16}$`).MatchString(key) || strings.ContainsAny(key, "lo") {
		t.Fatalf("key %q, %v", key, err)
	}
	// Only a hash of what KOReader sends is stored.
	var stored string
	_ = appDB.QueryRow(`SELECT key_hash FROM sync_keys WHERE user_id = ?`, u.ID).Scan(&stored)
	if stored == key || stored == md5Hex(key) || stored != syncKeyHash(md5Hex(key)) {
		t.Fatalf("stored %q", stored)
	}
	if code := auth("reader", key); code != 200 {
		t.Fatalf("good key: %d", code)
	}
	if code := auth("READER", key); code != 200 {
		t.Fatalf("usernames are case-insensitive: %d", code)
	}
	var used sql.NullInt64
	_ = appDB.QueryRow(`SELECT last_used_at FROM sync_keys WHERE user_id = ?`, u.ID).Scan(&used)
	if !used.Valid {
		t.Fatal("last use not recorded")
	}
	if code := auth("reader", key+"x"); code != 401 {
		t.Fatalf("bad key: %d", code)
	}
	if code := auth("nobody", key); code != 401 {
		t.Fatalf("unknown user: %d", code)
	}
	if code := auth("", ""); code != 401 {
		t.Fatalf("no credentials: %d", code)
	}
	// The plain key (not its MD5) isn't a credential either.
	req := httptest.NewRequest("GET", "/kosync/users/auth", nil)
	req.Header.Set("x-auth-user", "reader")
	req.Header.Set("x-auth-key", key)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("plain key: %d", rr.Code)
	}

	// Replacing the key retires the old one; revoking retires it too.
	newKey, _ := newSyncKey(u.ID)
	if auth("reader", key) != 401 || auth("reader", newKey) != 200 {
		t.Fatal("replaced key")
	}
	_ = revokeSyncKey(u.ID)
	if code := auth("reader", newKey); code != 401 {
		t.Fatalf("revoked key: %d", code)
	}
	// A disabled account can't sync.
	newKey, _ = newSyncKey(u.ID)
	_, _ = appDB.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, u.ID)
	if code := auth("reader", newKey); code != 401 {
		t.Fatalf("disabled account: %d", code)
	}
	// Every authenticated route checks.
	for _, r := range [][2]string{{"PUT", "/kosync/syncs/progress"}, {"GET", "/kosync/syncs/progress/abc"}} {
		if rec, _ := device(t, mux, r[0], r[1], "reader", "wrong", `{}`); rec.Code != 401 {
			t.Fatalf("%s %s: %d", r[0], r[1], rec.Code)
		}
	}
}

func countUsers(t *testing.T) int {
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

func TestKosyncPrivateLibrary(t *testing.T) {
	u, _ := setupLibraryAndUser(t, 0)
	freshGuessLimiter(t)
	key, _ := newSyncKey(u.ID)
	origPublic := publicCatalog
	publicCatalog = false
	t.Cleanup(func() { publicCatalog = origPublic })
	h := withAuth(kosyncTestMux())
	// Devices have no session; the API checks its own credentials.
	if rec, _ := device(t, h, "GET", "/kosync/users/auth", "reader", key, ""); rec.Code != 200 {
		t.Fatalf("through the private-library guard: %d", rec.Code)
	}
	// The server address without /kosync gets an answer KOReader shows.
	rec, out := device(t, h, "GET", "/users/auth", "reader", key, "")
	if rec.Code != 401 || !strings.Contains(out["message"].(string), "/kosync") {
		t.Fatalf("missing /kosync: %d %v", rec.Code, out)
	}
	if rec, _ := device(t, h, "POST", "/users/create", "", "", `{}`); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("missing /kosync, register: %d", rec.Code)
	}
	// The rest of a private library stays closed.
	if rec := call(t, h, nil, "GET", "/api/me/progress/x?format=epub", ""); rec.Code != 401 {
		t.Fatalf("web API without a session: %d", rec.Code)
	}
}

func TestKosyncRateLimit(t *testing.T) {
	u, _ := setupLibraryAndUser(t, 0)
	freshGuessLimiter(t)
	key, _ := newSyncKey(u.ID)
	mux := kosyncTestMux()

	// A device repeating a stale key counts once, so it can't lock others out.
	for i := 0; i < 30; i++ {
		if rec, _ := device(t, mux, "PUT", "/kosync/syncs/progress", "reader", "stale", `{}`); rec.Code != 401 {
			t.Fatalf("stale key, try %d: %d", i, rec.Code)
		}
	}
	if rec, _ := device(t, mux, "GET", "/kosync/users/auth", "reader", key, ""); rec.Code != 200 {
		t.Fatalf("good key after a stale one: %d", rec.Code)
	}
	// Guessing different keys is blocked after ten, even the right one.
	for i := 0; i < 9; i++ {
		device(t, mux, "GET", "/kosync/users/auth", "reader", "guess"+itoa(int64(i)), "")
	}
	rec, out := device(t, mux, "GET", "/kosync/users/auth", "reader", key, "")
	if rec.Code != http.StatusTooManyRequests || out["message"] == "" {
		t.Fatalf("after ten wrong keys: %d %v", rec.Code, out)
	}
	// The window passes.
	kosyncGuesses.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if rec, _ := device(t, mux, "GET", "/kosync/users/auth", "reader", key, ""); rec.Code != 200 {
		t.Fatalf("after the window: %d", rec.Code)
	}
}

func TestKosyncProgressRoundTrip(t *testing.T) {
	u, _ := setupLibraryAndUser(t, 0)
	freshGuessLimiter(t)
	key, _ := newSyncKey(u.ID)
	other, _ := createUser("other", "longenough-pw", false)
	otherKey, _ := newSyncKey(other.ID)
	mux := kosyncTestMux()
	put := func(user, key, body string) (int, map[string]any) {
		rec, out := device(t, mux, "PUT", "/kosync/syncs/progress", user, key, body)
		return rec.Code, out
	}
	get := func(user, key, doc string) (int, map[string]any) {
		rec, out := device(t, mux, "GET", "/kosync/syncs/progress/"+doc, user, key, "")
		return rec.Code, out
	}

	// A document that matches no library file is stored all the same, so
	// devices sync each other.
	doc := "0b229176d4e8db7f6d2b5a4952368d7a"
	xp := "/body/DocFragment[20]/body/p[22]/img.0"
	before := time.Now().Unix()
	code, out := put("reader", key, `{"document":"`+doc+`","progress":"`+xp+`","percentage":0.31879884821061,
		"device":"Kobo Clara","device_id":"A1B2","metadata":{"filename":"x.epub","title":"T","authors":"A"}}`)
	if code != 200 || out["document"] != doc || len(out) != 2 {
		t.Fatalf("put: %d %v", code, out)
	}
	if ts := int64(out["timestamp"].(float64)); ts < before || ts > time.Now().Unix() {
		t.Fatalf("timestamp must be server time in seconds: %d", ts)
	}
	code, out = get("reader", key, doc)
	if code != 200 || out["progress"] != xp || out["percentage"] != 0.31879884821061 || out["device"] != "Kobo Clara" ||
		out["device_id"] != "A1B2" || out["document"] != doc || out["timestamp"] == nil {
		t.Fatalf("get: %d %v", code, out)
	}
	// Last writer wins, even backwards; a page number stays a string; no
	// device_id means none is returned.
	if code, _ := put("reader", key, `{"document":"`+doc+`","progress":"56","percentage":0.1,"device":"Phone"}`); code != 200 {
		t.Fatal(code)
	}
	if _, out = get("reader", key, doc); out["progress"] != "56" || out["percentage"] != 0.1 || out["device_id"] != nil {
		t.Fatalf("overwrite: %v", out)
	}
	// Positions are per user.
	if code, out := get("other", otherKey, doc); code != 200 || len(out) != 0 {
		t.Fatalf("other user sees it: %v", out)
	}
	// An unknown document is 200 {}, which KOReader reads as "no progress".
	rec, _ := device(t, mux, "GET", "/kosync/syncs/progress/ffffffffffffffffffffffffffffffff", "reader", key, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("unknown document: %d %s", rec.Code, rec.Body)
	}

	for body, want := range map[string]float64{
		`{"progress":"1","percentage":0.1,"device":"d"}`:                                                 2004,
		`{"document":"a:b","progress":"1","percentage":0.1,"device":"d"}`:                                2004,
		`{"document":"abc","percentage":0.1,"device":"d"}`:                                               2003,
		`{"document":"abc","progress":"1","percentage":"x","device":"d"}`:                                2003,
		`{"document":"abc","progress":"1","percentage":0.1}`:                                             2003,
		`{"document":"abc","progress":"1","percentage":0.1,"device":"` + strings.Repeat("d", 300) + `"}`: 2003,
	} {
		if code, out := put("reader", key, body); code != 403 || out["code"] != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if code, out := put("reader", key, `[1]`); code != 400 || out["code"] == nil {
		t.Errorf("array body: %d %v", code, out)
	}
	// A numeric-string percentage is accepted, as by the reference server.
	if code, _ := put("reader", key, `{"document":"abc","progress":"1","percentage":"0.5","device":"d"}`); code != 200 {
		t.Errorf("string percentage: %d", code)
	}
	if code, out := get("reader", key, "a%3Ab"); code != 403 || out["code"] != 2004.0 {
		t.Errorf("bad document on GET: %d %v", code, out)
	}
}

// libraryBook gives a test book a format file, and returns the ids KOReader
// would use for it (binary, filename).
func libraryBook(t *testing.T, uuid, name, format string, content []byte) (binary, filename string) {
	addBookFile(t, uuid, name, format, content)
	binary, _ = partialMD5(bytes.NewReader(content))
	return binary, fileNameDigest(name + "." + format)
}

type webProgress struct {
	Progress *progressRec `json:"progress"`
}

func webGet(t *testing.T, mux http.Handler, u *User, uuid, format string) *progressRec {
	t.Helper()
	var out webProgress
	rec := call(t, mux, u, "GET", "/api/me/progress/"+uuid+"?format="+format, "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("web GET: %d %s", rec.Code, rec.Body)
	}
	return out.Progress
}

func webPut(t *testing.T, mux http.Handler, u *User, uuid, format, pos string, pct float64, at int64) {
	t.Helper()
	rec := call(t, mux, u, "PUT", "/api/me/progress/"+uuid,
		`{"format":"`+format+`","position":"`+pos+`","percent":`+jsonNum(pct)+`,"updated_at":`+itoa(at)+`}`)
	if rec.Code != 200 {
		t.Fatalf("web PUT: %d %s", rec.Code, rec.Body)
	}
}

func TestKosyncJoinsWebReaderEPUB(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	freshGuessLimiter(t)
	key, _ := newSyncKey(u.ID)
	book := books[0]
	binary, filename := libraryBook(t, book, "Book 0 - A", "epub", testEPUB(t))
	mux := kosyncTestMux()

	// Opening the book on the web hashes its file, so devices are recognised.
	if p := webGet(t, mux, u, book, "epub"); p != nil {
		t.Fatalf("no progress yet: %+v", p)
	}
	xp := "/body/DocFragment[2]/body/div/p[3]/text().5"
	device(t, mux, "PUT", "/kosync/syncs/progress", "reader", key,
		`{"document":"`+binary+`","progress":"`+xp+`","percentage":0.4,"device":"Kobo","device_id":"dev1"}`)
	if s, src := readStatus(u.ID, book); s != "reading" || src != "auto" {
		t.Fatalf("device reading should mark the book: %s/%s", s, src)
	}

	// The web reader resumes at the device's paragraph.
	p := webGet(t, mux, u, book, "epub")
	if p == nil || p.Position != "epubcfi(/6/6!/4/4/8)" || p.Percent != 0.4 {
		t.Fatalf("web after device: %+v", p)
	}
	// The device reads back exactly what it sent.
	_, out := device(t, mux, "GET", "/kosync/syncs/progress/"+binary, "reader", key, "")
	if out["progress"] != xp || out["device"] != "Kobo" || out["device_id"] != "dev1" {
		t.Fatalf("device's own position: %v", out)
	}

	// Reading on, on the web: the device gets the web position as an XPointer.
	webAt := p.UpdatedAt + 5000
	webPut(t, mux, u, book, "epub", "epubcfi(/6/8!/4/2/4/1:0)", 0.9, webAt)
	_, out = device(t, mux, "GET", "/kosync/syncs/progress/"+binary, "reader", key, "")
	if out["progress"] != "/body/DocFragment[3]/body/section/p[2]" || out["percentage"] != 0.9 ||
		out["device"] != webReaderDevice || out["timestamp"] != float64(webAt/1000) || out["document"] != binary {
		t.Fatalf("device after web: %v", out)
	}
	// So does a device that matches books by file name.
	_, out = device(t, mux, "GET", "/kosync/syncs/progress/"+filename, "reader", key, "")
	if out["progress"] != "/body/DocFragment[3]/body/section/p[2]" || out["document"] != filename {
		t.Fatalf("filename device after web: %v", out)
	}

	// That device reads on: the first device and the web get its position.
	device(t, mux, "PUT", "/kosync/syncs/progress", "reader", key,
		`{"document":"`+filename+`","progress":"/body/DocFragment[3]/body/section/p[1]","percentage":0.98,"device":"Phone"}`)
	_, _ = appDB.Exec(`UPDATE device_progress SET updated_at = ? WHERE document = ?`, webAt+1000, filename)
	_, out = device(t, mux, "GET", "/kosync/syncs/progress/"+binary, "reader", key, "")
	if out["progress"] != "/body/DocFragment[3]/body/section/p[1]" || out["device"] != "Phone" || out["document"] != binary {
		t.Fatalf("first device after second: %v", out)
	}
	if p := webGet(t, mux, u, book, "epub"); p.Position != "epubcfi(/6/8!/4/2/2)" || p.UpdatedAt != webAt+1000 {
		t.Fatalf("web after second device: %+v", p)
	}
	if s, _ := readStatus(u.ID, book); s != "finished" {
		t.Fatalf("98%% on a device should mark finished: %s", s)
	}
}

func TestKosyncJoinsWebReaderPDF(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	freshGuessLimiter(t)
	key, _ := newSyncKey(u.ID)
	book := books[0]
	// Only the file name is used: a device that matches by name needs no
	// hashing at all.
	_, filename := libraryBook(t, book, "Scan", "pdf", []byte("%PDF-1.4 not really"))
	mux := kosyncTestMux()

	device(t, mux, "PUT", "/kosync/syncs/progress", "reader", key,
		`{"document":"`+filename+`","progress":"56","percentage":0.28,"device":"Kindle"}`)
	if p := webGet(t, mux, u, book, "pdf"); p == nil || p.Position != "56" || p.Percent != 0.28 {
		t.Fatalf("web after device: %+v", p)
	}
	webPut(t, mux, u, book, "pdf", "60", 0.3, time.Now().UnixMilli()+1000)
	if _, out := device(t, mux, "GET", "/kosync/syncs/progress/"+filename, "reader", key, ""); out["progress"] != "60" || out["percentage"] != 0.3 {
		t.Fatalf("device after web: %v", out)
	}
	// Another format of the book isn't affected.
	if p := webGet(t, mux, u, book, "epub"); p != nil {
		t.Fatalf("epub: %+v", p)
	}
}

func TestSyncKeyAccountPage(t *testing.T) {
	u, _ := setupLibraryAndUser(t, 0)
	freshGuessLimiter(t)
	mux := kosyncTestMux()
	mux.HandleFunc("/account", requireUser(accountHandler))
	post := func(action string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/account/sync-key", strings.NewReader(url.Values{"action": {action}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(req, u))
		return rec
	}

	page := call(t, mux, u, "GET", "/account", "").Body.String()
	if !strings.Contains(page, "Create sync key") || !strings.Contains(page, "/kosync") {
		t.Fatal("account page lacks the device sync section")
	}
	rec := post("generate")
	m := regexp.MustCompile(`<code class="sync-key">([a-z]{16})</code>`).FindStringSubmatch(rec.Body.String())
	if rec.Code != 200 || m == nil || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("generate: %d, key shown: %v", rec.Code, m)
	}
	if r, _ := device(t, mux, "GET", "/kosync/users/auth", "reader", m[1], ""); r.Code != 200 {
		t.Fatalf("shown key doesn't work: %d", r.Code)
	}
	page = call(t, mux, u, "GET", "/account", "").Body.String()
	if strings.Contains(page, m[1]) || !strings.Contains(page, "Replace sync key") {
		t.Fatal("the key must be shown only once")
	}
	if rec := post("revoke"); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if r, _ := device(t, mux, "GET", "/kosync/users/auth", "reader", m[1], ""); r.Code != 401 {
		t.Fatalf("revoked key works: %d", r.Code)
	}
	if rec := post("other"); rec.Code != 400 {
		t.Fatalf("unknown action: %d", rec.Code)
	}
}

// Databases created before this feature have login_links as migration 4
// (links.go used to append it); the sync tables must come after it.
func TestMigrationAfterLoginLinks(t *testing.T) {
	dir := t.TempDir()
	all := migrations
	migrations = all[:4]
	db, err := openAppDB(dir)
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = openAppDB(dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"login_links", "sync_keys", "device_progress", "book_digests"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: %d %v", table, n, err)
		}
	}
	if !strings.Contains(all[3], "login_links") {
		t.Error("migration 4 must stay login_links")
	}

}
