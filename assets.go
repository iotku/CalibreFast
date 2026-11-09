package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Built browser assets: static/dist/, made by `go generate` (tools/jsbuild).
// File names carry a content hash, so browsers may cache them forever; a
// change gets a new name, which templates pick up via {{ asset "name" }}.
// Everything is held in memory, gzipped once at startup.

const distDir = "static/dist"

type builtAsset struct {
	body, gz    []byte // gz is nil when compressing doesn't help
	contentType string
}

var (
	assetFiles = map[string]*builtAsset{} // by file name in static/dist
	assetNames = map[string]string{}      // manifest: asset name -> file name
)

func init() {
	if err := loadAssets(distDir); err != nil {
		panic(fmt.Sprintf("%v (run `go generate` to build the browser assets)", err))
	}
}

func loadAssets(dir string) error {
	m, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(m, &assetNames); err != nil {
		return fmt.Errorf("%s/manifest.json: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "manifest.json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		ct := mime.TypeByExtension(filepath.Ext(e.Name()))
		if ct == "" {
			ct = "application/json" // .map
		}
		a := &builtAsset{body: body, contentType: ct}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(body)
		_ = zw.Close()
		if buf.Len() < len(body) {
			a.gz = buf.Bytes()
		}
		assetFiles[e.Name()] = a
	}
	for name, file := range assetNames {
		if assetFiles[file] == nil {
			return fmt.Errorf("%s/manifest.json: %s -> %s is missing", dir, name, file)
		}
	}
	return nil
}

// assetURL is the template function asset: the URL of a built asset by its
// name ("app-htmx.js", "style.css"). tools/jsbuild builds whatever names
// the templates use.
func assetURL(name string) (string, error) {
	file, ok := assetNames[name]
	if !ok {
		return "", fmt.Errorf("asset %q isn't built; run `go generate`", name)
	}
	return "/" + distDir + "/" + file, nil
}

// serveAsset serves /static/dist/<file>: cached for a year (the name
// changes with the content), gzipped when the browser accepts it.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	a := assetFiles[path.Base(r.URL.Path)]
	if a == nil || path.Dir(r.URL.Path) != "/"+distDir {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Vary", "Accept-Encoding")
	body := a.body
	if a.gz != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// acceptsGzip reports whether Accept-Encoding allows gzip (not "gzip;q=0").
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(coding) != "gzip" {
			continue
		}
		q := strings.ReplaceAll(params, " ", "")
		return q != "q=0" && q != "q=0.0" && q != "q=0.00" && q != "q=0.000"
	}
	return false
}
