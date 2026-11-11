package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// uploadCoverHandler handles POST /upload-cover/{uuid}: the image in the
// request (readCoverUpload) becomes the book's cover.jpg. The thumbnail cache
// is invalidated and has_cover is set to 1 in the database.
func uploadCoverHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uuid := r.PathValue("uuid")
	if uuid == "" || !isSafeUUIDComponent(uuid) {
		http.Error(w, "missing or invalid UUID", http.StatusBadRequest)
		return
	}

	bookPath, err := resolveBookPath(uuid)
	if err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}

	img, err := readCoverUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hasBackup, err := saveCoverJPEG(filepath.Join(baseDir, bookPath), img)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove the old cover's thumbnail, then give the new cover a new stamp.
	coverStamp, _ := bookCoverStamp(uuid)
	thumbPath := thumbCachePath(uuid, coverStamp)
	if err := os.Remove(thumbPath); err != nil && !os.IsNotExist(err) {
		log.Printf("upload-cover: failed to remove thumbnail cache for %s: %v", uuid, err)
	}
	coverChanged(uuid)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success":true,"uuid":%q,"hasBackup":%t}`, uuid, hasBackup)
}

// readCoverUpload decodes the image a cover request carries: either a
// multipart form with a "cover" file field (a JPG or PNG upload), or a body
// that is a base64 data URL (a page of the book, drawn by the browser).
// Its errors are the client's (400).
func readCoverUpload(r *http.Request) (image.Image, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil { // 32 MB max
			return nil, fmt.Errorf("failed to parse form: %w", err)
		}
		file, header, err := r.FormFile("cover")
		if err != nil {
			return nil, fmt.Errorf("missing cover file: %w", err)
		}
		defer file.Close()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			return nil, errors.New("unsupported image format (use JPG or PNG)")
		}
		img, _, err := image.Decode(file)
		if err != nil {
			return nil, fmt.Errorf("failed to decode image: %w", err)
		}
		return img, nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, errors.New("failed to read body")
	}
	// data:image/png;base64,iVBOR...
	dataURL := string(body)
	if !strings.HasPrefix(dataURL, "data:image/") {
		return nil, errors.New("expected a data:image/ URL")
	}
	_, b64, ok := strings.Cut(dataURL, ",")
	if !ok {
		return nil, errors.New("malformed data URL")
	}
	imgData, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(imgData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image from data URL: %w", err)
	}
	return img, nil
}

// saveCoverJPEG writes img as dir/cover.jpg. The cover it replaces is kept as
// cover_backup.jpg, so the change can be reverted (revertCoverIn).
func saveCoverJPEG(dir string, img image.Image) (hasBackup bool, err error) {
	coverPath := filepath.Join(dir, "cover.jpg")
	backupPath := filepath.Join(dir, "cover_backup.jpg")
	if _, err := os.Stat(coverPath); err == nil {
		_ = copyFile(coverPath, backupPath)
	}

	outFile, err := os.Create(coverPath)
	if err != nil {
		return false, fmt.Errorf("failed to create cover file: %w", err)
	}
	if err := jpeg.Encode(outFile, img, &jpeg.Options{Quality: 90}); err != nil {
		outFile.Close()
		os.Remove(coverPath)
		return false, fmt.Errorf("failed to encode JPEG: %w", err)
	}
	if err := outFile.Close(); err != nil {
		return false, fmt.Errorf("failed to write cover file: %w", err)
	}
	now := time.Now()
	_ = os.Chtimes(coverPath, now, now)

	_, hasBackup = coverFilesIn(dir)
	return hasBackup, nil
}

// coverFilesIn reports whether dir has a cover, and a previous one to revert to.
func coverFilesIn(dir string) (hasCover, hasBackup bool) {
	_, cErr := os.Stat(filepath.Join(dir, "cover.jpg"))
	_, bErr := os.Stat(filepath.Join(dir, "cover_backup.jpg"))
	return cErr == nil, bErr == nil
}

var errNoCoverBackup = errors.New("no previous cover to revert to")

// revertCoverIn swaps dir's cover.jpg and cover_backup.jpg, so a revert can
// itself be undone.
func revertCoverIn(dir string) error {
	coverPath := filepath.Join(dir, "cover.jpg")
	backupPath := filepath.Join(dir, "cover_backup.jpg")
	if _, err := os.Stat(backupPath); err != nil {
		return errNoCoverBackup
	}
	tempPath := filepath.Join(dir, "cover_temp.jpg")
	if _, err := os.Stat(coverPath); err == nil {
		_ = copyFile(coverPath, tempPath)
	}
	if err := copyFile(backupPath, coverPath); err != nil {
		return fmt.Errorf("failed to restore backup cover: %w", err)
	}
	if _, err := os.Stat(tempPath); err == nil {
		_ = os.Rename(tempPath, backupPath)
	}
	now := time.Now()
	_ = os.Chtimes(coverPath, now, now)
	return nil
}

// coverStatusHandler handles GET /cover-status/{uuid}
// Returns whether the book has an active cover and whether a previous backup cover exists.
func coverStatusHandler(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if uuid == "" || !isSafeUUIDComponent(uuid) {
		http.Error(w, "missing or invalid UUID", http.StatusBadRequest)
		return
	}

	bookPath, err := resolveBookPath(uuid)
	if err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}

	hasCover, hasBackup := coverFilesIn(filepath.Join(baseDir, bookPath))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"hasCover":%t,"hasBackup":%t}`, hasCover, hasBackup)
}

// revertCoverHandler handles POST /revert-cover/{uuid}
// Swaps cover.jpg with cover_backup.jpg so the user can restore their previous cover (and undo if desired).
func revertCoverHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	uuid := r.PathValue("uuid")
	if uuid == "" || !isSafeUUIDComponent(uuid) {
		http.Error(w, "missing or invalid UUID", http.StatusBadRequest)
		return
	}

	bookPath, err := resolveBookPath(uuid)
	if err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}

	if err := revertCoverIn(filepath.Join(baseDir, bookPath)); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errNoCoverBackup) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}

	// Remove the old cover's thumbnail, then give the restored cover a new stamp.
	bookStamp, _ := bookCoverStamp(uuid)
	thumbPath := thumbCachePath(uuid, bookStamp)
	if err := os.Remove(thumbPath); err != nil && !os.IsNotExist(err) {
		log.Printf("revert-cover: failed to remove thumbnail cache for %s: %v", uuid, err)
	}
	coverChanged(uuid)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success":true,"reverted":true,"hasBackup":true}`)
}
