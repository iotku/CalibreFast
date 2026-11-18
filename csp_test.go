package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSecurityHeadersTrustedTypes(t *testing.T) {
	defer func(v bool) { trustedTypesReportOnly = v }(trustedTypesReportOnly)
	get := func() http.Header {
		h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Header()
	}

	trustedTypesReportOnly = false
	hd := get()
	got := hd.Get("Content-Security-Policy")
	for _, want := range []string{"require-trusted-types-for 'script'", "trusted-types cf-server-html ", "'allow-duplicates'", "report-uri " + cspReportPath} {
		if !strings.Contains(got, want) {
			t.Fatalf("enforced policy %q lacks %q", got, want)
		}
	}
	if hd.Get("Content-Security-Policy-Report-Only") != "" {
		t.Fatal("report-only header sent while enforcing")
	}

	trustedTypesReportOnly = true
	hd = get()
	if hd.Get("Content-Security-Policy") != "" || hd.Get("Content-Security-Policy-Report-Only") != trustedTypesPolicy {
		t.Fatalf("-trusted-types-report-only: %v", hd)
	}
}

// Every policy name the browser code makes is in the header's list, and
// the list has nothing the code doesn't make: a name missing from the list
// fails in the browser, and a spare one is an opening nobody needs.
func TestTrustedTypesPolicyNames(t *testing.T) {
	files, err := filepath.Glob("web/*.ts")
	if err != nil {
		t.Fatal(err)
	}
	made := map[string]bool{}
	re := regexp.MustCompile(`(?:makePolicy|createPolicy)\(\s*"([^"]+)"`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			made[m[1]] = true
		}
		if f != "web/trusted.ts" && regexp.MustCompile(`trustedTypes\??\.createPolicy|makePolicy\(`).Match(b) {
			t.Errorf("%s makes a Trusted Types policy: they all live in web/trusted.ts", f)
		}
	}
	listed := map[string]bool{}
	for _, n := range trustedTypesPolicyNames {
		listed[n] = true
		if !made[n] {
			t.Errorf("csp.go lists %q, which web/ never makes", n)
		}
	}
	for n := range made {
		if !listed[n] {
			t.Errorf("web/ makes policy %q, missing from trustedTypesPolicyNames (csp.go)", n)
		}
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
	if n := strings.Count(buf.String(), "csp refused"); n != 1 {
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
