package main

import (
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	defer f.Close()
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

	// 1. FAST PATH: already cached
	if f, err := os.Open(thumbPath); err == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("X-Cache-Hit", "true")
		http.ServeContent(w, r, "thumb.jpg", time.Time{}, f)
		return
	}

	// 2. SLOW PATH: generate
	origPath := filepath.Join(baseDir, path.(string), "cover.jpg")

	f, err := os.Open(origPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	thumb := resizeToWidth(img, 600)

	out, err := os.Create(thumbPath)
	if err == nil {
		jpeg.Encode(out, thumb, &jpeg.Options{Quality: 85})
		out.Close()
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("X-Cache-Hit", "false")
	jpeg.Encode(w, thumb, &jpeg.Options{Quality: 85})
}

func resizeToWidth(img image.Image, width int) image.Image {
	bounds := img.Bounds()

	scale := float64(width) / float64(bounds.Dx())
	height := int(float64(bounds.Dy()) * scale)

	dst := image.NewRGBA(image.Rect(0, 0, width, height))

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			srcX := int(float64(x) / scale)
			srcY := int(float64(y) / scale)

			dst.Set(x, y, img.At(srcX, srcY))
		}
	}

	return dst
}
