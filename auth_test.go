package main

import (
	"encoding/json"
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
	resetKosyncCaches()
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
		"/shuffle": http.StatusUnauthorized, "/pages/shuffle/page1.json": http.StatusUnauthorized,
		"/latest": http.StatusUnauthorized, "/?page=2": http.StatusUnauthorized,
		"/api/books?uuids=x": http.StatusUnauthorized, "/pages/discover/featured.json": http.StatusUnauthorized,
		"/api/me/pins": http.StatusUnauthorized, "/pages/discover/tag-0.json": http.StatusUnauthorized,
		"/login": 200, "/register": 200, "/static/dist/style-X.css": 200, "/auth/github/start": 200, "/api/me": 200,
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
	for in, want := range map[string]string{
		"/tag/x":              "/tag/x",
		"/search?q=a%20b#top": "/search?q=a%20b#top",
		"/author/A%2FB":       "/author/A%2FB",
		"//evil.com":          "/",
		"///evil.com":         "/",
		"https://evil.com":    "/",
		"HTTPS://evil.com":    "/",
		"javascript:alert(1)": "/",
		"/\\evil.com":         "/",
		"\\\\evil.com":        "/",
		"/\t/evil.com":        "/",
		"/\n/evil.com":        "/",
		"/%2F/evil.com":       "/",
		"evil.com":            "/",
		"//user@evil.com/x":   "/",
		"":                    "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func newJar() http.CookieJar {
	j, _ := cookiejar.New(nil)
	return j
}

func TestCoverStamp(t *testing.T) {
	for in, want := range map[string]string{
		"2026-11-09T00:29:47.324232Z":      "20261109002947", // as the Go sqlite driver returns it
		"2026-11-09 00:29:47.324232+00:00": "20261109002947", // as Calibre stores it
		"2026-11-09 02:29:47+02:00":        "20261109002947",
		"2026-11-09 00:29:47":              "20261109002947",
	} {
		if got := coverStamp(in); got != want {
			t.Errorf("coverStamp(%q) = %q, want %q", in, got, want)
		}
	}
}

// The sign-in page is only a sign-in form: no site name, no links into the
// library, no nav or search, no app scripts.
func TestLoginPageRevealsNothing(t *testing.T) {
	setupTestAppDB(t)
	origTitle := SiteTitle
	SiteTitle = "Secret Stacks"
	defer func() { SiteTitle = origTitle }()

	rec := httptest.NewRecorder()
	loginHandler(rec, httptest.NewRequest("GET", "/login", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `action="/login"`) {
		t.Fatalf("no sign-in form: %d\n%s", rec.Code, body)
	}
	for _, leak := range []string{"Secret Stacks", "CalibreFast", `href="/"`, "/authors", "/search", "<script", "tb-nav", "user-nav"} {
		if strings.Contains(body, leak) {
			t.Errorf("sign-in page contains %q", leak)
		}
	}
}

// Creating an account goes on to where the visitor was headed (the reader's
// Sign in, via /account/import), as signing in does; never off the site.
func TestRegisterFollowsNext(t *testing.T) {
	setupTestAppDB(t)
	orig := allowRegistrations
	allowRegistrations = true
	t.Cleanup(func() { allowRegistrations = orig })
	for i, c := range []struct{ next, want string }{
		{"/account/import?book=b1&next=%2Fread%3Fuuid%3Db1", "/account/import?book=b1&next=%2Fread%3Fuuid%3Db1"},
		{"//evil.example/x", "/"},
		{"https://evil.example/", "/"},
	} {
		form := url.Values{"username": {"newuser" + string(rune('a'+i))}, "password": {"a long enough password"}, "password2": {"a long enough password"}, "next": {c.next}}
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		registerHandler(rec, req)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != c.want {
			t.Errorf("next %q: %d %q, want %q", c.next, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}
}

// /api/me?reading=1 carries the reading marks /api/me/reading would send
// (the library grids' first look); plain /api/me doesn't.
func TestMeWithReading(t *testing.T) {
	setupTestAppDB(t)
	u, err := createUser("reader", "a long enough password", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := setStatus(u.ID, "book-1", "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	get := func(target string) map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		meHandler(rec, asUser(httptest.NewRequest(http.MethodGet, target, nil), u))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", target, rec.Code, rec.Body)
		}
		var me map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatal(err)
		}
		return me
	}
	if _, ok := get("/api/me")["reading"]; ok {
		t.Error("plain /api/me sent the marks")
	}
	var marks readingMarks
	if err := json.Unmarshal(get("/api/me?reading=1")["reading"], &marks); err != nil {
		t.Fatal(err)
	}
	if marks.User != "reader" || len(marks.Wanted) != 1 || marks.Wanted[0] != "book-1" {
		t.Errorf("marks = %+v", marks)
	}
	rec := httptest.NewRecorder()
	readingHandler(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/me/reading", nil), u))
	var alone readingMarks
	_ = json.Unmarshal(rec.Body.Bytes(), &alone)
	if a, b := mustJSON(t, marks), mustJSON(t, alone); a != b {
		t.Errorf("?reading=1 marks %s differ from /api/me/reading's %s", a, b)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
