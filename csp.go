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

// Trusted Types, in report-only mode for now.
//
// require-trusted-types-for 'script' makes the browser refuse a plain string
// at every DOM sink that turns text into markup or code (innerHTML,
// outerHTML, insertAdjacentHTML, iframe srcdoc, document.write, script src,
// new Worker(url), eval, …). That stops the whole class of client-side XSS
// where data from a book, its metadata or a user ends up parsed as HTML.
//
// It's sent as Content-Security-Policy-Report-Only, so nothing is blocked
// yet: each place that would be is reported to /api/csp-report and logged
// once. When that log stays empty (our own code moved off string HTML, and
// the vendored libraries given a policy), the header becomes the enforcing
// Content-Security-Policy. See docs/SECURITY.md.
const cspReportPath = "/api/csp-report"

const trustedTypesReportOnly = "require-trusted-types-for 'script'; report-uri " + cspReportPath

// securityHeaders adds the report-only Trusted Types policy to every
// response. Only documents act on it; other responses ignore it.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy-Report-Only", trustedTypesReportOnly)
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
		log.Printf("csp report-only: %s %q at %s:%d:%d on %s",
			directive, truncate(v.Sample, 120), v.SourceFile, v.Line, v.Column, pathOnly(v.DocumentURI))
		if cspSeen.full {
			log.Printf("csp report-only: %d distinct reports logged; not logging more until restart", cspReportMaxSeen)
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
