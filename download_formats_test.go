package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Every format Calibre stores is listed and downloadable, not only those
// CalibreFast reads or accepts as uploads; a book's .html format downloads
// as bytes, never as a page from our origin.
func TestDownloadAnyCalibreFormat(t *testing.T) {
	_, books := setupLibraryAndUser(t, 1)
	uuid := books[0]
	path, _ := resolveBookPath(uuid)
	dir := filepath.Join(baseDir, path)
	_ = os.MkdirAll(dir, 0o755)
	for _, name := range []string{"Book.chm", "Book.html", "cover.jpg", "metadata.opf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	formatCache.Delete(uuid)
	t.Cleanup(func() { formatCache.Delete(uuid) })

	formats := extractFormats(resolveFormats(uuid, baseDir, path))
	if !slices.Contains(formats, "chm") || !slices.Contains(formats, "html") || slices.Contains(formats, "jpg") || slices.Contains(formats, "opf") {
		t.Fatalf("formats %v", formats)
	}
	for _, f := range []string{"chm", "html"} {
		rec := httptest.NewRecorder()
		downloadHandler(rec, httptest.NewRequest("GET", "/download/"+uuid+"/"+f, nil))
		if rec.Code != 200 || rec.Body.String() != "Book."+f {
			t.Fatalf("%s: %d %q", f, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: Content-Type %q", f, ct)
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename=Book.`+f {
			t.Errorf("%s: Content-Disposition %q", f, cd)
		}
	}
}
