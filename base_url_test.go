package main

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// Behind an HTTPS reverse proxy, links and the device sync address use https.
func TestBaseURLScheme(t *testing.T) {
	orig := publicURL
	t.Cleanup(func() { publicURL = orig })
	publicURL = ""
	for _, tt := range []struct {
		name   string
		header map[string]string
		tls    bool
		want   string
	}{
		{"plain", nil, false, "http://books.lan"},
		{"direct TLS", nil, true, "https://books.lan"},
		{"X-Forwarded-Proto", map[string]string{"X-Forwarded-Proto": "https"}, false, "https://books.lan"},
		{"X-Forwarded-Proto chain", map[string]string{"X-Forwarded-Proto": "HTTPS, http"}, false, "https://books.lan"},
		{"Forwarded", map[string]string{"Forwarded": `for=10.0.0.1;proto="https";host=books.lan`}, false, "https://books.lan"},
		{"forwarded http", map[string]string{"X-Forwarded-Proto": "http"}, false, "http://books.lan"},
	} {
		r := httptest.NewRequest("GET", "http://books.lan/account", nil)
		for k, v := range tt.header {
			r.Header.Set(k, v)
		}
		if tt.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := baseURL(r); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
		if got := kosyncServerURL(r); got != tt.want+kosyncPrefix {
			t.Errorf("%s: sync %q", tt.name, got)
		}
	}
	publicURL = "https://books.example.com/"
	if got := kosyncServerURL(httptest.NewRequest("GET", "http://books.lan/", nil)); got != "https://books.example.com"+kosyncPrefix {
		t.Errorf("-public-url: %q", got)
	}
}
