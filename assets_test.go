package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
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
