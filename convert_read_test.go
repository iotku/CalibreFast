package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rangeCall(t *testing.T, mux http.Handler, u *User, path, rng string) *httptest.ResponseRecorder {
	t.Helper()
	req := asUser(httptest.NewRequest("GET", path, nil), u)
	req.Header.Set("Range", rng)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// A Kindle book's file is served converted to EPUB, from the cache once
// made, with Range support; nothing is written to the library.
func TestAsEPUB(t *testing.T) {
	u, books := setupLibraryAndUser(t, 2)
	origCache := cacheDir
	cacheDir = t.TempDir()
	t.Cleanup(func() { cacheDir = origCache })

	src, err := os.ReadFile("pkg/mobi/testdata/tiny.azw3")
	if err != nil {
		t.Fatal(err)
	}
	put := func(uuid, name string, data []byte) string {
		path, _ := resolveBookPath(uuid)
		dir := filepath.Join(baseDir, path)
		_ = os.MkdirAll(dir, 0o755)
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, data, 0o644); err != nil {
			t.Fatal(err)
		}
		formatCache.Delete(uuid)
		t.Cleanup(func() { formatCache.Delete(uuid) })
		return dir
	}
	dir := put(books[0], "Book.azw3", src)
	// The second book's file is DRM-protected (its MOBI header says encrypted).
	drm := bytes.Clone(src)
	rec0 := int(binary.BigEndian.Uint32(drm[78:]))
	binary.BigEndian.PutUint16(drm[rec0+12:], 2)
	put(books[1], "Book.azw3", drm)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /book/as-epub/{uuid}/{format}", requireReader(asEPUBHandler))

	rec := call(t, mux, u, "GET", "/book/as-epub/"+books[0]+"/azw3", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/epub+zip" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("GET: %d %v", rec.Code, rec.Header())
	}
	body := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || zr.File[0].Name != "mimetype" {
		t.Fatalf("not an EPUB: %v", err)
	}
	if rec.Header().Get("Last-Modified") == "" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("caching headers: %v", rec.Header())
	}
	// Ranges, as the reader reads it.
	rr := rangeCall(t, mux, u, "/book/as-epub/"+books[0]+"/azw3", "bytes=0-9")
	if rr.Code != http.StatusPartialContent || rr.Body.String() != string(body[:10]) {
		t.Errorf("Range: %d %q", rr.Code, rr.Body.String())
	}
	// Made once and kept in the cache, never in the library.
	cached, _ := filepath.Glob(filepath.Join(cacheDir, "converted", books[0]+".azw3.*.epub"))
	if len(cached) != 1 {
		t.Errorf("cache: %v", cached)
	}
	if files, _ := os.ReadDir(dir); len(files) != 1 {
		t.Errorf("the library folder has %d files", len(files))
	}
	// A changed file is converted again, and the old copy goes.
	put(books[0], "Book.azw3", append(bytes.Clone(src), 0))
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "Book.azw3"), later, later)
	if rec := call(t, mux, u, "GET", "/book/as-epub/"+books[0]+"/azw3", ""); rec.Code != 200 {
		t.Fatalf("after a change: %d", rec.Code)
	}
	if cached, _ := filepath.Glob(filepath.Join(cacheDir, "converted", books[0]+".azw3.*.epub")); len(cached) != 1 || strings.Contains(cached[0], ".azw3."+itoa(int64(len(src)))+"-") {
		t.Errorf("cache after a change: %v", cached)
	}

	for path, want := range map[string]int{
		"/book/as-epub/" + books[1] + "/azw3": http.StatusUnprocessableEntity, // DRM
		"/book/as-epub/" + books[0] + "/mobi": http.StatusNotFound,            // no such file
		"/book/as-epub/" + books[0] + "/epub": http.StatusNotFound,            // not converted
		"/book/as-epub/" + books[0] + "/djvu": http.StatusNotFound,
		"/book/as-epub/not-a-uuid!/azw3":      http.StatusNotFound,
	} {
		if rec := call(t, mux, u, "GET", path, ""); rec.Code != want {
			t.Errorf("%s: %d, want %d (%s)", path, rec.Code, want, strings.TrimSpace(rec.Body.String()))
		}
	}
	if rec := call(t, mux, nil, "GET", "/book/as-epub/"+books[0]+"/azw3", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", rec.Code)
	}
	if rec := call(t, mux, u, "POST", "/book/as-epub/"+books[0]+"/azw3", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}
