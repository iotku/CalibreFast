# mobi

`import "github.com/iotku/CalibreFast/pkg/mobi"`

Reads Kindle books (MOBI 6, AZW, AZW3/KF8, and joint MOBI+KF8 files) and
converts them to EPUB 3. Pure Go: the only dependencies are
`golang.org/x/net/html` and `golang.org/x/text`.

| Function | What it does |
| --- | --- |
| `Open(data)` / `OpenLimits(data, lim)` | Parses a book from its bytes. `ErrNotMobi` for anything else, `ErrDRM` for an encrypted book. |
| `OpenFile(path, lim)` | Reads and parses a file, refusing one over `Limits.MaxFile`. |
| `(*Book).Metadata` | Title, authors, publisher, description, ISBN, ASIN, subjects, date, language and page direction from the MOBI and EXTH headers. |
| `(*Book).KF8` | Whether the book is read as KF8 (AZW3, or a joint file's KF8 half). |
| `(*Book).WriteEPUB(w)` | Writes the book as an EPUB 3 with an EPUB 2 NCX too. The output is deterministic: the same book always gives the same bytes. |
| `ConvertFile(src, dst, lim)` | Converts a file, writing `dst` atomically (temp file and rename). |

## Usage

```go
b, err := mobi.OpenFile("book.azw3", mobi.Limits{})
switch {
case errors.Is(err, mobi.ErrDRM):
	// can't be read
case err != nil:
	...
}
fmt.Println(b.Metadata.Title)
f, _ := os.Create("book.epub")
defer f.Close()
err = b.WriteEPUB(f)

// or simply
err = mobi.ConvertFile("book.azw3", "book.epub", mobi.Limits{})
```

## What the EPUB contains

- **KF8:** each skeleton (with its fragments put back in) becomes a
  `partNNNN.xhtml`. Flows become `flowNNNN.css` / `flowNNNN.svg`.
  `kindle:pos:fid` links become `file#id`. When the target element has only
  an `aid`, it gets `id="aid-…"`. `kindle:embed` and `kindle:flow` URLs in
  markup and CSS point at the extracted files.
- **MOBI 6:** the single HTML stream is split at `<mbp:pagebreak>`. An anchor
  `filepos<N>` goes at every position a `filepos` link or the NCX names.
  `recindex` images become `src`.
- Images (JPEG, PNG, GIF, WebP) are copied as they are. Fonts are
  de-obfuscated and inflated.
- The table of contents comes from the NCX index, with KF8's parent and child
  links giving the nesting. Without an NCX, the TOC is one entry for the
  start.
- The cover is EXTH 201 (or 129) and is marked `cover-image`.
- The identifier is a name-based UUID of the file's bytes.

## Hostile input

Book files are untrusted. Every offset, count and length in them is checked.
The limits below are on by default, and `Limits` can lower or raise them.

- The file (`OpenFile`): 512 MiB.
- Decompressed text: 128 MiB. PalmDOC output is capped per call. HUFF/CDIC
  caps each record and phrase at 1 MiB, caps the expansion of all phrases
  together, and rejects phrase cycles.
- Each image or font, after inflating: 32 MiB.
- Index records, entries and TOC depth.

Markup is parsed with an HTML5 parser and rewritten as well-formed XHTML.
Several things are dropped on the way:

- `script`, `iframe`, `object`, `embed`, `form` and similar elements.
- SVG `script` and animation elements.
- `on*` attributes.
- `meta` and `base`.
- Any URL whose scheme isn't `http`, `https` or `mailto` (relative links
  stay).
- Characters XML can't hold.

Reading the output still needs the same care as any EPUB, such as CalibreFast's
script-free sandboxed reader.

## Limits

- DRM-protected books are refused, not decrypted.
- HUFF/CDIC is implemented from the format description and unit tested, but
  it hasn't been tested against a store book. Calibre writes only PalmDOC.
- KF8 fragment tables with a wrong insert position aren't repaired, only
  clamped. KindleUnpack repairs them by searching for the `aid`.
- Print Replica (`.azw4`, a PDF inside) and Kindle dictionaries aren't
  handled specially.
- The KF8 guide index and the page-number map (`PAGE`) aren't
  used.

## Tests

`testdata/` has a small source EPUB and Calibre's conversions of it to AZW3,
MOBI 6 and joint MOBI. See `testdata/README.md` to regenerate them. Run
`go test -fuzz FuzzConvert` to fuzz the whole reader.
