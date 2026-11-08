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

	thumbPath := filepath.Join(cacheDir, "covers", "thumb", uuid+".jpg")
	if err := ensureDir(thumbPath); err != nil {
		println(err.Error())
	}

	// FAST PATH: cached thumbnail exists AND is NOT older than original cover.jpg
	if thumbStat, err := os.Stat(thumbPath); err == nil && !thumbStat.ModTime().Before(origStat.ModTime()) {
		if f, err := os.Open(thumbPath); err == nil {
			defer func(f *os.File) {
				logErr(f.Close(), "failed to close thumb "+thumbPath)
			}(f)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("X-Cache-Hit", "true")
			http.ServeContent(w, r, "thumb.jpg", thumbStat.ModTime(), f)
			return
		}
	}

	// SLOW PATH: generate fresh thumbnail directly from cover.jpg
	f, err := os.Open(origPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func(f *os.File) {
		logErr(f.Close(), "failed to close original cover.jpg at "+origPath)
	}(f)

	img, _, err := image.Decode(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	thumb := resizeToWidth(img, 600)
	var buf bytes.Buffer
	err = jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: 85})
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		log.Println("failed to encode cover jpg at " + thumbPath)
		return
	}

	data := buf.Bytes()

	// Write to cache file synchronously with current timestamp
	if err := os.WriteFile(thumbPath, data, 0644); err != nil {
		log.Printf("cover-thumb: failed to write thumbnail %s: %v", thumbPath, err)
	} else {
		_ = os.Chtimes(thumbPath, origStat.ModTime(), origStat.ModTime())
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Cache-Hit", "false")
	_, _ = w.Write(data)
}

func resizeToWidth(img image.Image, width int) image.Image {
	bounds := img.Bounds()

	scale := float64(width) / float64(bounds.Dx())
	height := int(float64(bounds.Dy()) * scale)

	dst := image.NewRGBA(image.Rect(0, 0, width, height))

	draw.NearestNeighbor.Scale(
		dst,
		dst.Bounds(),
		img,
		bounds,
		draw.Over,
		nil,
	)

	return dst
}
