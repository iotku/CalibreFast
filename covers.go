package main

import (
	"context"
	"errors"
	"io/fs"
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

	"github.com/iotku/CalibreFast/pkg/imageutil"
	"github.com/iotku/CalibreFast/pkg/logsafe"
)

var uuidPathIndex sync.Map // map[string]string uuid -> relative path to book dir
var modTimeCache sync.Map  // map[string]int64 uuid -> last modified time of book from calibre database, used for cache-busting cover.jpg and cover-thumb.jpg
// noCover: uuid -> struct{}, books whose cover.jpg wasn't there when a
// thumbnail was asked for. Cleared by coverChanged and catalogChanged.
var noCover sync.Map

var formatCache sync.Map // map[string][]string uuid -> list of formats and their paths relative to the baseDir

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
	f, err := libraryOpen(coverPath)
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
	info, err := libraryStat(origPath)
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
	// A book already found to have no cover isn't looked for again on the
	// library (which may be slow storage) until its cover or entry changes.
	if _, missing := noCover.Load(uuid); missing {
		noThumb(w, r)
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
		if errors.Is(err, fs.ErrNotExist) {
			noCover.Store(uuid, struct{}{})
			noThumb(w, r)
			return
		}
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
	origStat, err := libraryStat(origPath)
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
	orig, err := libraryReadFile(origPath)
	if err != nil {
		return nil, false, err
	}
	if data, err = makeThumb(orig, abort); err != nil {
		return nil, false, err
	}

	// The thumbnail takes the cover's modification time: it's stale once the
	// cover is newer.
	if err := writeThumbFile(thumbPath, data, origStat.ModTime()); err != nil {
		log.Printf("cover-thumb: failed to write thumbnail %s: %v", logsafe.Text(thumbPath), logsafe.Text(err))
	}
	return data, false, nil
}

// writeThumbFile writes a thumbnail under a temporary name and renames it
// into place, so nobody reads half a file: not this server's requests, nor
// another process making thumbnails at the same time (-build-thumbnails).
func writeThumbFile(path string, data []byte, modTime time.Time) error {
	if err := ensureDir(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".thumb-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		_ = os.Chtimes(tmp, modTime, modTime)
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// thumbCached: the book's thumbnail for its current cover is in the cache
// (a stat, nothing read).
func thumbCached(uuid string) bool {
	stamp, err := bookCoverStamp(uuid)
	if err != nil {
		return false
	}
	_, err = os.Stat(thumbCachePath(uuid, stamp))
	return err == nil
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

// makeThumb returns a thumbnail of the cover image (imageutil.Thumb at
// thumbWidth and thumbQuality). abort is checked between decoding, scaling
// and encoding; its error stops the work.
func makeThumb(orig []byte, abort func() error) ([]byte, error) {
	return imageutil.Thumb(orig, thumbWidth, thumbQuality, abort)
}

func writeThumb(w http.ResponseWriter, data []byte, cacheHit bool) {
	w.Header().Set("Content-Type", http.DetectContentType(data)) // the original may be served as-is (not always JPEG)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Cache-Hit", strconv.FormatBool(cacheHit))
	_, _ = w.Write(data)
}

// noThumb answers for a book without a cover. The browser keeps the answer
// for a day, so a coverless card costs one request per day rather than one
// per view (or several on one page, where the book shows in more than one
// row). The URL carries the book's last_modified stamp, which a new cover
// changes, so the cover shows at once when one is added. A day, not a year:
// a library on a network mount that was briefly missing would otherwise
// leave covers blank for good.
func noThumb(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.NotFound(w, r)
}

// coverChanged records a new cover.jpg in metadata.db, as Calibre does: the
// book has a cover and was modified now.
func coverChanged(uuid string) {
	noCover.Delete(uuid)
	if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1, last_modified = ? WHERE uuid = ?`,
		calibreDateTime(time.Now()), uuid); err != nil {
		log.Printf("cover: failed to update %s in metadata.db: %v", uuid, err)
	}
	catalogChanged(uuid)
	// The page files carry the cover's stamp: the grid, Discover and
	// Shuffle ask for the new cover, not the one browsers cached.
	refreshPagesFor(uuid)
}
