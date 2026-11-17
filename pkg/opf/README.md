# opf

`import "github.com/iotku/CalibreFast/pkg/opf"`

Reads and writes the metadata section of OPF package documents: Calibre's
per-book `metadata.opf` and the package document inside an EPUB.

## What it handles

- Dublin Core: `title`, `creator` (several), `description`, `publisher`,
  `date`, `language` (several, in order, first is primary), `identifier`
  (with `opf:scheme` and `id`), `subject`.
- `<meta>` in both EPUB 2 (`name`/`content`) and EPUB 3
  (`property`/`refines`/text) form, kept as they are on a round trip.
- Series: `Load` fills `Metadata.Series`/`SeriesIndex` from
  `calibre:series`/`calibre:series_index`, or from EPUB 3's
  `belongs-to-collection` (+ `collection-type`, `group-position`).
  `SyncSeriesMeta` writes them back as Calibre's meta and drops the EPUB 3
  form, so the file says one thing.
- Identifiers: `Write` puts `id="uuid_id"` (which `unique-identifier` points
  at) on the book's uuid identifier only, adding a Calibre-style
  `<dc:identifier opf:scheme="uuid">` when the file has none.
  `IsLibraryIdentifier`/`SplitLibraryIdentifiers` separate Calibre's
  bookkeeping identifiers (`calibre`, `uuid`, `urn:uuid:…`) from ISBNs and
  the like.
- XML 1.1 documents (via `pkg/xml11`).

Helpers: `CleanLanguages`, `ParseLanguages`, `CleanSeries`,
`FormatSeriesIndex`, `ParseAnyDate` (OPF, Calibre, PDF and written dates),
`UniqueTags`.

## Usage

```go
pkg, err := opf.Load("Author/Title (12)/metadata.opf")
if err != nil { ... }
fmt.Println(pkg.Metadata.Title, pkg.Metadata.Series, pkg.Metadata.SeriesIndex)

pkg.Metadata.Series, pkg.Metadata.SeriesIndex = "Dune", "2"
pkg.Metadata.SyncSeriesMeta()
err = opf.Write(pkg, "metadata.opf", bookUUID) // bookUUID may be ""
```

`Metadata` has JSON tags (only `Series*` fields are named explicitly); the
CalibreFast editor sends and receives it as is.

## Limits

- Only metadata is modelled; the manifest, spine and guide are not read and
  are not written. `Write` produces a package with a `<metadata>` only, which
  is what Calibre's `metadata.opf` is. Don't use it to rewrite an EPUB's
  package document.
- `dc:creator` roles and `file-as`, `dc:contributor`, `dc:rights` and other
  elements not listed above are dropped on a round trip.
- Self-closing `<meta/>` elements are written as `<meta ...></meta>`.
