package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestServeAsset(t *testing.T) {
	url, err := assetURL("app-htmx.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assetURL("nope.js"); err == nil {
		t.Error("assetURL of an unknown name: no error")
	}

	get := func(path, acceptEncoding string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if acceptEncoding != "" {
			req.Header.Set("Accept-Encoding", acceptEncoding)
		}
		rec := httptest.NewRecorder()
		serveAsset(rec, req)
		return rec
	}

	plain := get(url, "")
	if plain.Code != 200 || plain.Header().Get("Content-Encoding") != "" {
		t.Fatalf("plain: %d %q", plain.Code, plain.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(plain.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q", plain.Header().Get("Cache-Control"))
	}
	if !strings.HasPrefix(plain.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("Content-Type = %q", plain.Header().Get("Content-Type"))
	}

	gz := get(url, "br, gzip, deflate")
	if gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Accept-Encoding gzip: Content-Encoding = %q", gz.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	unzipped, _ := io.ReadAll(zr)
	if !bytes.Equal(unzipped, plain.Body.Bytes()) {
		t.Error("gzipped body doesn't match the plain one")
	}

	if rec := get(url, "gzip;q=0"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 still got gzip")
	}
	for _, p := range []string{"/static/dist/nope.js", "/static/dist/x/" + strings.TrimPrefix(url, "/static/dist/")} {
		if rec := get(p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		}
	}
}

// TestVendor checks the third-party libraries in static/vendor/: served
// with types browsers accept, linked by path from templates and web/, every
// package in use, and no script loaded from another host.
func TestVendor(t *testing.T) {
	var vendored []string
	for p, a := range assetFiles {
		if !strings.HasPrefix(p, "/"+vendorDir+"/") {
			continue
		}
		vendored = append(vendored, p)
		want := map[string]string{".js": "text/javascript", ".mjs": "text/javascript", ".wasm": "application/wasm"}[path.Ext(p)]
		if !strings.HasPrefix(a.contentType, want) {
			t.Errorf("%s: Content-Type %q, want %s", p, a.contentType, want)
		}
	}
	if len(vendored) == 0 {
		t.Fatal("nothing loaded from " + vendorDir)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", vendored[0], nil)
	req.Header.Set("Accept-Encoding", "gzip")
	serveAsset(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("%s: %d, Cache-Control %q", vendored[0], rec.Code, rec.Header().Get("Cache-Control"))
	}

	// Every /static/vendor/ path in the sources exists (a path ending in /
	// is a directory a library fetches files from), and every package is used.
	refRe := regexp.MustCompile(`/` + vendorDir + `/[^"'` + "`" + `\s)]+`)
	// A script URL (src, href or string literal) on another host.
	externalRe := regexp.MustCompile(`["'` + "`" + `]\s*(https?:)?//[^"'` + "`" + `\s]+\.m?js\s*[?"'` + "`" + `]`)
	files, _ := filepath.Glob("templates/*.html")
	ts, _ := filepath.Glob("web/*.ts")
	used := map[string]bool{} // package dir -> referenced
	for _, f := range append(files, ts...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range externalRe.FindAll(b, -1) {
			t.Errorf("%s: script from another host: %s (vendor it in %s)", f, m, vendorDir)
		}
		for _, ref := range refRe.FindAllString(string(b), -1) {
			pkg := strings.Join(strings.SplitN(ref, "/", 5)[:4], "/") // /static/vendor/<pkg>@<version>
			used[pkg] = true
			if strings.HasSuffix(ref, "/") {
				if !slices.ContainsFunc(vendored, func(p string) bool { return strings.HasPrefix(p, ref) }) {
					t.Errorf("%s: %s: no such directory", f, ref)
				}
			} else if assetFiles[ref] == nil {
				t.Errorf("%s: %s: no such file", f, ref)
			}
		}
	}
	dirs, err := os.ReadDir(vendorDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !used["/"+vendorDir+"/"+d.Name()] {
			t.Errorf("%s/%s isn't used by templates/ or web/; delete it", vendorDir, d.Name())
		}
	}
}
