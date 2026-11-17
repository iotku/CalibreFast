package main

// Covers for books in the import queue, taken from their own files when they
// are staged: the cover an EPUB declares, else a PDF's first page when that
// page is a single picture (a scan, or a cover image placed on the page).
// With one, the queue shows it and the review page doesn't open the book in
// the browser to pick a default (web/cover-dialog.ts). Anything harder (a
// PDF page of text and vectors, an EPUB without a declared cover) is left to
// the review page, which renders pages itself.

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/iotku/CalibreFast/pkg/epubzip"
	"github.com/iotku/CalibreFast/pkg/imageutil"
)

const (
	// importCoverMaxBytes bounds the image file read from a book.
	importCoverMaxBytes = 32 << 20
	// importCoverTimeout bounds reading one PDF for its first page.
	importCoverTimeout = 30 * time.Second
)

// extractImportCover saves a cover for the waiting book in dir from its
// files, unless it has one. EPUBs are tried first, as the browser does.
// It reports whether dir has a cover now.
func extractImportCover(dir string, files []importFile) bool {
	if hasCover, _ := coverFilesIn(dir); hasCover {
		return true
	}
	for _, format := range []string{"epub", "pdf"} {
		for _, f := range files {
			if f.Format != format {
				continue
			}
			p := filepath.Join(dir, f.Name)
			var data []byte
			var err error
			if format == "epub" {
				data, err = epubzip.CoverData(p, importCoverMaxBytes)
			} else {
				data, err = pdfFirstPageData(p)
			}
			if err == nil {
				data, err = coverJPEG(data)
			}
			if err != nil {
				log.Printf("imports: no cover from %s: %v", f.Name, err)
				continue
			}
			if err := writeNewCover(dir, data); err != nil {
				if errors.Is(err, os.ErrExist) {
					return true // chosen meanwhile, on the review page
				}
				log.Printf("imports: couldn't save the cover from %s: %v", f.Name, err)
				continue
			}
			return true
		}
	}
	return false
}

// backfillImportCovers gives books already waiting (staged before covers
// were taken from their files) a cover, one at a time.
func backfillImportCovers(b *importInbox) {
	for _, it := range b.list() {
		extractImportCover(b.itemDir(it.ID), it.Files)
	}
}

// writeNewCover saves data (from coverJPEG) as dir/cover.jpg, only if there
// is no cover.jpg yet: one the admin chose meanwhile is never replaced
// (os.ErrExist).
func writeNewCover(dir string, data []byte) error {
	p := filepath.Join(dir, "cover.jpg")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(p)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(p)
		return err
	}
	return nil
}

// pdfFirstPageData is the picture that is a PDF's first page, as stored in
// the PDF (imageutil.PDFFirstPageImage). Pages drawn as text and vectors
// can't be rendered here; the review page does those.
func pdfFirstPageData(p string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), importCoverTimeout)
	defer cancel()
	return imageutil.PDFFirstPageImage(ctx, p, importCoverMaxBytes)
}
