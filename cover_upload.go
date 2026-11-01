package main

import (
	"bytes"
	"encoding/base64"
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

// uploadCoverHandler handles POST /upload-cover/{uuid}
// It accepts either:
//   - A multipart form with a "cover" file field (JPG/PNG image upload)
//   - A JSON body with a base64 data URL in "dataUrl" field (page screenshot)
//
// The image is decoded, re-encoded as JPEG, and saved as cover.jpg in the book directory.
// The thumbnail cache is invalidated and has_cover is set to 1 in the database.
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

	var img image.Image

	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// Image file upload
		if err := r.ParseMultipartForm(32 << 20); err != nil { // 32 MB max
			http.Error(w, "failed to parse form: "+err.Error(), http.StatusBadRequest)
			return
		}

		file, header, err := r.FormFile("cover")
		if err != nil {
			http.Error(w, "missing cover file: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		ext := strings.ToLower(filepath.Ext(header.Filename))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			http.Error(w, "unsupported image format (use JPG or PNG)", http.StatusBadRequest)
			return
		}

		img, _, err = image.Decode(file)
		if err != nil {
			http.Error(w, "failed to decode image: "+err.Error(), http.StatusBadRequest)
			return
		}

	} else {
		// Base64 data URL from page screenshot
		body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		dataUrl := string(body)

		// Parse data URL: data:image/png;base64,iVBOR...
		if !strings.HasPrefix(dataUrl, "data:image/") {
			http.Error(w, "expected a data:image/ URL", http.StatusBadRequest)
			return
		}

		commaIdx := strings.Index(dataUrl, ",")
		if commaIdx < 0 {
			http.Error(w, "malformed data URL", http.StatusBadRequest)
			return
		}

		b64Data := dataUrl[commaIdx+1:]
		imgData, err := base64.StdEncoding.DecodeString(b64Data)
		if err != nil {
			http.Error(w, "failed to decode base64: "+err.Error(), http.StatusBadRequest)
			return
		}

		img, _, err = image.Decode(bytes.NewReader(imgData))
		if err != nil {
			http.Error(w, "failed to decode image from data URL: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Save as cover.jpg
	destDir := filepath.Join(baseDir, bookPath)
	coverPath := filepath.Join(destDir, "cover.jpg")
	backupPath := filepath.Join(destDir, "cover_backup.jpg")

	// If cover.jpg already exists, back it up to cover_backup.jpg so it can be reverted
	if _, err := os.Stat(coverPath); err == nil {
		_ = copyFile(coverPath, backupPath)
	}

	outFile, err := os.Create(coverPath)
	if err != nil {
		http.Error(w, "failed to create cover file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := jpeg.Encode(outFile, img, &jpeg.Options{Quality: 90}); err != nil {
		outFile.Close()
		os.Remove(coverPath)
		http.Error(w, "failed to encode JPEG: "+err.Error(), http.StatusInternalServerError)
		return
	}
	outFile.Close()

	now := time.Now()
	_ = os.Chtimes(coverPath, now, now)

	// Update has_cover in the database
	if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1 WHERE uuid = ?`, uuid); err != nil {
		log.Printf("upload-cover: failed to set has_cover for %s: %v", uuid, err)
	}

	// Invalidate thumbnail cache
	thumbPath := filepath.Join(cacheDir, "covers", "thumb", uuid+".jpg")
	if err := os.Remove(thumbPath); err != nil && !os.IsNotExist(err) {
		log.Printf("upload-cover: failed to remove thumbnail cache for %s: %v", uuid, err)
	}

	hasBackup := false
	if _, err := os.Stat(backupPath); err == nil {
		hasBackup = true
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success":true,"uuid":%q,"hasBackup":%t}`, uuid, hasBackup)
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

	destDir := filepath.Join(baseDir, bookPath)
	coverPath := filepath.Join(destDir, "cover.jpg")
	backupPath := filepath.Join(destDir, "cover_backup.jpg")

	hasCover := false
	if _, err := os.Stat(coverPath); err == nil {
		hasCover = true
	}

	hasBackup := false
	if _, err := os.Stat(backupPath); err == nil {
		hasBackup = true
	}

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

	destDir := filepath.Join(baseDir, bookPath)
	coverPath := filepath.Join(destDir, "cover.jpg")
	backupPath := filepath.Join(destDir, "cover_backup.jpg")

	if _, err := os.Stat(backupPath); err != nil {
		http.Error(w, "no previous cover to revert to", http.StatusBadRequest)
		return
	}

	// Swap current cover and backup so user can undo if desired
	tempPath := filepath.Join(destDir, "cover_temp.jpg")
	if _, err := os.Stat(coverPath); err == nil {
		_ = copyFile(coverPath, tempPath)
	}
	if err := copyFile(backupPath, coverPath); err != nil {
		http.Error(w, "failed to restore backup cover: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := os.Stat(tempPath); err == nil {
		_ = os.Rename(tempPath, backupPath)
	}

	now := time.Now()
	_ = os.Chtimes(coverPath, now, now)

	// Invalidate thumbnail cache
	thumbPath := filepath.Join(cacheDir, "covers", "thumb", uuid+".jpg")
	if err := os.Remove(thumbPath); err != nil && !os.IsNotExist(err) {
		log.Printf("revert-cover: failed to remove thumbnail cache for %s: %v", uuid, err)
	}

	if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1 WHERE uuid = ?`, uuid); err != nil {
		log.Printf("revert-cover: failed to set has_cover for %s: %v", uuid, err)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"success":true,"reverted":true,"hasBackup":true}`)
}
