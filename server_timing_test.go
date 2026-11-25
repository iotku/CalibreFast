package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// Every response says how long the server took, whatever the handler wrote.
func TestServerTiming(t *testing.T) {
	re := regexp.MustCompile(`^auth;dur=\d+\.\d\d, app;dur=\d+\.\d\d$`)
	for name, h := range map[string]http.HandlerFunc{
		"body":    func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hi")) },
		"status":  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"nothing": func(w http.ResponseWriter, r *http.Request) {},
		"error":   func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) },
	} {
		rec := httptest.NewRecorder()
		serverTiming(h).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if got := rec.Header().Values("Server-Timing"); len(got) != 1 || !re.MatchString(got[0]) {
			t.Errorf("%s: Server-Timing %q", name, got)
		}
	}
}
