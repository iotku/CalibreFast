package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	if code := run(nil); code != http.StatusUnauthorized || called {
		t.Fatalf("anonymous: got %d called=%v", code, called)
	}
	if code := run(reader); code != http.StatusForbidden || called {
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
