package main

import (
	"bytes"
	"strings"
	"testing"
)

// The readers' scripts look their controls up by id and fail loudly when one
// is missing, so both pages must render with everything web/reader-chrome.ts
// needs.
func TestReaderPages(t *testing.T) {
	shared := []string{
		`id="toc-sidebar"`, `id="toc-btn"`, `id="settings-sheet"`, `id="settings-btn"`, `id="bookmark-btn"`,
		`id="prev-btn"`, `id="next-btn"`, `id="scrubber"`, `id="progress-label"`, `id="reader-mini"`,
		`id="reader-toast"`, `id="chapter-title"`, `id="toc-list"`, `id="bookmark-list"`, `id="sync-status"`,
		`data-setting="theme"`, `id="annot-bar"`, `id="annot-note"`, `id="annot-copy"`, `id="annot-delete"`,
		`id="note-dialog"`, `id="koreader-tools"`, `id="ko-export"`, `id="ko-import"`, `id="ko-file"`, `id="ko-status"`, `id="note-text"`, `id="note-quote"`, `id="annotation-list"`,
	}
	for _, tc := range []struct {
		name   string
		search bool
		want   []string
		absent string
	}{
		{"reader.html", true, []string{`id="reader-container"`, `id="search-input"`, `id="search-results"`, `data-setting="flow"`}, ""},
		{"reader-pdf.html", false, []string{`id="pdf-container"`, `id="zoom-in"`, `id="zoom-out"`, `id="zoom-actual"`, `id="zoom-label"`, `id="loupe-btn"`,
			`data-setting="pdfDual"`, `data-setting="pdfRtl"`, `data-setting="pdfCover"`, `data-setting="pdfZoom"`}, `id="search-input"`},
	} {
		var buf bytes.Buffer
		data := ReaderPageData{Title: "Library", BookTitle: `A <b>Book</b>`, Search: tc.search}
		if err := templates.ExecuteTemplate(&buf, tc.name, data); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		html := buf.String()
		for _, want := range append(shared, tc.want...) {
			if !strings.Contains(html, want) {
				t.Errorf("%s: missing %s", tc.name, want)
			}
		}
		if tc.absent != "" && strings.Contains(html, tc.absent) {
			t.Errorf("%s: has %s", tc.name, tc.absent)
		}
		if !strings.Contains(html, `A &lt;b&gt;Book&lt;/b&gt;`) || strings.Contains(html, `A <b>Book</b>`) {
			t.Errorf("%s: the book title isn't escaped", tc.name)
		}
	}
}
