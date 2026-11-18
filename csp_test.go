package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestSecurityHeadersTrustedTypesReportOnly(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	got := rec.Header().Get("Content-Security-Policy-Report-Only")
	if !strings.Contains(got, "require-trusted-types-for 'script'") || !strings.Contains(got, "report-uri "+cspReportPath) {
		t.Fatalf("report-only policy = %q", got)
	}
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("Trusted Types must not be enforced yet")
	}
}

func TestCSPReportLogsEachViolationOnce(t *testing.T) {
	cspSeen.Lock()
	cspSeen.keys, cspSeen.full = map[string]bool{}, false
	cspSeen.Unlock()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	post := func(body string) int {
		rec := httptest.NewRecorder()
		cspReportHandler(rec, httptest.NewRequest(http.MethodPost, cspReportPath, strings.NewReader(body)))
		return rec.Code
	}
	rep := func(sample string) string {
		return `{"csp-report":{"document-uri":"http://x/search?q=secret","effective-directive":"require-trusted-types-for",` +
			`"source-file":"http://x/static/dist/discover.js","line-number":12,"column-number":3,"script-sample":"` + sample + `"}}`
	}
	if c := post(rep("Element innerHTML|<div>one")); c != http.StatusNoContent {
		t.Fatalf("report: %d", c)
	}
	post(rep("Element innerHTML|<div>two")) // same sink and place, other data
	if n := strings.Count(buf.String(), "csp report-only"); n != 1 {
		t.Fatalf("logged %d times, want 1:\n%s", n, buf.String())
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatalf("page query logged: %s", buf.String())
	}

	if c := post(strings.Repeat("x", cspReportMaxBody+1)); c != http.StatusBadRequest {
		t.Fatalf("oversized body: %d", c)
	}
	rec := httptest.NewRecorder()
	cspReportHandler(rec, httptest.NewRequest(http.MethodGet, cspReportPath, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
}
