package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Colors are written out only in web/css/tokens.css; every other stylesheet
// uses its semantic tokens (var(--bg-surface), var(--accent), …), so a theme
// is one more block of values there. The reader keeps its own page themes
// (reader.css), and a book's placeholder cover is a hue from the server
// (hsl(var(--cover-hue) …)).
func TestNoRawColorsOutsideTokens(t *testing.T) {
	files, _ := filepath.Glob("web/*.css")
	more, _ := filepath.Glob("web/css/*.css")
	files = append(files, more...)
	more, _ = filepath.Glob("web/css/*/*.css")
	files = append(files, more...)

	allowed := map[string]bool{"web/css/tokens.css": true, "web/reader.css": true}
	raw := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\((?:[^v]|v[^a])|:\s*(?:white|black)\s*;`)
	comment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	for _, f := range files {
		if allowed[filepath.ToSlash(f)] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := comment.ReplaceAllStringFunc(string(b), func(c string) string {
			return strings.Repeat("\n", strings.Count(c, "\n"))
		})
		for i, line := range strings.Split(src, "\n") {
			if raw.MatchString(line) {
				t.Errorf("%s:%d: a color outside web/css/tokens.css (use a token): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Pages take their styles from web/*.css (built into static/dist/), not
// <style> blocks: one place to look, cached, and themed by the tokens.
func TestNoStyleBlocksInTemplates(t *testing.T) {
	files, _ := filepath.Glob("templates/*.html")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "<style") {
			t.Errorf("%s has a <style> block; put it in web/ (e.g. web/css/pages/)", f)
		}
	}
}
