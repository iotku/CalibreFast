package main

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupWebPush: an app DB and a secrets key of the test's own, a fresh
// VAPID key, and pushes on.
func setupWebPush(t *testing.T) {
	t.Helper()
	setupTestAppDB(t)
	keyPath := filepath.Join(t.TempDir(), "secrets.key")
	origPath, origNo, origDelay, origOK, origHTTP := secretKeyPath, noWebPush, pushDelay, pushHostOK, pushHTTP
	secretKeyPath = func() string { return keyPath }
	noWebPush, pushDelay = false, 10*time.Millisecond
	vapidMu.Lock()
	vapidKey = nil
	vapidMu.Unlock()
	t.Cleanup(func() {
		pushQ.wg.Wait()
		secretKeyPath, noWebPush, pushDelay, pushHostOK, pushHTTP = origPath, origNo, origDelay, origOK, origHTTP
		vapidMu.Lock()
		vapidKey = nil
		vapidMu.Unlock()
	})
}

func TestPushEndpointHosts(t *testing.T) {
	for ep, want := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":                 true,
		"https://updates.push.services.mozilla.com/wpush/v2/x":    true,
		"https://web.push.apple.com/QGx":                          true,
		"https://wns2-par02p.notify.windows.com/w/?token=x":       true,
		"https://fcm.googleapis.com:443/fcm/send/abc":             true,
		"http://fcm.googleapis.com/fcm/send/abc":                  false, // not HTTPS
		"https://fcm.googleapis.com:8443/x":                       false,
		"https://user@fcm.googleapis.com/x":                       false,
		"https://fcm.googleapis.com.evil.example/x":               false,
		"https://evilfcm.googleapis.com.example/x":                false,
		"https://notfcm.googleapis.com/x":                         false, // not a subdomain
		"https://127.0.0.1/x":                                     false,
		"https://localhost/x":                                     false,
		"/relative":                                               false,
		"":                                                        false,
		"https://fcm.googleapis.com/" + strings.Repeat("a", 1100): false,
	} {
		if _, ok := validPushEndpoint(ep); ok != want {
			t.Errorf("%.60s: %v, want %v", ep, ok, want)
		}
	}
}

func TestPushSubscribeRoutes(t *testing.T) {
	setupWebPush(t)
	alice, _ := createUser("alice", "a long enough password", false)
	bob, _ := createUser("bob", "a long enough password", false)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me/push", requireUser(pushInfoHandler))
	mux.HandleFunc("PUT /api/me/push", requireUser(pushSubscribeHandler))
	mux.HandleFunc("DELETE /api/me/push", requireUser(pushUnsubscribeHandler))
	do := func(u *User, method, body string, want int) string {
		t.Helper()
		rec := call(t, mux, u, method, "/api/me/push", body)
		if rec.Code != want {
			t.Fatalf("%s: %d %s (want %d)", method, rec.Code, rec.Body, want)
		}
		return rec.Body.String()
	}
	owner := func(ep string) int64 {
		t.Helper()
		var id int64
		appDB.QueryRow(`SELECT user_id FROM push_subscriptions WHERE endpoint = ?`, ep).Scan(&id)
		return id
	}

	do(nil, "GET", "", http.StatusUnauthorized)
	var info struct {
		Enabled bool
		Key     string
	}
	json.Unmarshal([]byte(do(alice, "GET", "", http.StatusOK)), &info)
	raw, err := base64.RawURLEncoding.DecodeString(info.Key)
	if !info.Enabled || err != nil || len(raw) != 65 || raw[0] != 4 {
		t.Fatalf("key: %+v (%d bytes)", info, len(raw))
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		t.Fatalf("key isn't a P-256 point: %v", err)
	}
	// The same key next time (kept, sealed).
	vapidMu.Lock()
	vapidKey = nil
	vapidMu.Unlock()
	json.Unmarshal([]byte(do(alice, "GET", "", http.StatusOK)), &info)
	if base64.RawURLEncoding.EncodeToString(raw) != info.Key {
		t.Fatal("the key changed on reload")
	}

	const ep = "https://fcm.googleapis.com/fcm/send/device-1"
	do(alice, "PUT", `{"endpoint":"https://example.com/collect"}`, http.StatusBadRequest)
	do(alice, "PUT", `{"endpoint":"http://fcm.googleapis.com/x"}`, http.StatusBadRequest)
	do(alice, "PUT", `not json`, http.StatusBadRequest)
	do(alice, "PUT", `{"endpoint":"`+ep+`"}`, http.StatusNoContent)
	do(alice, "PUT", `{"endpoint":"`+ep+`"}`, http.StatusNoContent) // again: one row
	if owner(ep) != alice.ID {
		t.Fatal("not kept for alice")
	}
	// Bob can't remove alice's; signing in as bob on the device moves it.
	do(bob, "DELETE", `{"endpoint":"`+ep+`"}`, http.StatusNoContent)
	if owner(ep) != alice.ID {
		t.Fatal("bob removed alice's device")
	}
	do(bob, "PUT", `{"endpoint":"`+ep+`"}`, http.StatusNoContent)
	if owner(ep) != bob.ID {
		t.Fatal("not moved to bob")
	}
	do(bob, "DELETE", `{"endpoint":"`+ep+`"}`, http.StatusNoContent)
	if owner(ep) != 0 {
		t.Fatal("not removed")
	}

	// -no-web-push: nothing can be turned on; turning off still works.
	do(alice, "PUT", `{"endpoint":"`+ep+`"}`, http.StatusNoContent)
	noWebPush = true
	if !strings.Contains(do(alice, "GET", "", http.StatusOK), `"enabled":false`) {
		t.Fatal("still enabled")
	}
	do(alice, "PUT", `{"endpoint":"https://fcm.googleapis.com/fcm/send/device-2"}`, http.StatusForbidden)
	do(alice, "DELETE", `{"endpoint":"`+ep+`"}`, http.StatusNoContent)
	if owner(ep) != 0 {
		t.Fatal("not removed with pushes off")
	}
}

// A new notification sends each of the user's devices one empty, signed
// push; an endpoint the service says is gone is dropped.
func TestPushSentOnNotification(t *testing.T) {
	setupWebPush(t)
	alice, _ := createUser("alice", "a long enough password", false)
	bob, _ := createUser("bob", "a long enough password", false) // no device

	type got struct {
		path, auth, ttl string
		body            int
	}
	var mu sync.Mutex
	var pushes []got
	status := http.StatusCreated
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		pushes = append(pushes, got{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("TTL"), len(b)})
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer srv.Close()
	host, _ := url.Parse(srv.URL)
	pushHostOK = func(u *url.URL) bool { return u.Scheme == "https" && u.Host == host.Host }
	pushHTTP = srv.Client()

	ep := srv.URL + "/push/alice-phone"
	if _, err := appDB.Exec(`INSERT INTO push_subscriptions (user_id, endpoint, created_at) VALUES (?, ?, 0)`, alice.ID, ep); err != nil {
		t.Fatal(err)
	}
	notify := func(title string) {
		t.Helper()
		tx, _ := appDB.Begin()
		id, err := addEvent(tx, notificationEvent{Kind: notifyFavoriteAuthor, BookUUID: title, Title: title, Subject: "Someone"})
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range []*User{alice, bob} {
			if err := deliver(tx, u.ID, id, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Two at once: one push.
	notify("One")
	notify("Two")
	pushQ.wg.Wait()
	mu.Lock()
	if len(pushes) != 1 {
		t.Fatalf("pushes: %+v", pushes)
	}
	p := pushes[0]
	mu.Unlock()
	if p.path != "/push/alice-phone" || p.body != 0 || p.ttl == "" {
		t.Fatalf("push: %+v", p)
	}
	checkVAPID(t, p.auth, srv.URL)

	// Gone at the service: dropped, and nothing sent next time.
	mu.Lock()
	status = http.StatusGone
	mu.Unlock()
	notify("Three")
	pushQ.wg.Wait()
	var n int
	appDB.QueryRow(`SELECT COUNT(*) FROM push_subscriptions`).Scan(&n)
	if n != 0 {
		t.Fatal("gone endpoint kept")
	}
	notify("Four")
	pushQ.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 2 {
		t.Fatalf("pushes after drop: %d", len(pushes))
	}
}

// checkVAPID verifies the Authorization header: an ES256 JWT for the
// endpoint's origin, signed by the key it names.
func checkVAPID(t *testing.T, auth, origin string) {
	t.Helper()
	rest, ok := strings.CutPrefix(auth, "vapid t=")
	jwt, k, ok2 := strings.Cut(rest, ", k=")
	if !ok || !ok2 {
		t.Fatalf("authorization: %q", auth)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt: %q", jwt)
	}
	enc := base64.RawURLEncoding
	var claims struct {
		Aud string
		Exp int64
		Sub string
	}
	cb, _ := enc.DecodeString(parts[1])
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != origin || claims.Exp <= time.Now().Unix() || claims.Exp > time.Now().Add(24*time.Hour).Unix() ||
		!(strings.HasPrefix(claims.Sub, "https:") || strings.HasPrefix(claims.Sub, "mailto:")) {
		t.Fatalf("claims: %+v", claims)
	}
	kb, _ := enc.DecodeString(k)
	if len(kb) != 65 {
		t.Fatalf("k: %d bytes", len(kb))
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(kb[1:33]), Y: new(big.Int).SetBytes(kb[33:])}
	sig, _ := enc.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature doesn't verify")
	}
}

// A saved key that can't be read (secrets.key replaced) is made again, and
// the devices subscribed with the old one are dropped; making the first key
// drops nothing.
func TestVAPIDKeyReplaced(t *testing.T) {
	setupWebPush(t)
	alice, _ := createUser("alice", "a long enough password", false)
	sub := func() {
		t.Helper()
		if _, err := appDB.Exec(`INSERT INTO push_subscriptions (user_id, endpoint, created_at) VALUES (?, 'https://fcm.googleapis.com/x', 0)`, alice.ID); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (n int) {
		appDB.QueryRow(`SELECT COUNT(*) FROM push_subscriptions`).Scan(&n)
		return n
	}
	sub()
	if _, err := vapid(); err != nil || count() != 1 {
		t.Fatalf("first key: %v, %d devices", err, count())
	}
	appDB.Exec(`UPDATE settings SET value = 'sealed:garbage' WHERE key = ?`, settingVAPIDKey)
	vapidMu.Lock()
	vapidKey = nil
	vapidMu.Unlock()
	if _, err := vapid(); err != nil || count() != 0 {
		t.Fatalf("replaced key: %v, %d devices", err, count())
	}
}
