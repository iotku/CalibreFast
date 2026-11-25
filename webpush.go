package main

// System notifications (Web Push) for users who turn them on, per device.
//
// Notifications live in the app (notifications.go); this only lets a device
// know there's a new one while the app is closed. A push goes through the
// browser maker's push service (Apple, Google, Mozilla, Microsoft), a third
// party, so it's off until the user turns it on for a device (preferences,
// or the offer in the notifications menu of the installed app), and what
// reaches that service is kept to the least it takes:
//
//   - a push carries no payload: no title, no author, no count. The push
//     service learns only that this server has something for that device,
//     and when. The service worker (web/sw.ts) then asks this server for
//     the user's unseen notifications and shows them.
//   - the only thing kept per device is the push service's endpoint (no
//     encryption keys, since nothing is encrypted to send). Turning it off,
//     signing out on the device, or the service saying the endpoint is gone
//     deletes it.
//   - pushes go only to known push services (pushHosts), over HTTPS, with
//     no redirects, so a user can't make this server call anywhere else.
//   - -no-web-push turns it off on the server: nothing is sent and nobody
//     can turn it on.
//
// Requests are signed with VAPID (RFC 8292): an ECDSA P-256 key made the
// first time, kept in settings sealed with secrets.key (secretbox.go).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var noWebPush bool // -no-web-push

// pushDelay gathers what one change delivers (a batch of favorites news for
// many users) into one push per device, after its transaction.
var pushDelay = 3 * time.Second

const (
	settingVAPIDKey = "vapid_key"
	pushMaxEndpoint = 1024
	// pushMaxFailures: an endpoint that failed this often in a row goes.
	pushMaxFailures = 10
	pushTTL         = "86400" // a day: the in-app list has it anyway
)

// pushHosts: the push services browsers use. An endpoint must be on one of
// them (the host or a subdomain).
var pushHosts = []string{
	"fcm.googleapis.com",              // Chrome, Edge on Android, Samsung, Opera
	"push.services.mozilla.com",       // Firefox
	"push.apple.com",                  // Safari, iOS home-screen apps
	"notify.windows.com",              // Edge on Windows
	"android.googleapis.com",          // older Chrome
	"updates-autopush.dev.mozaws.net", // Firefox Nightly
}

// pushHostOK reports whether endpoint may be pushed to. Tests replace it.
var pushHostOK = func(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, s := range pushHosts {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

func validPushEndpoint(s string) (*url.URL, bool) {
	if s == "" || len(s) > pushMaxEndpoint {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || u.Fragment != "" {
		return nil, false
	}
	return u, pushHostOK(u)
}

// pushHTTP never follows a redirect: a push service has no reason to send
// one, and following it would let it point this server anywhere.
var pushHTTP = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func webPushEnabled() bool { return !noWebPush && appDB != nil }

var (
	vapidMu  sync.Mutex
	vapidKey *ecdsa.PrivateKey
)

// vapid returns the server's VAPID key, making and saving it the first
// time. If the saved one can't be read (secrets.key replaced), a new one is
// made and every device's endpoint is dropped: they were tied to the old
// key, and each device subscribes again when it sees the new one.
func vapid() (*ecdsa.PrivateKey, error) {
	vapidMu.Lock()
	defer vapidMu.Unlock()
	if vapidKey != nil {
		return vapidKey, nil
	}
	var sealed string
	err := appDB.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingVAPIDKey).Scan(&sealed)
	replacing := err == nil // devices subscribed with the old key
	if err == nil {
		if der, err := openSecret(settingVAPIDKey, sealed); err == nil {
			if k, err := x509.ParseECPrivateKey([]byte(der)); err == nil {
				vapidKey = k
				return k, nil
			}
		}
		log.Println("web push: the saved key can't be read; making a new one (devices subscribe again)")
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	s, err := sealSecret(settingVAPIDKey, string(der))
	if err != nil {
		return nil, err
	}
	tx, err := appDB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, settingVAPIDKey, s); err != nil {
		return nil, err
	}
	if replacing {
		if _, err := tx.Exec(`DELETE FROM push_subscriptions`); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	vapidKey = k
	return k, nil
}

// vapidPublic is the key browsers subscribe with (applicationServerKey):
// the uncompressed P-256 point, base64url.
func vapidPublic(k *ecdsa.PrivateKey) (string, error) {
	pub, err := k.PublicKey.ECDH()
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(pub.Bytes()), nil
}

// vapidAuth is the Authorization header for a push to endpoint u.
func vapidAuth(k *ecdsa.PrivateKey, u *url.URL, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	sub := "https://github.com/iotku/CalibreFast"
	if publicURL != "" {
		sub = strings.TrimRight(publicURL, "/")
	}
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": sub,
	})
	if err != nil {
		return "", err
	}
	signing := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	pub, err := vapidPublic(k)
	if err != nil {
		return "", err
	}
	return "vapid t=" + signing + "." + enc.EncodeToString(sig) + ", k=" + pub, nil
}

// The sender: deliver() marks a user; after pushDelay every marked user's
// devices get one empty push each.
var pushQ = struct {
	sync.Mutex
	users map[int64]bool
	timer *time.Timer
	wg    sync.WaitGroup // tests wait on it
}{users: map[int64]bool{}}

// pushNotify asks for a push to userID's devices, if they turned it on on
// any (checked with ex, in the caller's transaction).
func pushNotify(ex execer, userID int64) {
	if !webPushEnabled() {
		return
	}
	if q, ok := ex.(rowQuerier); ok {
		var on bool
		if q.QueryRow(`SELECT EXISTS (SELECT 1 FROM push_subscriptions WHERE user_id = ?)`, userID).Scan(&on); !on {
			return
		}
	}
	pushQ.Lock()
	defer pushQ.Unlock()
	pushQ.users[userID] = true
	if pushQ.timer == nil {
		pushQ.wg.Add(1)
		pushQ.timer = time.AfterFunc(pushDelay, flushPushes)
	}
}

func flushPushes() {
	defer pushQ.wg.Done()
	pushQ.Lock()
	users := pushQ.users
	pushQ.users = map[int64]bool{}
	pushQ.timer = nil
	pushQ.Unlock()
	for uid := range users {
		pushUser(uid)
	}
}

type pushTarget struct {
	id       int64
	endpoint string
}

// pushUser sends an empty push to each of userID's devices. The
// subscription rows are read now, so a device turned off since isn't sent.
func pushUser(userID int64) {
	if !webPushEnabled() {
		return
	}
	rows, err := appDB.Query(`SELECT id, endpoint FROM push_subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		logErr(err, "web push: subscriptions")
		return
	}
	var ts []pushTarget
	for rows.Next() {
		var t pushTarget
		if rows.Scan(&t.id, &t.endpoint) == nil {
			ts = append(ts, t)
		}
	}
	rows.Close()
	if len(ts) == 0 {
		return
	}
	k, err := vapid()
	if err != nil {
		logErr(err, "web push: key")
		return
	}
	for _, t := range ts {
		gone, err := sendPush(k, t.endpoint)
		switch {
		case gone:
			_, err = appDB.Exec(`DELETE FROM push_subscriptions WHERE id = ?`, t.id)
			logErr(err, "web push: drop endpoint")
		case err != nil:
			// Not the endpoint (which may hold the device's id at the
			// service): only the error's kind.
			log.Printf("web push: user %d: %v", userID, err)
			_, err = appDB.Exec(`UPDATE push_subscriptions SET failures = failures + 1 WHERE id = ?`, t.id)
			logErr(err, "web push: count failure")
			_, err = appDB.Exec(`DELETE FROM push_subscriptions WHERE id = ? AND failures >= ?`, t.id, pushMaxFailures)
			logErr(err, "web push: drop failing endpoint")
		default:
			_, err = appDB.Exec(`UPDATE push_subscriptions SET failures = 0 WHERE id = ? AND failures > 0`, t.id)
			logErr(err, "web push: reset failures")
		}
	}
}

var errPushRefused = errors.New("push service refused")

// sendPush posts an empty push to endpoint. gone: the service says the
// endpoint no longer exists (or was never ours), so it should be dropped.
func sendPush(k *ecdsa.PrivateKey, endpoint string) (gone bool, err error) {
	u, ok := validPushEndpoint(endpoint)
	if !ok {
		return true, nil
	}
	auth, err := vapidAuth(k, u, time.Now())
	if err != nil {
		return false, err
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), nil)
	if err != nil {
		return true, nil
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("TTL", pushTTL)
	req.Header.Set("Urgency", "normal")
	req.Header.Set("User-Agent", "CalibreFast")
	req.ContentLength = 0
	resp, err := pushHTTP.Do(req)
	if err != nil {
		return false, errors.New("push service unreachable")
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone,
		// 403: subscribed with another key (made again since).
		resp.StatusCode == http.StatusForbidden:
		return true, nil
	default:
		return false, fmt.Errorf("%w: %d", errPushRefused, resp.StatusCode)
	}
}

// GET /api/me/push: whether this server sends pushes, and the key to
// subscribe with.
func pushInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !webPushEnabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	k, err := vapid()
	if err != nil {
		http.Error(w, "push unavailable", http.StatusInternalServerError)
		return
	}
	pub, err := vapidPublic(k)
	if err != nil {
		http.Error(w, "push unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "key": pub})
}

type pushBody struct {
	Endpoint string `json:"endpoint"`
}

func readPushBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var b pushBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return "", false
	}
	if _, ok := validPushEndpoint(b.Endpoint); !ok {
		http.Error(w, "not a push service this server sends to", http.StatusBadRequest)
		return "", false
	}
	return b.Endpoint, true
}

// PUT /api/me/push {endpoint}: this device turned on. An endpoint already
// kept for someone else (another account signed in on the device) moves to
// this user.
func pushSubscribeHandler(w http.ResponseWriter, r *http.Request) {
	if !webPushEnabled() {
		http.Error(w, "system notifications are turned off on this server", http.StatusForbidden)
		return
	}
	ep, ok := readPushBody(w, r)
	if !ok {
		return
	}
	if _, err := appDB.Exec(`INSERT INTO push_subscriptions (user_id, endpoint, created_at) VALUES (?, ?, ?)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = excluded.user_id, failures = 0`,
		currentUser(r).ID, ep, nowUnix()); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/push {endpoint}: this device turned off (or signing out).
// Works with pushes off on the server too.
func pushUnsubscribeHandler(w http.ResponseWriter, r *http.Request) {
	ep, ok := readPushBody(w, r)
	if !ok {
		return
	}
	if _, err := appDB.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?`, ep, currentUser(r).ID); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
