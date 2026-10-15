package main

import (
	"archive/zip"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const maxMarkupSize = 16 << 20 // 16 MiB cap on html/xhtml we buffer for rewriting

// Matches src="..." / href="..." (also xlink:href) with either quote style.
var epubAttrRe = regexp.MustCompile(`(?i)\b(src|href)=(?:"([^"]*)"|'([^']*)')`)

// rewriteEpubURLs rewrites relative and root-absolute references in markup to
// absolute server paths under /epub/{uuid}/.
//
//	root: "/epub/{uuid}"   (what an epub-root-absolute "/OPS/x.png" maps onto)
//	base: URL of the directory containing the current file, e.g. /epub/{uuid}/OPS/
func rewriteEpubURLs(content, root string, base *url.URL) string {
	return epubAttrRe.ReplaceAllStringFunc(content, func(m string) string {
		sub := epubAttrRe.FindStringSubmatch(m)
		attr := sub[1]
		quote := string(m[len(attr)+1]) // char right after '='
		val := sub[2]
		if quote == "'" {
			val = sub[3]
		}

		if val == "" || strings.HasPrefix(val, "#") {
			return m
		}
		u, err := url.Parse(val)
		if err != nil || u.IsAbs() || u.Host != "" { // http:, data:, mailto:, //host/...
			return m
		}

		var out *url.URL
		if strings.HasPrefix(u.Path, "/") {
			c := *u
			c.Path = root + u.Path
			c.RawPath = ""
			out = &c
		} else {
			out = base.ResolveReference(u)
		}
		return attr + "=" + quote + out.String() + quote
	})
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

	bookPath, ok := uuidPathIndex.Load(uuid)
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

			// Everything from inside an epub is untrusted. Serve it in-origin but
			// sandboxed: no scripts, opaque origin, no cookies/storage access, and
			// subresources restricted to this server.
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Content-Security-Policy",
				"sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; font-src 'self'")

			// set content type based on extension
			ext := filepath.Ext(innerPath)
			switch ext {
			case ".html", ".htm", ".xhtml":
				if f.UncompressedSize64 > maxMarkupSize {
					http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
					return
				}
				raw, err := io.ReadAll(io.LimitReader(rc, maxMarkupSize))
				if err != nil {
					http.Error(w, "read error", 500)
					return
				}

				root := "/epub/" + url.PathEscape(uuid)
				dir := path.Dir(f.Name) // zip entry name, not request input
				basePath := root + "/"
				if dir != "." {
					basePath += dir + "/"
				}
				base := &url.URL{Path: basePath}

				if ext == ".xhtml" {
					h.Set("Content-Type", "application/xhtml+xml")
				} else {
					h.Set("Content-Type", "text/html")
				}
				_, _ = io.WriteString(w, rewriteEpubURLs(string(raw), root, base))

			case ".css":
				h.Set("Content-Type", "text/css")
			case ".js":
				h.Set("Content-Type", "application/javascript")
			case ".png":
				h.Set("Content-Type", "image/png")
			case ".jpg", ".jpeg":
				h.Set("Content-Type", "image/jpeg")
			case ".gif":
				h.Set("Content-Type", "image/gif")
			case ".svg":
				h.Set("Content-Type", "image/svg+xml")
			case ".ttf":
				h.Set("Content-Type", "font/ttf")
			case ".otf":
				h.Set("Content-Type", "font/otf")
			case ".woff":
				h.Set("Content-Type", "font/woff")
			case ".woff2":
				h.Set("Content-Type", "font/woff2")
			case ".xml", ".opf", ".ncx":
				h.Set("Content-Type", "application/xml")
			case ".xpgt":
				w.WriteHeader(http.StatusNoContent)
				return
			default:
				h.Set("Content-Type", "application/octet-stream")
			}

			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(innerPath)))
			_, _ = io.Copy(w, rc)
			return
		}
	}

	http.NotFound(w, r)
}
