package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireWrites(t *testing.T) {
	setupTestAppDB(t)
	orig := allowWrites
	defer func() { allowWrites = orig }()

	admin, _ := createUser("admin1", "correct horse battery", true)
	reader, _ := createUser("reader1", "correct horse battery", false)

	called := false
	h := requireWrites(func(w http.ResponseWriter, r *http.Request) { called = true })
	run := func(u *User) int {
		called = false
		req := httptest.NewRequest(http.MethodPost, "/metadata/x/apply", nil)
		if u != nil {
			req = req.WithContext(context.WithValue(req.Context(), userKey, u))
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	allowWrites = false
	if code := run(admin); code != http.StatusForbidden || called {
		t.Fatalf("read-only server, admin: got %d called=%v", code, called)
	}

	allowWrites = true
	if code := run(nil); code != http.StatusNotFound || called {
		t.Fatalf("anonymous: got %d called=%v", code, called)
	}
	if code := run(reader); code != http.StatusNotFound || called {
		t.Fatalf("non-admin: got %d called=%v", code, called)
	}
	if run(admin); !called {
		t.Fatal("admin with -allow-writes should be allowed")
	}
}

func TestCSRFProtect(t *testing.T) {
	h := csrfProtect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cases := []struct {
		name, method string
		headers      map[string]string
		want         int
	}{
		{"cross-site POST (modern browser)", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site but different origin POST", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-origin POST (Origin only, older browser)", "POST", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"cross-site PUT", "PUT", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-origin POST (the editor)", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://library.local"}, http.StatusOK},
		{"user typed / bookmark", "POST", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"non-browser client (curl)", "POST", nil, http.StatusOK},
		{"cross-site GET is a read", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://library.local/metadata/x/apply", nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestRequireReader(t *testing.T) {
	setupTestAppDB(t)
	orig := publicRead
	defer func() { publicRead = orig }()
	u, _ := createUser("someone", "longenough-pw", false)
	h := requireReader(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	run := func(who *User, html bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/download/x/epub", nil)
		if html {
			req.Header.Set("Accept", "text/html")
		}
		if who != nil {
			req = req.WithContext(context.WithValue(req.Context(), userKey, who))
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}
	publicRead = false
	if c := run(nil, false).Code; c != http.StatusUnauthorized {
		t.Errorf("anonymous download: %d", c)
	}
	if rec := run(nil, true); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Errorf("anonymous browser should be sent to sign in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := run(u, false).Code; c != http.StatusOK {
		t.Errorf("signed in: %d", c)
	}
	publicRead = true
	if c := run(nil, false).Code; c != http.StatusOK {
		t.Errorf("public read-only mode, anonymous: %d", c)
	}
}

// The routes merging, bulk editing and repairs add are all behind
// requireWrites: on a server without -allow-writes even an admin is refused,
// before anything is read or written.
func TestBulkEditAndMergeRoutesNeedWrites(t *testing.T) {
	setupTestAppDB(t)
	orig := allowWrites
	defer func() { allowWrites = orig }()
	mux := http.NewServeMux()
	withMux(mux, func() {
		registerMergeRoutes()
		registerAddFormatRoutes()
		registerRecycleRoutes()
		registerBulkEditRoutes()
		registerNotDuplicateRoutes()
		registerLibraryRepairRoutes()
	})
	admin, _ := createUser("admin1", "correct horse battery", true)

	routes := []struct{ method, path string }{
		{"GET", "/library/edit"},
		{"GET", "/api/library/table"},
		{"POST", "/api/library/bulk"},
		{"POST", "/api/library/repair-formats"},
		{"GET", "/admin/repairs"},
		{"GET", "/api/admin/repairs"},
		{"POST", "/api/admin/repairs/format-names"},
		{"POST", "/api/library/not-duplicates"},
		{"DELETE", "/api/library/not-duplicates/1"},
		{"GET", "/book/merge?ids=a,b"},
		{"GET", "/merge/metadata?ids=a,b"},
		{"POST", "/merge/apply"},
		{"POST", "/book/add-format/a"},
		{"GET", "/book/add-format/a/b"},
		{"GET", "/book/add-format/a/b/metadata"},
		{"POST", "/book/add-format/a/b/apply"},
		{"POST", "/api/library/books/a/formats/epub/remove"},
	}
	// (That an admin gets through with -allow-writes is TestRequireWrites.)
	allowWrites = false
	for _, rt := range routes {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}"))
		req = req.WithContext(context.WithValue(req.Context(), userKey, admin))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "-allow-writes") {
			t.Errorf("%s %s without -allow-writes: %d %q", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

// withMux runs register with http.HandleFunc registering on mux, so a test
// can serve the real routes without touching the default mux.
func withMux(mux *http.ServeMux, register func()) {
	orig := http.DefaultServeMux
	http.DefaultServeMux = mux
	defer func() { http.DefaultServeMux = orig }()
	register()
}

// To anyone but an admin, the admin and editing routes answer exactly like a
// path that doesn't exist: same status, same body, same headers, whatever the
// method or Accept header, and whether or not the server allows writes.
func TestAdminRoutesLookMissing(t *testing.T) {
	setupTestAppDB(t)
	orig := allowWrites
	defer func() { allowWrites = orig }()
	mux := http.NewServeMux()
	withMux(mux, func() {
		http.HandleFunc("/", indexHandler)
		registerAuthRoutes()
		registerLibraryRepairRoutes()
		registerBulkEditRoutes()
		registerImportRoutes()
		registerRecycleRoutes()
		registerMergeRoutes()
		registerAddFormatRoutes()
	})
	reader, _ := createUser("reader1", "correct horse battery", false)

	type resp struct {
		code   int
		body   string
		header http.Header
	}
	do := func(u *User, method, path string, html bool) resp {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		if html {
			req.Header.Set("Accept", "text/html")
		}
		if u != nil {
			req = req.WithContext(context.WithValue(req.Context(), userKey, u))
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return resp{rec.Code, rec.Body.String(), rec.Header()}
	}

	routes := []struct{ method, path string }{
		{"GET", "/admin/users"},
		{"POST", "/admin/users"}, // wrong method: no 405 giving it away
		{"POST", "/api/admin/users"},
		{"DELETE", "/api/admin/users/1"},
		{"GET", "/api/admin/links"},
		{"GET", "/admin/repairs"},
		{"GET", "/api/admin/repairs"},
		{"GET", "/library/edit"},
		{"GET", "/import"},
		{"GET", "/recycle-bin"},
		{"POST", "/merge/apply"},
	}
	for _, writes := range []bool{false, true} {
		allowWrites = writes
		for _, u := range []*User{nil, reader} {
			for _, html := range []bool{false, true} {
				for _, rt := range routes {
					want := do(u, rt.method, "/no-such-page", html)
					if want.code != http.StatusNotFound {
						t.Fatalf("unknown path: %d", want.code)
					}
					got := do(u, rt.method, rt.path, html)
					if got.code != want.code || got.body != want.body || len(got.header) != len(want.header) {
						t.Errorf("writes=%v user=%v html=%v %s %s: %d %q %v, want %d %q %v", writes, u != nil, html,
							rt.method, rt.path, got.code, got.body, got.header, want.code, want.body, want.header)
					}
				}
			}
		}
	}
}
