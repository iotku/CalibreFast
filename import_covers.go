package main

// Covers for books in the import queue, taken from their own files when they
// are staged: the cover an EPUB declares, else a PDF's first page when that
// page is a single picture (a scan, or a cover image placed on the page).
// With one, the queue shows it and the review page doesn't open the book in
// the browser to pick a default (web/cover-dialog.ts). Anything harder (a
// PDF page of text and vectors, an EPUB without a declared cover) is left to
// the review page, which renders pages itself.

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	// importCoverMaxWidth: covers are saved at most this wide, as the
	// browser saves them (SAVE_MAX_WIDTH in web/cover-sources.ts).
	importCoverMaxWidth = 1200
	// importCoverMaxBytes bounds the image file read from a book.
	importCoverMaxBytes = 32 << 20
	// importCoverTimeout bounds reading one PDF for its first page.
	importCoverTimeout = 30 * time.Second
	// pdfCoverMinSide: a picture on a first page smaller than this (its
	// longer side, in pixels) is a logo or an ornament, not the page.
	pdfCoverMinSide = 400
	// pdfCoverAspectSlack: how far (as a fraction) a picture's shape may be
	// from the page's and still be taken as the whole page.
	pdfCoverAspectSlack = 0.08
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
			var img image.Image
			var err error
			if format == "epub" {
				img, err = epubCoverImage(p)
			} else {
				img, err = pdfFirstPageImage(p)
			}
			if err != nil {
				log.Printf("imports: no cover from %s: %v", f.Name, err)
				continue
			}
			if err := writeNewCover(dir, img); err != nil {
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

// writeNewCover saves img as dir/cover.jpg, scaled down to
// importCoverMaxWidth, only if there is no cover.jpg yet: one the admin
// chose meanwhile is never replaced (os.ErrExist).
func writeNewCover(dir string, img image.Image) error {
	if img.Bounds().Dx() > importCoverMaxWidth {
		img = resizeToWidth(img, importCoverMaxWidth)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return err
	}
	p := filepath.Join(dir, "cover.jpg")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
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

// ---- EPUB --------------------------------------------------------------------

// epubManifest is the part of an EPUB's package document that names its cover.
type epubManifest struct {
	Metas []struct {
		Name    string `xml:"name,attr"`
		Content string `xml:"content,attr"`
	} `xml:"metadata>meta"`
	Items []struct {
		ID         string `xml:"id,attr"`
		Href       string `xml:"href,attr"`
		MediaType  string `xml:"media-type,attr"`
		Properties string `xml:"properties,attr"`
	} `xml:"manifest>item"`
}

// epubCoverHref is the zip path of the cover image an EPUB declares: EPUB 3's
// cover-image item, else EPUB 2's <meta name="cover">, else an image item
// whose id is "cover" or "cover-image".
func epubCoverHref(opfPath string, opfData []byte) (string, error) {
	var m epubManifest
	if err := xmlUnmarshal(opfData, &m); err != nil {
		return "", fmt.Errorf("parsing the OPF: %w", err)
	}
	isImage := func(mt string) bool { return strings.HasPrefix(strings.ToLower(mt), "image/") }
	href := ""
	for _, it := range m.Items {
		if isImage(it.MediaType) && strings.Contains(" "+it.Properties+" ", " cover-image ") {
			href = it.Href
			break
		}
	}
	if href == "" {
		coverID := ""
		for _, meta := range m.Metas {
			if strings.EqualFold(meta.Name, "cover") {
				coverID = meta.Content
				break
			}
		}
	ids:
		for _, id := range []string{coverID, "cover", "cover-image"} {
			if id == "" {
				continue
			}
			for _, it := range m.Items {
				if it.ID == id && isImage(it.MediaType) {
					href = it.Href
					break ids
				}
			}
		}
	}
	if href == "" {
		return "", errors.New("the EPUB declares no cover")
	}
	if i := strings.IndexAny(href, "?#"); i >= 0 {
		href = href[:i]
	}
	href = strings.ReplaceAll(href, "%20", " ")
	return path.Clean(path.Join(path.Dir(opfPath), href)), nil
}

// epubCoverImage is the cover image an EPUB file declares.
func epubCoverImage(p string) (image.Image, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	opfPath, err := epubPackagePath(&zr.Reader)
	if err != nil {
		return nil, err
	}
	opfData, err := readZipEntry(&zr.Reader, opfPath, 16<<20)
	if err != nil {
		return nil, err
	}
	href, err := epubCoverHref(opfPath, opfData)
	if err != nil {
		return nil, err
	}
	data, err := readZipEntry(&zr.Reader, href, importCoverMaxBytes)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", href, err)
	}
	return img, nil
}

// ---- PDF ---------------------------------------------------------------------

// pdfFirstPageImage is the picture that is a PDF's first page: the largest
// image on it, when it's big and has the page's shape. Pages drawn as text
// and vectors can't be rendered here; the review page does those.
func pdfFirstPageImage(p string) (image.Image, error) {
	ctx, cancel := context.WithTimeout(context.Background(), importCoverTimeout)
	defer cancel()
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	conf := model.NewDefaultConfiguration()
	dims, err := pdfapi.PageDims(ctx, f, conf)
	if err != nil {
		return nil, fmt.Errorf("reading the pages: %w", err)
	}
	if len(dims) == 0 || dims[0].Width <= 0 || dims[0].Height <= 0 {
		return nil, errors.New("no first page")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	pages, err := pdfapi.ExtractImagesRaw(ctx, f, []string{"1"}, conf)
	if err != nil && len(pages) == 0 {
		return nil, fmt.Errorf("reading the first page's pictures: %w", err)
	}
	// pdfcpu doesn't give an extracted picture's size: read each one's.
	var best []byte
	bestW, bestH := 0, 0
	for _, page := range pages {
		for _, im := range page {
			if im.IsImgMask || im.Thumb || im.Reader == nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(im, importCoverMaxBytes+1))
			if err != nil || len(data) > importCoverMaxBytes {
				continue
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				continue // a kind Go can't decode (JPEG 2000, CMYK TIFF…)
			}
			if cfg.Width*cfg.Height > bestW*bestH {
				best, bestW, bestH = data, cfg.Width, cfg.Height
			}
		}
	}
	if best == nil {
		return nil, errors.New("the first page has no picture that can be read")
	}
	if !pdfImageIsPage(bestW, bestH, dims[0].Width, dims[0].Height) {
		return nil, fmt.Errorf("the first page's largest picture (%dx%d) isn't the whole page", bestW, bestH)
	}
	img, _, err := image.Decode(bytes.NewReader(best))
	if err != nil {
		return nil, fmt.Errorf("decoding the first page's picture: %w", err)
	}
	return img, nil
}

// pdfImageIsPage reports whether a picture of w×h pixels is plausibly the
// whole of a page of pageW×pageH points: big enough, and the page's shape
// (either way round, for a page turned with /Rotate).
func pdfImageIsPage(w, h int, pageW, pageH float64) bool {
	if w <= 0 || h <= 0 || max(w, h) < pdfCoverMinSide {
		return false
	}
	ratio := float64(w) / float64(h)
	for _, page := range []float64{pageW / pageH, pageH / pageW} {
		if d := ratio/page - 1; d > -pdfCoverAspectSlack && d < pdfCoverAspectSlack {
			return true
		}
	}
	return false
}
