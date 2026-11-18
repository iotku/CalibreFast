package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Trusted Types.
//
// require-trusted-types-for 'script' makes the browser refuse a plain string
// at every DOM sink that turns text into markup or code (innerHTML,
// outerHTML, insertAdjacentHTML, iframe srcdoc, DOMParser, document.write,
// script src, new Worker(url), service worker registration, eval,
// Function, …). That stops the whole class of client-side XSS where data
// from a book, its metadata or a user ends up parsed as HTML or run as code,
// even where an escape was forgotten.
//
// Only values made by a named policy pass, and the trusted-types directive
// lists the only names allowed (web/trusted.ts makes them; each is for one
// narrow job). 'allow-duplicates' because bundles don't share modules: two
// scripts on one page each make their own "cf-server-html".
//
// Violations are refused and reported to /api/csp-report, which logs each
// distinct one once. -trusted-types-report-only sends the policy as
// Content-Security-Policy-Report-Only instead: reported but not refused, for
// when something breaks and a fix isn't out yet. See docs/SECURITY.md.
const cspReportPath = "/api/csp-report"

// trustedTypesPolicyNames: every name web/trusted.ts passes to createPolicy
// (TestTrustedTypesPolicyNames checks they match).
var trustedTypesPolicyNames = []string{"cf-server-html", "cf-inert-parse", "cf-dompurify", "cf-script-url", "cf-htmx", "default"}

var trustedTypesPolicy = "require-trusted-types-for 'script'; trusted-types " +
	strings.Join(trustedTypesPolicyNames, " ") + " 'allow-duplicates'; report-uri " + cspReportPath

// trustedTypesReportOnly: -trusted-types-report-only.
var trustedTypesReportOnly bool

// securityHeaders adds the Trusted Types policy to every response. Documents
// and workers act on it; other responses ignore it.
func securityHeaders(h http.Handler) http.Handler {
	header := "Content-Security-Policy"
	if trustedTypesReportOnly {
		header = "Content-Security-Policy-Report-Only"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(header, trustedTypesPolicy)
		h.ServeHTTP(w, r)
	})
}

const (
	cspReportMaxBody = 16 << 10 // a report is well under 2 KB
	cspReportMaxSeen = 500      // distinct violations logged before going quiet
)

var cspSeen = struct {
	sync.Mutex
	keys map[string]bool
	full bool
}{keys: map[string]bool{}}

// cspReport is the report-uri body: {"csp-report": {...}}.
type cspReport struct {
	Report struct {
		DocumentURI string `json:"document-uri"`
		Directive   string `json:"effective-directive"`
		Violated    string `json:"violated-directive"`
		SourceFile  string `json:"source-file"`
		Line        int    `json:"line-number"`
		Column      int    `json:"column-number"`
		Sample      string `json:"script-sample"`
		Disposition string `json:"disposition"` // "enforce" (refused) or "report"
	} `json:"csp-report"`
}

// cspReportHandler logs each distinct violation once (by sink, source file
// and position), so a page reloaded a hundred times is one line. Signed-in
// users only, and size- and count-capped, so it can't be used to fill the log.
func cspReportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, cspReportMaxBody+1))
	if err != nil || len(body) > cspReportMaxBody {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	var rep cspReport
	if json.Unmarshal(body, &rep) != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	v := rep.Report
	directive := v.Directive
	if directive == "" {
		directive = v.Violated
	}
	// The sample starts with the sink ("Element innerHTML|<div…"): keep that
	// part in the key, not the data, which differs on every call.
	sink, data, _ := strings.Cut(v.Sample, "|")
	place := v.SourceFile + ":" + strconv.Itoa(v.Line) + ":" + strconv.Itoa(v.Column)
	if pathOnly(v.SourceFile) == pathOnly(v.DocumentURI) {
		// An inline script: the same one sits on a different line of
		// every page, so it's known by what it writes instead.
		place = "inline " + truncate(data, 40)
	}
	key := directive + " " + sink + " " + place

	cspSeen.Lock()
	seen := cspSeen.keys[key]
	full := cspSeen.full
	if !seen && !full {
		cspSeen.keys[key] = true
		if len(cspSeen.keys) >= cspReportMaxSeen {
			cspSeen.full = true
		}
	}
	cspSeen.Unlock()

	if !seen && !full {
		what := "refused"
		if v.Disposition == "report" {
			what = "report-only"
		}
		log.Printf("csp %s: %s %q at %s:%d:%d on %s",
			what, directive, truncate(v.Sample, 120), v.SourceFile, v.Line, v.Column, pathOnly(v.DocumentURI))
		if cspSeen.full {
			log.Printf("csp: %d distinct reports logged; not logging more until restart", cspReportMaxSeen)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// pathOnly: a page's path without its query (which can hold search terms).
func pathOnly(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if j := strings.IndexByte(u, '/'); j >= 0 {
			u = u[j:]
		} else {
			u = "/"
		}
	}
	p, _, _ := strings.Cut(u, "?")
	return p
}
