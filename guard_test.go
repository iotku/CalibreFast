package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireWrites(t *testing.T) {
	orig := allowWrites
	defer func() { allowWrites = orig }()

	called := false
	h := requireWrites(func(w http.ResponseWriter, r *http.Request) { called = true })

	allowWrites = false
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/metadata/x/apply", nil))
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("read-only: got %d, handler called=%v", rec.Code, called)
	}

	allowWrites = true
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/metadata/x/apply", nil))
	if !called {
		t.Fatal("with -allow-writes the handler should run")
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
