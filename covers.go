package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

var coverIndex sync.Map               // map[string]string
var formatCache sync.Map              // map[string][]string
var imageSem = make(chan struct{}, 8) // only n HDD reads at once
func coverHandler(w http.ResponseWriter, r *http.Request) {
	imageSem <- struct{}{}        // acquire slot
	defer func() { <-imageSem }() // release slot
	// uuid comes from URL: /cover/{uuid}
	uuid := filepath.Base(r.URL.Path)

	if uuid == "" {
		http.Error(w, "missing uuid", http.StatusBadRequest)
		return
	}

	bookPath, ok := coverIndex.Load(uuid)
	if !ok {
		http.Error(w, "book not found", http.StatusNotFound)
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

	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("Content-Type", "image/jpeg")

	http.ServeContent(w, r, "cover.jpg", fileModTime(f), f)
}

func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0755)
}

func coverThumbHandler(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/cover-thumb/")

	path, ok := coverIndex.Load(uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	thumbPath := filepath.Join(cacheDir, "covers", "thumb", uuid+".jpg")
	if err := ensureDir(thumbPath); err != nil {
		println(err.Error())
	}

	// FAST PATH: already cached
	if f, err := os.Open(thumbPath); err == nil {
		defer func(f *os.File) {
			logErr(f.Close(), "failed to close thumb "+thumbPath)
		}(f)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("X-Cache-Hit", "true")
		http.ServeContent(w, r, "thumb.jpg", time.Time{}, f)
		return
	}

	// SLOW PATH: generate
	origPath := filepath.Join(baseDir, path.(string), "cover.jpg")

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

	// Write to file
	go func(data []byte, path string) {
		logErr(os.WriteFile(path, data, 0644),
			"failed to write thumbnail "+path)
	}(append([]byte(nil), data...), thumbPath)

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
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
