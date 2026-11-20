package main

// Reading Kindle books (AZW3, AZW, MOBI) in the EPUB reader.
//
// GET /book/as-epub/{uuid}/{format} is the book's file in that format
// converted to EPUB (pkg/mobi). The reader opens /read?uuid=…&format=azw3
// and reads that, exactly as it reads an .epub from /download. The library
// is never written to: the EPUB is kept in the cache folder, named by the
// source file's size and modification time and the converter's version, and
// made again when either changes. Its Last-Modified is when it was made, so
// browsers (no-cache: always revalidated) and the reader's If-Range checks
// see a new conversion as a new file.
//
// Positions, bookmarks and highlights are saved under the Kindle format
// ("azw3"), not "epub": they are places in the converted file, which a real
// EPUB of the same book (added later) doesn't share. KOReader's positions in
// a Kindle file don't map onto the converted one either, so KOReader
// progress and highlight import/export stay EPUB and PDF only.

import (
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/iotku/CalibreFast/pkg/mobi"
)

// convertedFormats: the formats read through a conversion to EPUB, in the
// order one is picked for a book with several.
var convertedFormats = []string{"azw3", "azw", "mobi"}

func isConvertedFormat(f string) bool {
	for _, c := range convertedFormats {
		if f == c {
			return true
		}
	}
	return false
}

// readerFormats: every format a web reader opens, in the order a book's
// reader is picked when it has several.
var readerFormats = append([]string{"epub", "pdf"}, convertedFormats...)

// readerLink is the web reader for one format of a book, "" for a format no
// reader opens. extra is added to the query ("preview=1").
func readerLink(param, id, format, extra string) string {
	q := param + "=" + url.QueryEscape(id)
	format = strings.ToLower(format)
	path := "/read?"
	switch {
	case format == "epub":
	case format == "pdf":
		path = "/read-pdf?"
	case isConvertedFormat(format) && param == "uuid":
		q += "&format=" + format
	default:
		return ""
	}
	if extra != "" {
		q += "&" + extra
	}
	return path + q
}

func registerConvertedReadRoutes() {
	http.HandleFunc("GET /book/as-epub/{uuid}/{format}", requireReader(asEPUBHandler))
}

func asEPUBHandler(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	format := strings.ToLower(r.PathValue("format"))
	if !isSafeUUIDComponent(uuid) || !isConvertedFormat(format) {
		http.NotFound(w, r)
		return
	}
	src, err := bookFilePath(uuid, format)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	epub, err := convertedEPUB(uuid, format, src)
	switch {
	case errors.Is(err, mobi.ErrDRM):
		http.Error(w, "this book is DRM-protected and can't be read here", http.StatusUnprocessableEntity)
		return
	case err != nil:
		log.Printf("converting %s (%s) to EPUB: %v", uuid, format, err)
		http.Error(w, "this book's file couldn't be converted for reading", http.StatusUnprocessableEntity)
		return
	}
	name := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)) + ".epub"
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, epub) // Range and If-Range, as /download
}

// convertMu: one conversion at a time (they're rare, and each holds the
// whole book in memory).
var convertMu sync.Mutex

func convertedDir() string {
	if cacheDir != "" {
		return filepath.Join(cacheDir, "converted")
	}
	return filepath.Join(os.TempDir(), "calibrefast-converted")
}

// convertedEPUB is the path of src converted to EPUB, converting it unless
// a copy of the file as it is now is in the cache.
func convertedEPUB(uuid, format, src string) (string, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return "", err
	}
	dir := convertedDir()
	prefix := uuid + "." + format + "."
	dst := filepath.Join(dir, fmt.Sprintf("%s%d-%d-v%d.epub", prefix, fi.Size(), fi.ModTime().UnixNano(), mobi.Version))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	convertMu.Lock()
	defer convertMu.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // made while this waited
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := mobi.ConvertFile(src, dst, mobi.Limits{}); err != nil {
		return "", err
	}
	// Copies of the file as it was, or made by an older converter.
	old, _ := filepath.Glob(filepath.Join(dir, prefix+"*.epub"))
	for _, o := range old {
		if o != dst {
			_ = os.Remove(o)
		}
	}
	return dst, nil
}
