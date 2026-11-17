# koreader

`import "github.com/iotku/CalibreFast/pkg/koreader"`

Converts what [KOReader](https://koreader.rocks) stores about a book —
reading positions and highlights — to and from the forms a web reader
uses, and reads and writes KOReader's Lua data files.

## Positions: XPointer ↔ EPUB CFI

KOReader's engine, crengine, points into an EPUB with an XPointer into its
own DOM, which wraps each spine item in a `<DocFragment>`:

```
/body/DocFragment[11]/body/div/p[7]/text().123
```

Web readers (epub.js) use an EPUB CFI:

```
epubcfi(/6/22!/4/2/14/1:123)
```

```go
cfi, err := koreader.XPointerToCFI("book.epub", "/body/DocFragment[11]/body/div/p[7]/text().123")
xp, err := koreader.CFIToXPointer("book.epub", "epubcfi(/6/22!/4/2/14)")
```

`DocFragment[N]` is the N-th spine item that is in the manifest (crengine
makes no fragment for an itemref it can't find), so the chapter always
converts. Inside it, the element path is followed in the chapter's XHTML;
when every step resolves, the position converts to that element (usually a
paragraph), otherwise to the start of the chapter. Character offsets are
dropped here: this is for "where was I", where the paragraph is enough.

## Highlights: KOReader's annotations file

Since 2025 KOReader can move annotations between devices with a file,
`<book file name>.annotations.lua`, next to the book's settings or in a folder
the user picks. It writes one with *Export annotations* (or on closing a
book), and when a book is opened it merges such a file in and deletes it.
The file is a Lua table:

```lua
return {
    ["annotations"] = {
        [1] = {
            ["chapter"] = "Two",
            ["color"] = "red",
            ["datetime"] = "2025-03-01 10:00:00",        -- local time
            ["datetime_updated"] = "2025-03-02 08:00:00",
            ["drawer"] = "lighten",                    -- none: a page bookmark
            ["epubcfi"] = "epubcfi(/6/4!/4/2/2,/1:8,/1:19)", -- newer versions
            ["note"] = "…",
            ["page"] = "/body/DocFragment[2]/body/div/p[1]/text().8",
            ["pageno"] = 4,
            ["pos0"] = "/body/DocFragment[2]/body/div/p[1]/text().8",
            ["pos1"] = "/body/DocFragment[2]/body/div/p[1]/text().19",
            ["text"] = "the highlighted text",
        },
    },
    ["datetime"] = "1970-01-01 00:00:00",
    ["device_id"] = "…",                                -- KOReader skips its own
    ["paging"] = true,                                  -- PDFs and other paged books only
}
```

For PDFs, `page` is the page number, `pos0`/`pos1` are `{page, x, y, zoom,
rotation}` points inside the first and last word, and `pboxes` lists the
highlighted rectangles `{x, y, w, h}` in points from the page's top left.

```go
// Export
lua, skipped := koreader.File("book.epub", "epub", highlights, koreader.Options{DeviceID: "my-server", Source: "MyApp"})

// Import
tab, err := koreader.ParseLuaTable(string(data))
highlights, res, err := koreader.Annotations("book.epub", "epub", tab, koreader.Options{MaxTextLen: 4000})
```

A `Highlight`'s `Anchor` is an EPUB CFI range for EPUBs, or JSON
`{"page": 3, "rects": [[x0, y0, x1, y1], …]}` for PDFs.

Things to know:

- **Character positions differ.** A CFI counts UTF-16 units in the text as
  written; an XPointer counts characters in the text as crengine stores it
  (line breaks and tabs become spaces, runs of spaces collapse, white space
  that opens a block is dropped). The package models crengine's rules
  (lvxml.cpp `PreProcessXmlString`, lvtinydom.cpp
  `ldomElementWriter::onText`). When KOReader's XPointers don't land on the
  highlighted text, the text is searched for near where they point.
- **Imports are idempotent.** A highlight's id is derived from KOReader's own
  identity for it (its place and creation date), prefixed `ko-`, so importing
  the same file twice gives the same ids. Merging (what's new, what's
  updated) is up to the caller; `ImportResult` has fields for that.
- **Exports only add.** `File` dates the file `1970-01-01 00:00:00`
  (`Oldest`). On import KOReader deletes its own annotations that are older
  than the file and missing from it; nothing is older, so nothing is deleted.
- Highlights whose place can't be converted are skipped and counted.

## Lua files

`ParseLuaTable` reads `return { … }` files as data: tables, strings (all of
Lua's escapes), numbers (including hex), booleans and nil, with comments.
Anything else — a function call, an operator, a variable — is an error;
nothing is executed. Depth and size are bounded. `WriteLua` writes a value
the way KOReader's dump does: one entry per line, four spaces per level,
array entries as `[n] = …`, keys sorted, strings quoted as Lua's `%q`.

## Testing

`go test ./pkg/koreader` runs on built-in fixtures (also available to other
packages' tests from `pkg/koreader/koreadertest`). Two optional checks use
real KOReader data:

- `KOREADER_TEST_DATA=<checkout of koreader/test-data>` converts a position
  in `leaves.epub` both ways.
- `KOREADER_ANNOTATIONS=<folder with book.epub and book.epub.annotations.lua>`
  imports a file exported by KOReader and checks our CFIs against the ones
  newer KOReader versions save.
