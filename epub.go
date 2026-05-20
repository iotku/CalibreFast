package main

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

func rewriteEpubURLs(content, base string) string {
	// rewrite href="..." and src="..." that are relative (don't start with http/https/#//)
	attrs := []string{"src", "href"}
	for _, attr := range attrs {
		// find all attr="value" occurrences
		prefix := attr + `="`
		result := strings.Builder{}
		remaining := content
		for {
			idx := strings.Index(remaining, prefix)
			if idx == -1 {
				result.WriteString(remaining)
				break
			}
			result.WriteString(remaining[:idx+len(prefix)])
			remaining = remaining[idx+len(prefix):]

			// find closing quote
			end := strings.Index(remaining, `"`)
			if end == -1 {
				result.WriteString(remaining)
				break
			}
			url := remaining[:end]
			remaining = remaining[end:]

			// only rewrite relative URLs
			if !strings.HasPrefix(url, "http") &&
				!strings.HasPrefix(url, "#") &&
				!strings.HasPrefix(url, "//") &&
				!strings.HasPrefix(url, "data:") &&
				url != "" {
				result.WriteString(base + url)
			} else {
				result.WriteString(url)
			}
		}
		content = result.String()
	}
	return content
}

func epubFileHandler(w http.ResponseWriter, r *http.Request) {
	// /epub/{uuid}/{path...}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/epub/"), "/", 2)
	if len(parts) < 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	uuid := parts[0]
	innerPath := parts[1]

	bookPath, ok := coverIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// find the epub file
	files := resolveFormats(uuid, baseDir, bookPath.(string))
	var epubFile string
	for _, f := range files {
		if strings.HasSuffix(f, ".epub") {
			epubFile = filepath.Join(baseDir, bookPath.(string), f)
			break
		}
	}
	if epubFile == "" {
		http.NotFound(w, r)
		return
	}

	// open the zip
	zr, err := zip.OpenReader(epubFile)
	if err != nil {
		http.Error(w, "failed to open epub", 500)
		return
	}
	defer func(zr *zip.ReadCloser) {
		logErr(zr.Close(), "failed to close zip.ReadCloser in epubFileHandler")
	}(zr)

	// find the requested file inside the zip
	for _, f := range zr.File {
		if f.Name == innerPath {
			rc, err := f.Open()
			if err != nil {
				http.Error(w, "failed to read file", 500)
				return
			}
			defer func(rc io.ReadCloser) {
				logErr(rc.Close(), "failed to close zip.ReadCloser in epubFileHandler")
			}(rc)

			// set content type based on extension
			ext := filepath.Ext(innerPath)
			switch ext {
			case ".html", ".xhtml":
				content, err := io.ReadAll(rc)
				if err != nil {
					http.Error(w, "read error", 500)
					return
				}

				// get the directory of the current file within the epub
				fileDir := path.Dir(innerPath) // e.g. "OPS" for "OPS/chapter1.xhtml"
				base := fmt.Sprintf("/epub/%s/%s/", uuid, fileDir)

				s := rewriteEpubURLs(string(content), base)
				w.Header().Set("Content-Type", "application/xhtml+xml")
				w.Write([]byte(s))
				return
			case ".css":
				w.Header().Set("Content-Type", "text/css")
			case ".js":
				w.Header().Set("Content-Type", "application/javascript")
			case ".png":
				w.Header().Set("Content-Type", "image/png")
			case ".jpg", ".jpeg":
				w.Header().Set("Content-Type", "image/jpeg")
			case ".gif":
				w.Header().Set("Content-Type", "image/gif")
			case ".svg":
				w.Header().Set("Content-Type", "image/svg+xml")
			case ".ttf":
				w.Header().Set("Content-Type", "font/ttf")
			case ".otf":
				w.Header().Set("Content-Type", "font/otf")
			case ".woff":
				w.Header().Set("Content-Type", "font/woff")
			case ".woff2":
				w.Header().Set("Content-Type", "font/woff2")
			case ".xml", ".opf", ".ncx":
				w.Header().Set("Content-Type", "application/xml")
			case ".xpgt":
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				w.Header().Set("Content-Type", "application/octet-stream")
			}

			io.Copy(w, rc)
			return
		}
	}

	http.NotFound(w, r)
}
