package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeProvider is a GitHub-like OAuth server that checks PKCE.
func fakeProvider(t *testing.T, userID int64, login string) (*httptest.Server, *oauthProvider) {
	t.Helper()
	challenges := map[string]string{} // code -> challenge
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
			http.Error(w, "bad authorize request", 400)
			return
		}
		challenges["code-123"] = q.Get("code_challenge")
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=code-123&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "secret" || pkceChallenge(r.Form.Get("code_verifier")) != challenges[r.Form.Get("code")] {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": userID, "login": login, "email": ""})
	})
	mux.HandleFunc("/emails", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"email": login + "@example.com", "primary": true, "verified": true}})
	})
	srv := httptest.NewServer(mux)
	p := githubProvider("client", "secret")
	p.AuthURL, p.TokenURL, p.UserURL, p.EmailsURL = srv.URL+"/authorize", srv.URL+"/token", srv.URL+"/user", srv.URL+"/emails"
	return srv, p
}

// oauthApp runs CalibreFast's auth routes; returns its server and a client
// that keeps cookies but doesn't follow redirects.
func oauthApp(t *testing.T, p *oauthProvider) (*httptest.Server, *http.Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/{provider}/start", oauthStart)
	mux.HandleFunc("GET /auth/{provider}/callback", oauthCallback)
	mux.HandleFunc("GET /api/me", meHandler)
	app := httptest.NewServer(withAuth(mux))
	origURL, origProviders := publicURL, oauthProviders
	publicURL = app.URL
	oauthProviders = map[string]*oauthProvider{"github": p}
	t.Cleanup(func() { app.Close(); publicURL, oauthProviders = origURL, origProviders })
	jar := newJar()
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return app, client
}

// signIn walks start → provider → callback and returns the final redirect.
func signIn(t *testing.T, app *httptest.Server, client *http.Client, startQuery string) string {
	t.Helper()
	resp, err := client.Get(app.URL + "/auth/github/start" + startQuery)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = client.Get(resp.Header.Get("Location")) // provider authorize
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = client.Get(resp.Header.Get("Location")) // our callback
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Header.Get("Location")
}

func signedInAs(t *testing.T, app *httptest.Server, client *http.Client) string {
	resp, err := client.Get(app.URL + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ User *User }
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.User == nil {
		return ""
	}
	return body.User.Username
}

func TestOAuthClosedRegistrationRefusesUnknown(t *testing.T) {
	setupTestAppDB(t)
	allowRegistrations = false
	srv, p := fakeProvider(t, 42, "octo")
	defer srv.Close()
	app, client := oauthApp(t, p)
	if loc := signIn(t, app, client, "?next=/tag/x"); !strings.HasPrefix(loc, "/login?error=") {
		t.Fatalf("expected refusal, got %q", loc)
	}
	if who := signedInAs(t, app, client); who != "" {
		t.Fatalf("signed in as %q", who)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 0 {
		t.Fatalf("user created with closed registration")
	}
}

func TestOAuthOpenRegistrationCreatesAndReuses(t *testing.T) {
	setupTestAppDB(t)
	allowRegistrations = true
	defer func() { allowRegistrations = false }()
	srv, p := fakeProvider(t, 42, "octo")
	defer srv.Close()
	app, client := oauthApp(t, p)
	if loc := signIn(t, app, client, "?next=/tag/x"); loc != "/tag/x" {
		t.Fatalf("redirect: %q", loc)
	}
	if who := signedInAs(t, app, client); who != "octo" {
		t.Fatalf("signed in as %q", who)
	}
	// Second sign-in maps to the same account (by provider id).
	app2Client := &http.Client{Jar: newJar(), CheckRedirect: client.CheckRedirect}
	signIn(t, app, app2Client, "")
	var users int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	if users != 1 {
		t.Fatalf("expected 1 user, got %d", users)
	}
	var email string
	_ = appDB.QueryRow(`SELECT email FROM identities`).Scan(&email)
	if email != "octo@example.com" {
		t.Errorf("primary verified email not fetched: %q", email)
	}
}

func TestOAuthLinkFromAccount(t *testing.T) {
	setupTestAppDB(t)
	allowRegistrations = false
	u, _ := createUser("carol", "longenough-pw", false)
	srv, p := fakeProvider(t, 7, "carol-gh")
	defer srv.Close()
	app, client := oauthApp(t, p)

	// Sign in with password to get a session cookie into the jar.
	appURL, _ := url.Parse(app.URL)
	rec := httptest.NewRecorder()
	if err := startSession(rec, httptest.NewRequest("GET", "/", nil), u.ID); err != nil {
		t.Fatal(err)
	}
	client.Jar.SetCookies(appURL, rec.Result().Cookies())

	if loc := signIn(t, app, client, "?link=1"); !strings.HasPrefix(loc, "/account?notice=") {
		t.Fatalf("link redirect: %q", loc)
	}
	// Now a fresh browser can sign in with GitHub as carol.
	fresh := &http.Client{Jar: newJar(), CheckRedirect: client.CheckRedirect}
	signIn(t, app, fresh, "")
	if who := signedInAs(t, app, fresh); who != "carol" {
		t.Fatalf("after linking, signed in as %q", who)
	}
}

func TestOAuthStateMustMatch(t *testing.T) {
	setupTestAppDB(t)
	allowRegistrations = true
	defer func() { allowRegistrations = false }()
	srv, p := fakeProvider(t, 9, "mallory")
	defer srv.Close()
	app, client := oauthApp(t, p)

	// Attacker starts a flow, then feeds the callback URL to a victim browser
	// (no state cookie): must be refused.
	resp, _ := client.Get(app.URL + "/auth/github/start")
	resp.Body.Close()
	resp, _ = client.Get(resp.Header.Get("Location"))
	resp.Body.Close()
	callback := resp.Header.Get("Location")
	victim := &http.Client{Jar: newJar(), CheckRedirect: client.CheckRedirect}
	resp, _ = victim.Get(callback)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?error=") {
		t.Fatalf("callback without state cookie accepted: %q", loc)
	}
	// The rightful browser's first use works…
	appURL, _ := url.Parse(app.URL)
	stateCookies := client.Jar.Cookies(&url.URL{Scheme: "http", Host: appURL.Host, Path: "/auth/"})
	resp, _ = client.Get(callback)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("legitimate callback refused: %q", loc)
	}
	// …but the state is single-use: replaying it, even with the cookie, fails.
	replay := &http.Client{Jar: newJar(), CheckRedirect: client.CheckRedirect}
	replay.Jar.SetCookies(appURL, stateCookies)
	resp, _ = replay.Get(callback)
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login?error=") {
		t.Fatalf("replayed state accepted: %q", loc)
	}
}
