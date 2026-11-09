package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Browser assets, held in memory and gzipped once at startup. Browsers may
// cache them forever, because their URLs change whenever their content does:
//
//   - static/dist/: our scripts and styles, made by `go generate`
//     (tools/jsbuild). File names carry a content hash; templates get the
//     current one via {{ asset "name" }}.
//   - static/vendor/<package>@<version>/: third-party libraries, the files
//     as published on npm. Templates and web/ link them by path.

const (
	distDir   = "static/dist"
	vendorDir = "static/vendor"
)

type builtAsset struct {
	body, gz    []byte // gz is nil when compressing doesn't help
	contentType string
}

var (
	assetFiles = map[string]*builtAsset{} // by URL path
	assetNames = map[string]string{}      // dist manifest: asset name -> file name
)

func init() {
	if err := loadAssets(distDir); err != nil {
		panic(fmt.Sprintf("%v (run `go generate` to build the browser assets)", err))
	}
	if err := loadVendor(vendorDir); err != nil {
		panic(err)
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
		addAsset("/"+dir+"/"+e.Name(), body)
	}
	for name, file := range assetNames {
		if assetFiles["/"+dir+"/"+file] == nil {
			return fmt.Errorf("%s/manifest.json: %s -> %s is missing", dir, name, file)
		}
	}
	return nil
}

// loadVendor loads every file under dir (static/vendor), at its path.
func loadVendor(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		addAsset("/"+filepath.ToSlash(p), body)
		return nil
	})
}

// addAsset holds body for serving at urlPath, gzipped when that's smaller.
func addAsset(urlPath string, body []byte) {
	ext := path.Ext(urlPath)
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "text/plain; charset=utf-8" // LICENSE files
		if ext == ".map" {
			ct = "application/json"
		}
	}
	a := &builtAsset{body: body, contentType: ct}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(body)
	_ = zw.Close()
	if buf.Len() < len(body) {
		a.gz = buf.Bytes()
	}
	assetFiles[urlPath] = a
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

// serveAsset serves /static/dist/ and /static/vendor/: cached for a year
// (the URL changes with the content), gzipped when the browser accepts it.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	a := assetFiles[r.URL.Path]
	if a == nil {
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
