# imageutil

`import "github.com/iotku/CalibreFast/pkg/imageutil"`

Cover and thumbnail helpers for ebook libraries.

| Function | What it does |
| --- | --- |
| `ResizeToWidth(img, w)` | Scales to `w` pixels wide, keeping the shape (Catmull-Rom; quick halvings first when much wider). |
| `Thumb(data, width, quality, abort)` | A JPEG thumbnail, never larger than the original: images already narrow enough, or whose thumbnail would be more bytes, come back unchanged. `abort` is checked between decoding, scaling and encoding, so a server can stop work for a request that went away. |
| `CoverJPEG(data, CoverOptions{MaxWidth, KeepMaxBytes, Quality})` | A JPEG cover from any supported image. A JPEG that is already narrow and small enough is kept byte for byte (re-encoding only loses quality); anything else is decoded, scaled down if too wide, and encoded once. |
| `PDFFirstPageImage(ctx, path, maxBytes)` | The picture that *is* a PDF's first page (a scan, or a cover placed on the page), as stored in the PDF. Uses [pdfcpu](https://github.com/pdfcpu/pdfcpu). |
| `PDFImageIsPage(w, h, pageW, pageH)` | Whether a w×h-pixel picture is plausibly a whole page of pageW×pageH points: at least 400 px on its long side, and within 8% of the page's shape (either way round). |

Importing the package registers JPEG, PNG, GIF, TIFF and WebP decoders.

## Usage

```go
cover, err := imageutil.CoverJPEG(uploaded, imageutil.CoverOptions{MaxWidth: 2560, KeepMaxBytes: 5 << 20, Quality: 92})

thumb, err := imageutil.Thumb(cover, 400, 80, func() error { return r.Context().Err() })

ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
defer cancel()
page, err := imageutil.PDFFirstPageImage(ctx, "book.pdf", 32<<20)
```

## Limits

- `PDFFirstPageImage` doesn't render pages: a first page drawn with text and
  vectors is an error. Images Go can't decode (JPEG 2000, CMYK TIFF, …) are
  skipped.
- Output is always JPEG (alpha is flattened onto black by the encoder's
  RGBA conversion, as `image/jpeg` does).
