package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

var uuidPathIndex sync.Map // map[string]string uuid -> relative path to book dir
var modTimeCache sync.Map  // map[string]int64 uuid -> last modified time of book from calibre database, used for cache-busting cover.jpg and cover-thumb.jpg
var formatCache sync.Map   // map[string][]string uuid -> list of formats and their paths relative to the baseDir

// imageSem bounds cover-thumb requests in progress, across all clients: one
// per core, since generating a thumbnail is CPU-bound (~80 ms and ~30 MB for
// a typical cover). GOMAXPROCS is the core count, or the container's CPU
// limit when it's lower.
var imageSem = make(chan struct{}, runtime.GOMAXPROCS(0))

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

// bookCoverStamp loads the modification timestamp (shared coverStamp format) from cache
// OR fallsbacks to os.stat if modTimeCache can't find the uuid
func bookCoverStamp(uuid string) (string, error) {
	// Prefer Calibre's cached modification timestamp.
	if value, ok := modTimeCache.Load(uuid); ok {
		if lastModified, ok := value.(string); ok {
			if stamp := coverStamp(lastModified); stamp != "" {
				return stamp, nil
			}
		}
	}

	// Fallback: resolve the original cover's modification time.
	path, ok := uuidPathIndex.Load(uuid)
	if !ok {
		var err error
		path, err = resolveBookPath(uuid)
		if err != nil {
			return "", err
		}
	}

	origPath := filepath.Join(baseDir, path.(string), "cover.jpg")
	info, err := os.Stat(origPath)
	if err != nil {
		return "", err
	}

	return info.ModTime().UTC().Format("20060102150405"), nil
}

func coverThumbHandler(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/cover-thumb/")
	if !isSafeUUIDComponent(uuid) {
		http.Error(w, "invalid cover id", http.StatusBadRequest)
		return
	}
	defer thumbRequestStarted()()

	ctx := r.Context()

	// Fast path: serve cached thumbnails without waiting for imageSem.
	if data, ok := readCachedThumb(uuid); ok {
		writeThumb(w, data, true)
		return
	}

	select {
	case imageSem <- struct{}{}:

	// A request the browser cancels (a cover scrolled away) stops before the
	// next expensive step and leaves its thumbnail to the idle-time backlog.
	case <-ctx.Done():
		backlogThumb(uuid)
		return
	}
	defer func() { <-imageSem }()

	// Another request may have generated the thumbnail while we waited.
	if data, ok := readCachedThumb(uuid); ok {
		writeThumb(w, data, true)
		return
	}

	data, _, err := loadThumb(uuid, ctx.Err)
	switch {
	case errors.Is(err, context.Canceled):
		backlogThumb(uuid)
	case err != nil:
		http.NotFound(w, r)
	default:
		writeThumb(w, data, false)
	}
}

// loadThumb returns the book's cached thumbnail, first making it if it's
// missing or older than cover.jpg. abort is checked before each expensive
// step (reading and decoding the cover, scaling, encoding); its error stops
// the work and is returned.
func loadThumb(uuid string, abort func() error) (data []byte, cacheHit bool, err error) {
	path, ok := uuidPathIndex.Load(uuid)
	if !ok {
		if path, err = resolveBookPath(uuid); err != nil {
			return nil, false, err
		}
	}
	origPath := filepath.Join(baseDir, path.(string), "cover.jpg")
	origStat, err := os.Stat(origPath)
	if err != nil {
		return nil, false, err
	}

	stamp, err := bookCoverStamp(uuid)
	if err != nil {
		return nil, false, err
	}

	thumbPath := thumbCachePath(uuid, stamp)
	if thumbStat, err := os.Stat(thumbPath); err == nil && !thumbStat.ModTime().Before(origStat.ModTime()) {
		if data, err := os.ReadFile(thumbPath); err == nil {
			return data, true, nil
		}
	}

	if err := abort(); err != nil {
		return nil, false, err
	}
	orig, err := os.ReadFile(origPath)
	if err != nil {
		return nil, false, err
	}
	if data, err = makeThumb(orig, abort); err != nil {
		return nil, false, err
	}

	// The thumbnail takes the cover's modification time: it's stale once the
	// cover is newer.
	if err := ensureDir(thumbPath); err != nil {
		log.Printf("cover-thumb: %v", err)
	}
	if err := os.WriteFile(thumbPath, data, 0644); err != nil {
		log.Printf("cover-thumb: failed to write thumbnail %s: %v", thumbPath, err)
	} else {
		_ = os.Chtimes(thumbPath, origStat.ModTime(), origStat.ModTime())
	}
	return data, false, nil
}

// Thumbnails only need to look good at library-grid sizes (cards are ~200 CSS
// px wide, so 400px covers 2x screens); the full cover is shown in the book
// modal.
const (
	thumbWidth   = 400
	thumbQuality = 80
)

// thumbCachePath is the cached thumbnail for a book. Once we're production ready,
// consider changing the path name (e.g. thumb-v2) to ensure fresh generation
func thumbCachePath(uuid, stamp string) string {
	return filepath.Join(cacheDir, "covers", "thumb", uuid+"-"+stamp+".jpg")
}

// readCachedThumb returns the cached thumbnail for the book's current cover
// version without requiring the image generation semaphore. It uses Calibre's
// cached modification timestamp, falling back to the filesystem if unavailable.
func readCachedThumb(uuid string) ([]byte, bool) {
	stamp, err := bookCoverStamp(uuid)
	if err != nil {
		return nil, false
	}

	data, err := os.ReadFile(thumbCachePath(uuid, stamp))
	return data, err == nil
}

// makeThumb returns a thumbnail of the cover image, never larger than the
// original: covers already at or below thumbWidth, or whose thumbnail would
// be more bytes than the original, are returned unchanged. abort is checked
// between decoding, scaling and encoding; its error stops the work.
func makeThumb(orig []byte, abort func() error) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	if img.Bounds().Dx() <= thumbWidth {
		return orig, nil
	}
	if err := abort(); err != nil {
		return nil, err
	}
	small := resizeToWidth(img, thumbWidth)
	if err := abort(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: thumbQuality}); err != nil {
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

// coverChanged records a new cover.jpg in metadata.db, as Calibre does: the
// book has a cover and was modified now.
func coverChanged(uuid string) {
	if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1, last_modified = ? WHERE uuid = ?`,
		calibreDateTime(time.Now()), uuid); err != nil {
		log.Printf("cover: failed to update %s in metadata.db: %v", uuid, err)
	}
}
