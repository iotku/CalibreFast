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
