package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// htmlSinkPattern matches the places browser code turns a string into markup
// or script: what Trusted Types (csp.go) refuses once enforced. Reading
// innerHTML is fine; assigning it isn't.
var htmlSinkPattern = regexp.MustCompile(`\.(innerHTML|outerHTML)\s*=[^=]|\binnerHTML\s*:|insertAdjacentHTML|parseFromString|parseHTMLUnsafe|setHTMLUnsafe|createContextualFragment|document\.write|\.srcdoc\b`)

// htmlSinkAllowed lists the files that may use those sinks, each through a
// named Trusted Types policy. Keep it this short: build HTML from data with
// DOM APIs (el(), icon(), textContent) instead. See CLAUDE.md.
var htmlSinkAllowed = map[string]bool{
	"web/trusted.ts": true,
}

// TestNoRawHTMLSinks keeps our own browser code (web/*.ts and the inline
// scripts in templates/) off HTML-string sinks, so Trusted Types can be
// enforced. Vendored libraries (static/vendor/) are covered by a policy instead.
func TestNoRawHTMLSinks(t *testing.T) {
	var files []string
	for _, glob := range []string{"web/*.ts", "templates/*.html"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatal("no browser code found")
	}
	for _, f := range files {
		if htmlSinkAllowed[filepath.ToSlash(f)] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			code := line
			if j := strings.Index(code, "//"); j >= 0 && !strings.Contains(code[:j], `"`) && !strings.Contains(code[:j], "`") {
				code = code[:j] // a comment may name a sink
			}
			if strings.HasPrefix(strings.TrimSpace(code), "*") {
				continue // inside a /** … */ comment
			}
			if htmlSinkPattern.MatchString(code) {
				t.Errorf("%s:%d: HTML-string sink; build it with DOM APIs (web/dom.ts) instead:\n\t%s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestHTMLSinkPattern checks the pattern finds what it should, and passes reads.
func TestHTMLSinkPattern(t *testing.T) {
	for _, s := range []string{
		`box.innerHTML = html;`,
		`a.outerHTML="x"`,
		`el("div", { innerHTML: x })`,
		`a.insertAdjacentHTML("beforeend", x)`,
		`new DOMParser().parseFromString(t, "text/html")`,
		`frame.srcdoc = page`,
		`document.write(x)`,
	} {
		if !htmlSinkPattern.MatchString(s) {
			t.Errorf("not caught: %s", s)
		}
	}
	for _, s := range []string{
		`return div.innerHTML;`,
		`cleanHTML(preview.innerHTML)`,
		`if (a.innerHTML === b) {}`,
	} {
		if htmlSinkPattern.MatchString(s) {
			t.Errorf("wrongly caught: %s", s)
		}
	}
}
