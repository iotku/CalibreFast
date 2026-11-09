package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/draw"
)

var uuidPathIndex sync.Map // map[string]string uuid -> relative path to book dir
var modTimeCache sync.Map  // map[string]int64 uuid -> last modified time of book from calibre database, used for cache-busting cover.jpg and cover-thumb.jpg
var formatCache sync.Map   // map[string][]string uuid -> list of formats and their paths relative to the baseDir
// TODO: Performance, maybe this can be increased?
var imageSem = make(chan struct{}, 4) // only n HDD reads at once

func coverHandler(w http.ResponseWriter, r *http.Request) {
	// uuid comes from URL: /cover/{uuid}
	uuid := filepath.Base(r.URL.Path)

	if uuid == "" {
		http.Error(w, "missing uuid", http.StatusBadRequest)
		return
	}

	bookPath, ok := uuidPathIndex.Load(uuid)
	if !ok {
		var err error
		bookPath, err = resolveBookPath(uuid)
		if err != nil {
			http.Error(w, "book not found", http.StatusNotFound)
			return
		}
	}

	// Build safe path
	coverPath := filepath.Join(baseDir, bookPath.(string), "cover.jpg")

	// prevent path traversal escape (extra safety)
	coverPath, err := filepath.Abs(coverPath)
	if err != nil {
		http.Error(w, "invalid path", 500)
		return
	}

	// ensure file exists
	f, err := os.Open(coverPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func(f *os.File) {
		logErr(f.Close(), "failed to close cover.jpg at "+coverPath)
	}(f)

	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	w.Header().Set("Content-Type", "image/jpeg")

	http.ServeContent(w, r, "cover.jpg", fileModTime(f), f)
}

var uuidComponentRe = regexp.MustCompile(`^[a-fA-F0-9-]+$`)

func isSafeUUIDComponent(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, "/") || strings.Contains(s, "\\") || strings.Contains(s, "..") {
		return false
	}
	return uuidComponentRe.MatchString(s)
}

func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0755)
}

func coverThumbHandler(w http.ResponseWriter, r *http.Request) {
	imageSem <- struct{}{}        // acquire slot
	defer func() { <-imageSem }() // release slot
	uuid := strings.TrimPrefix(r.URL.Path, "/cover-thumb/")
	if !isSafeUUIDComponent(uuid) {
		http.Error(w, "invalid cover id", http.StatusBadRequest)
		return
	}

	path, ok := uuidPathIndex.Load(uuid)
	if !ok {
		var err error
		path, err = resolveBookPath(uuid)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}

	origPath := filepath.Join(baseDir, path.(string), "cover.jpg")
	origStat, err := os.Stat(origPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	thumbPath := thumbCachePath(uuid)
	if err := ensureDir(thumbPath); err != nil {
		println(err.Error())
	}

	// FAST PATH: cached thumbnail exists AND is NOT older than original cover.jpg
	if thumbStat, err := os.Stat(thumbPath); err == nil && !thumbStat.ModTime().Before(origStat.ModTime()) {
		if data, err := os.ReadFile(thumbPath); err == nil {
			writeThumb(w, data, true)
			return
		}
	}

	// SLOW PATH: generate fresh thumbnail from cover.jpg
	orig, err := os.ReadFile(origPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := makeThumb(orig)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Write to cache file synchronously with current timestamp
	if err := os.WriteFile(thumbPath, data, 0644); err != nil {
		log.Printf("cover-thumb: failed to write thumbnail %s: %v", thumbPath, err)
	} else {
		_ = os.Chtimes(thumbPath, origStat.ModTime(), origStat.ModTime())
	}
	writeThumb(w, data, false)
}

// Thumbnails only need to look good at library-grid sizes (cards are ~200 CSS
// px wide, so 400px covers 2x screens); the full cover is shown in the book
// modal.
const (
	thumbWidth   = 400
	thumbQuality = 80
)

// thumbCachePath is the cached thumbnail for a book. The directory is
// versioned: bump it when thumbnail settings change so old ones regenerate.
func thumbCachePath(uuid string) string {
	return filepath.Join(cacheDir, "covers", "thumb-v2", uuid+".jpg")
}

// makeThumb returns a thumbnail of the cover image, never larger than the
// original: covers already at or below thumbWidth, or whose thumbnail would
// be more bytes than the original, are returned unchanged.
func makeThumb(orig []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	if img.Bounds().Dx() <= thumbWidth {
		return orig, nil
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, resizeToWidth(img, thumbWidth), &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, err
	}
	if buf.Len() >= len(orig) {
		return orig, nil
	}
	return buf.Bytes(), nil
}

func writeThumb(w http.ResponseWriter, data []byte, cacheHit bool) {
	w.Header().Set("Content-Type", http.DetectContentType(data)) // the original may be served as-is (not always JPEG)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Cache-Hit", strconv.FormatBool(cacheHit))
	_, _ = w.Write(data)
}

func resizeToWidth(img image.Image, width int) image.Image {
	bounds := img.Bounds()

	scale := float64(width) / float64(bounds.Dx())
	height := max(1, int(float64(bounds.Dy())*scale+0.5))

	dst := image.NewRGBA(image.Rect(0, 0, width, height))

	draw.CatmullRom.Scale(
		dst,
		dst.Bounds(),
		img,
		bounds,
		draw.Over,
		nil,
	)

	return dst
}
