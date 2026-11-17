package imageutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register the GIF decoder
	"image/jpeg"
	_ "image/png" // register the PNG decoder
	"io"
	"os"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // register the TIFF decoder
	_ "golang.org/x/image/webp" // register the WebP decoder
)

const (
	// pdfPageMinSide: a picture on a page smaller than this (its longer
	// side, in pixels) is a logo or an ornament, not the page.
	pdfPageMinSide = 400
	// pdfPageAspectSlack: how far (as a fraction) a picture's shape may be
	// from the page's and still be taken as the whole page.
	pdfPageAspectSlack = 0.08
)

// ResizeToWidth scales img to width pixels wide, keeping its shape (at
// least 1 pixel high), with Catmull-Rom resampling.
func ResizeToWidth(img image.Image, width int) image.Image {
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

// Thumb returns a JPEG thumbnail of an image, width pixels wide at JPEG
// quality, never larger than the original: images already at or below
// width, or whose thumbnail would be more bytes than the original, are
// returned unchanged. abort (if not nil) is checked between decoding,
// scaling and encoding; its error stops the work.
func Thumb(orig []byte, width, quality int, abort func() error) ([]byte, error) {
	if abort == nil {
		abort = func() error { return nil }
	}
	img, _, err := image.Decode(bytes.NewReader(orig))
	if err != nil {
		return nil, err
	}
	if img.Bounds().Dx() <= width {
		return orig, nil
	}
	if err := abort(); err != nil {
		return nil, err
	}
	small := ResizeToWidth(img, width)
	if err := abort(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	if buf.Len() >= len(orig) {
		return orig, nil
	}
	return buf.Bytes(), nil
}

// CoverOptions configure CoverJPEG.
type CoverOptions struct {
	MaxWidth     int // a wider image is scaled down to this
	KeepMaxBytes int // a JPEG up to this size (and MaxWidth wide) is kept byte for byte
	Quality      int // JPEG quality when the image has to be encoded
}

// CoverJPEG turns an image file's bytes into a JPEG cover. A JPEG at most
// MaxWidth wide and KeepMaxBytes big is kept as it is: re-encoding it would
// only lose quality. Anything else (GIF, PNG, TIFF, WebP, or a JPEG too wide
// or too big) is decoded, scaled down to MaxWidth if wider, and encoded once
// at Quality. Its errors are about the image.
func CoverJPEG(data []byte, opts CoverOptions) ([]byte, error) {
	// Decoded either way, so a kept file is known to be a whole image.
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not an image that can be read (use JPG or PNG): %w", err)
	}
	if format == "jpeg" && img.Bounds().Dx() <= opts.MaxWidth && len(data) <= opts.KeepMaxBytes {
		return data, nil
	}
	if img.Bounds().Dx() > opts.MaxWidth {
		img = ResizeToWidth(img, opts.MaxWidth)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: opts.Quality}); err != nil {
		return nil, fmt.Errorf("encoding the cover: %w", err)
	}
	return buf.Bytes(), nil
}

// PDFFirstPageImage is the picture that is a PDF's first page, as stored in
// the PDF (often a JPEG): the largest image on it, when it's big and has the
// page's shape (PDFImageIsPage). Pictures over maxBytes are ignored. Pages
// drawn as text and vectors can't be rendered: that's an error.
func PDFFirstPageImage(ctx context.Context, p string, maxBytes int64) ([]byte, error) {
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
			data, err := io.ReadAll(io.LimitReader(im, maxBytes+1))
			if err != nil || int64(len(data)) > maxBytes {
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
	if !PDFImageIsPage(bestW, bestH, dims[0].Width, dims[0].Height) {
		return nil, fmt.Errorf("the first page's largest picture (%dx%d) isn't the whole page", bestW, bestH)
	}
	return best, nil
}

// PDFImageIsPage reports whether a picture of w×h pixels is plausibly the
// whole of a page of pageW×pageH points: big enough, and the page's shape
// (either way round, for a page turned with /Rotate).
func PDFImageIsPage(w, h int, pageW, pageH float64) bool {
	if w <= 0 || h <= 0 || max(w, h) < pdfPageMinSide {
		return false
	}
	ratio := float64(w) / float64(h)
	for _, page := range []float64{pageW / pageH, pageH / pageW} {
		if d := ratio/page - 1; d > -pdfPageAspectSlack && d < pdfPageAspectSlack {
			return true
		}
	}
	return false
}
