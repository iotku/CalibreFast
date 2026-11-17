package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/imageutil"
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

	data, err := readCoverUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hasBackup, err := saveCoverJPEG(filepath.Join(baseDir, bookPath), data)
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

const (
	// coverMaxWidth: a wider cover is scaled down to this. Narrower ones
	// keep their size: thumbnails are made from the cover when needed.
	coverMaxWidth = 2560
	// coverKeepMaxBytes: a JPEG cover up to this size (and coverMaxWidth
	// wide) is saved byte for byte, never re-compressed.
	coverKeepMaxBytes = 5 << 20
	// coverQuality is the JPEG quality of a cover that has to be encoded
	// (another format, too wide, or too big).
	coverQuality = 92
	// coverUploadMaxBytes bounds a cover request.
	coverUploadMaxBytes = 32 << 20
)

// coverJPEG is the cover.jpg to save for an image file's bytes
// (imageutil.CoverJPEG): a JPEG at most coverMaxWidth wide and
// coverKeepMaxBytes big is kept as it is; anything else is decoded, scaled
// down to coverMaxWidth if wider, and encoded once at coverQuality. Its
// errors are about the image (the client's, for an upload).
func coverJPEG(data []byte) ([]byte, error) {
	return imageutil.CoverJPEG(data, imageutil.CoverOptions{
		MaxWidth:     coverMaxWidth,
		KeepMaxBytes: coverKeepMaxBytes,
		Quality:      coverQuality,
	})
}

// readCoverUpload reads the cover a request carries, as cover.jpg's bytes
// (coverJPEG): either a multipart form with a "cover" file field (an
// uploaded image, or one from the book or a lookup, as it is), or a body
// that is a base64 data URL (a page of the book, drawn by the browser).
// Its errors are the client's (400).
func readCoverUpload(r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(nil, r.Body, coverUploadMaxBytes+1<<20)
		if err := r.ParseMultipartForm(coverUploadMaxBytes); err != nil {
			return nil, fmt.Errorf("failed to parse form: %w", err)
		}
		data, err := formImageFile(r, "cover")
		if err != nil {
			return nil, err
		}
		return coverJPEG(data)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, coverUploadMaxBytes))
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
	return coverJPEG(imgData)
}

// formImageFile reads the image file in a parsed multipart form's field,
// up to coverUploadMaxBytes. A missing file is an error wrapping
// http.ErrMissingFile. Its errors are the client's (400).
func formImageFile(r *http.Request, field string) ([]byte, error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		return nil, fmt.Errorf("missing %s file: %w", field, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, coverUploadMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read the %s file", field)
	}
	if len(data) > coverUploadMaxBytes {
		return nil, fmt.Errorf("the %s file is too big", field)
	}
	return data, nil
}

// saveCoverJPEG writes data (from coverJPEG) as dir/cover.jpg. The cover it
// replaces is kept as cover_backup.jpg, so the change can be reverted
// (revertCoverIn).
func saveCoverJPEG(dir string, data []byte) (hasBackup bool, err error) {
	coverPath := filepath.Join(dir, "cover.jpg")
	backupPath := filepath.Join(dir, "cover_backup.jpg")
	if _, err := os.Stat(coverPath); err == nil {
		_ = copyFile(coverPath, backupPath)
	}

	outFile, err := os.Create(coverPath)
	if err != nil {
		return false, fmt.Errorf("failed to create cover file: %w", err)
	}
	if _, err := outFile.Write(data); err != nil {
		outFile.Close()
		os.Remove(coverPath)
		return false, fmt.Errorf("failed to write cover file: %w", err)
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
