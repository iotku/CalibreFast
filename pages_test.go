package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestServePagesCaching(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "page1.json"), []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	pagesVersion.Store(12345)
	h := servePages(dir)
	get := func(url string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		return rec.Code, rec.Header().Get("Cache-Control")
	}

	if code, cc := get("/pages/page1.json?v=" + currentPagesVersion()); code != 200 || cc != "private, max-age=31536000, immutable" {
		t.Errorf("current version: %d %q, want 200 and cached for good", code, cc)
	}
	for _, url := range []string{"/pages/page1.json", "/pages/page1.json?v=old"} {
		if code, cc := get(url); code != 200 || cc != "no-cache" {
			t.Errorf("%s: %d %q, want 200 and no-cache", url, code, cc)
		}
	}
	// A rewrite changes the version: what was current is now revalidated.
	old := currentPagesVersion()
	pagesVersion.Store(67890)
	if _, cc := get("/pages/page1.json?v=" + old); cc != "no-cache" {
		t.Errorf("stale version: %q, want no-cache", cc)
	}
}
