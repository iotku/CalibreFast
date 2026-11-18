package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceWorker(t *testing.T) {
	rec := httptest.NewRecorder()
	serveWorker(rec, httptest.NewRequest("GET", "/sw.js", nil))
	body := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	reader, _ := assetURL("reader.js")
	if !strings.HasPrefix(body, "self.CF_ASSETS=[") || !strings.Contains(body, `"`+reader+`"`) || !strings.Contains(body, "epub.min.js") {
		t.Fatalf("asset list: %.300s", body)
	}
	if strings.Contains(body[:strings.Index(body, "\n")], ".map") {
		t.Error("source maps in the asset list")
	}

	rec = httptest.NewRecorder()
	offlineHandler(rec, httptest.NewRequest("GET", "/offline", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "offline-reading-books") {
		t.Fatalf("offline page: %d", rec.Code)
	}
	// On a private server neither loads without a session: the worker's
	// asset list names every script and stylesheet.
	for _, p := range []string{"/sw.js", "/offline"} {
		if isPublicPath(p) {
			t.Errorf("%s is public: it describes the app to signed-out visitors", p)
		}
	}
}

// The EPUB reader reads books in parts with Range requests (web/epub-source.ts).
func TestDownloadRanges(t *testing.T) {
	_, books := setupLibraryAndUser(t, 1)
	uuid := books[0]
	path, _ := resolveBookPath(uuid)
	dir := filepath.Join(baseDir, path)
	_ = os.MkdirAll(dir, 0o755)
	content := strings.Repeat("0123456789", 1000)
	if err := os.WriteFile(filepath.Join(dir, "book.epub"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	formatCache.Delete(uuid)
	t.Cleanup(func() { formatCache.Delete(uuid) })

	get := func(h map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/download/"+uuid+"/epub", nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		downloadHandler(rec, r)
		return rec
	}
	rec := get(map[string]string{"Range": "bytes=-100"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != content[len(content)-100:] ||
		rec.Header().Get("Content-Range") != "bytes 9900-9999/10000" {
		t.Fatalf("suffix range: %d %q", rec.Code, rec.Header().Get("Content-Range"))
	}
	lm := rec.Header().Get("Last-Modified")
	if lm == "" {
		t.Fatal("no Last-Modified")
	}
	if rec := get(map[string]string{"Range": "bytes=10-19", "If-Range": lm}); rec.Code != 206 || rec.Body.String() != content[10:20] {
		t.Fatalf("range: %d %q", rec.Code, rec.Body.String())
	}
	// The file changed: the whole new file, not a range of it.
	if rec := get(map[string]string{"Range": "bytes=10-19", "If-Range": "Mon, 02 Jan 2006 15:04:05 GMT"}); rec.Code != 200 {
		t.Fatalf("stale If-Range: %d", rec.Code)
	}
}

// The web app manifest makes the site installable (Chrome's "Install app"):
// a name, a start page, standalone display and 192/512 px icons that exist.
func TestManifest(t *testing.T) {
	rec := httptest.NewRecorder()
	manifestHandler(rec, httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type %q", ct)
	}
	var m struct {
		Name, Display string
		Icons                   []struct{ Src, Sizes, Purpose string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Name == "" || m.Display != "standalone" {
		t.Errorf("manifest: %+v", m)
	}
	sizes := map[string]bool{}
	for _, i := range m.Icons {
		sizes[i.Sizes] = true
		if assetFiles[i.Src] == nil {
			t.Errorf("icon %s isn't served", i.Src)
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] {
		t.Errorf("icon sizes %v: installing needs 192 and 512", sizes)
	}
}

// The icons are public (the browser may fetch them without a session); the
// manifest, which names the library, isn't.
func TestAppIconsPublic(t *testing.T) {
	if !isPublicPath("/static/icons/icon-192-v1.png") {
		t.Error("icons should be public")
	}
	if isPublicPath("/manifest.webmanifest") {
		t.Error("the manifest names the library: it shouldn't be public")
	}
	if strings.Contains(string(appHead("reader")), "theme-color") {
		t.Error("the readers set their own theme-color")
	}
	if !strings.Contains(string(appHead()), `rel="manifest"`) {
		t.Error("appHead should link the manifest")
	}
}
