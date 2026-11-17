# epubzip

`import "github.com/iotku/CalibreFast/pkg/epubzip"`

Small helpers for reading inside an EPUB's zip archive.

| Function | What it does |
| --- | --- |
| `PackagePath(zr)` | Path of the package document (OPF): `META-INF/container.xml`'s first rootfile, else the first `*.opf` in the archive. |
| `ReadEntry(zr, name, max)` | Reads one entry, refusing entries larger than `max` bytes (so a hostile archive can't make you allocate gigabytes). |
| `HrefPath(opfPath, href)` | Resolves a manifest `href` (percent-decoded, fragment dropped) against the OPF's directory. |
| `CoverHref(opfPath, opfData)` | The zip path of the declared cover image: EPUB 3's `cover-image` item, else EPUB 2's `<meta name="cover">`, else an image item with id `cover` or `cover-image`. |
| `CoverData(path, max)` | Opens an EPUB file and returns its cover image bytes. |

## Usage

```go
zr, err := zip.OpenReader("book.epub")
if err != nil { ... }
defer zr.Close()

opfPath, err := epubzip.PackagePath(&zr.Reader)
opf, err := epubzip.ReadEntry(&zr.Reader, opfPath, 16<<20)
href, err := epubzip.CoverHref(opfPath, opf)
cover, err := epubzip.ReadEntry(&zr.Reader, href, 32<<20)

// or simply
cover, err := epubzip.CoverData("book.epub", 32<<20)
```

## Limits

- Only the first rootfile in `container.xml` is used.
- `CoverHref` doesn't guess: a book that declares no cover gets an error, even
  if it has an image called `cover.jpg`.
- Image data is returned as stored; nothing is decoded or validated.
