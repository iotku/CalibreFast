package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func setupTestAppDB(t *testing.T) {
	t.Helper()
	db, err := openAppDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig := appDB
	appDB = db
	t.Cleanup(func() { db.Close(); appDB = orig })
}

func TestPasswordAuth(t *testing.T) {
	setupTestAppDB(t)
	if _, err := createUser("al", "longenough-pw", false); err != errBadUsername {
		t.Errorf("short username: got %v", err)
	}
	if _, err := createUser("alice", "short", false); err != errWeakPassword {
		t.Errorf("weak password: got %v", err)
	}
	u, err := createUser("alice", "longenough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createUser("ALICE", "longenough-pw", false); err != errUsernameTaken {
		t.Errorf("case-insensitive duplicate: got %v", err)
	}
	if got, err := authenticate("Alice", "longenough-pw"); err != nil || got.ID != u.ID {
		t.Errorf("valid login: %v %v", got, err)
	}
	for _, c := range [][2]string{{"alice", "wrong-password"}, {"nobody", "longenough-pw"}} {
		if _, err := authenticate(c[0], c[1]); err != errBadCredentials {
			t.Errorf("%v: got %v", c, err)
		}
	}
	// Disabled accounts can't sign in.
	_, _ = appDB.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, u.ID)
	if _, err := authenticate("alice", "longenough-pw"); err != errBadCredentials {
		t.Errorf("disabled: got %v", err)
	}
}

func TestSessionsAndLoginFlow(t *testing.T) {
	setupTestAppDB(t)
	if _, err := createUser("bob", "longenough-pw", false); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("POST /logout", logoutHandler)
	mux.HandleFunc("GET /api/me", meHandler)
	srv := httptest.NewServer(withAuth(mux))
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	login := func(pw, next string) *http.Response {
		resp, err := client.PostForm(srv.URL+"/login", url.Values{"username": {"bob"}, "password": {pw}, "next": {next}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := login("nope-nope-nope", "/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", resp.StatusCode)
	}
	resp := login("longenough-pw", "//evil.example/x")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d → %q (open redirect must be refused)", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie missing or not HttpOnly/Lax: %+v", cookie)
	}
	// Only a hash of the token is stored.
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, cookie.Value).Scan(&n)
	if n != 0 {
		t.Fatal("raw session token stored in the database")
	}

	me := func() int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/me", nil)
		req.AddCookie(cookie)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if me() != http.StatusOK {
		t.Fatal("session not recognised")
	}
	req, _ := http.NewRequest("POST", srv.URL+"/logout", nil)
	req.AddCookie(cookie)
	if r, err := client.Do(req); err == nil {
		r.Body.Close()
	}
	if me() != http.StatusUnauthorized {
		t.Fatal("session still valid after logout")
	}
}

func TestLibraryPrivateByDefault(t *testing.T) {
	setupTestAppDB(t)
	orig := publicCatalog
	defer func() { publicCatalog = orig }()

	h := withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	check := func(cases map[string]int) {
		t.Helper()
		for p, want := range cases {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
			if rec.Code != want {
				t.Errorf("public=%v %s: got %d, want %d", publicCatalog, p, rec.Code, want)
			}
		}
	}

	publicCatalog = false // the default
	check(map[string]int{
		"/": http.StatusUnauthorized, "/pages/page1.json": http.StatusUnauthorized,
		"/cover-thumb/some-uuid": http.StatusUnauthorized, "/book/get/some-uuid": http.StatusUnauthorized,
		"/api/tags": http.StatusUnauthorized, "/shelves": http.StatusUnauthorized,
		"/login": 200, "/register": 200, "/static/style.css": 200, "/auth/github/start": 200, "/api/me": 200,
	})
	// Browsers are sent to the sign-in page instead.
	req := httptest.NewRequest("GET", "/tag/x?page=2", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Errorf("html redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	publicCatalog = true // -public
	check(map[string]int{"/": 200, "/cover-thumb/some-uuid": 200, "/shelves": 200})
}

func TestLoginRateLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &limiter{hits: map[string][]time.Time{}, max: 3, window: time.Minute, nowFunc: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if !l.allow("k") {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.allow("k") {
		t.Fatal("4th attempt allowed")
	}
	now = now.Add(61 * time.Second)
	if !l.allow("k") {
		t.Fatal("not allowed after the window")
	}
}

func TestLastAdminProtected(t *testing.T) {
	setupTestAppDB(t)
	a, _ := createUser("onlyadmin", "longenough-pw", true)
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/admin/users/{id}", adminUpdateUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", adminDeleteUser)
	do := func(method, body string) int {
		req := httptest.NewRequest(method, "/api/admin/users/"+itoa(a.ID), strings.NewReader(body))
		req = req.WithContext(withUserCtx(req, a))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("PATCH", `{"is_admin":false}`); c != http.StatusConflict {
		t.Errorf("demote last admin: %d", c)
	}
	if c := do("DELETE", ""); c != http.StatusConflict {
		t.Errorf("delete last admin: %d", c)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"/tag/x": "/tag/x", "//evil.com": "/", "https://evil.com": "/", "/\\evil.com": "/", "": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func newJar() http.CookieJar {
	j, _ := cookiejar.New(nil)
	return j
}
