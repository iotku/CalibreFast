# isbn

`import "github.com/iotku/CalibreFast/pkg/isbn"`

Finds ISBNs in ebook files, checks them, and ranks them so the ebook's own
ISBN comes first.

## Checking and normalising

```go
isbn.Normalize("ISBN 0-306-40615-2")     // "9780306406157"
isbn.Normalize("urn:isbn:978-0306406157") // "9780306406157"
isbn.Normalize("0-306-40615-3")           // "" (bad check digit)
isbn.Valid10("030640615X")                // check digit only
isbn.Valid13("9780306406157")             // 978/979 prefix + EAN check digit
isbn.To13("0306406152")                   // "9780306406157"
isbn.FromIdentifier("isbn", "0306406152") // typed or bare ISBNs; skips ASINs etc.
```

## Finding

Each finder returns `[]Candidate{ISBN13, File, Source, Label}`, where Source
is `opf`, `exth`, `xmp` or `text`, and Label is the edition named next to it
in the text (`ebook`, `hardcover`, `paperback`, `print`, `audio`, …).

| Function | Looks at |
| --- | --- |
| `EPUB(path, name)` | `dc:identifier`, `dc:source` (labelled `print`), then the text of the first 5 and last 3 spine documents |
| `MOBI(path, name)` | EXTH record 104 of MOBI/AZW3 |
| `XMP(packet, name)` | XMP properties named like an ISBN (`prism:isbn`, `prism:eIsbn`, …) and `dc:identifier` |
| `InText(text, name)` | Free text, such as a copyright page; checks every match and picks up nearby edition labels |
| `PDF(PDFSources{XMP, Pages}, name)` | XMP + page text that you extracted yourself |
| `FindInBook(dir, files, pdf)` | All of a book's files, ranked (`Response{Candidates, PDF}`) |

`Rank` sorts candidates (ebook-labelled first, then metadata, then other text,
then print/audio-labelled) and drops repeats.

```go
resp := isbn.FindInBook("/library/Author/Title (3)", []string{"Title.epub", "Title.azw3"}, nil)
for _, c := range resp.Candidates {
	fmt.Println(c.ISBN13, c.File, c.Source, c.Label)
}
```

## Limits

- PDFs aren't parsed here: Go has no reliable PDF text extraction. Pass the
  XMP packet and page text (e.g. from pdf.js) in `PDFSources`;
  `FindInBook` reports the first PDF's name in `Response.PDF` so you know to.
- EPUB text search reads only the first 5 and last 3 spine documents (up to
  4 MiB each), where copyright pages usually are.
- Everything is returned as ISBN-13. Numbers that look like ISBNs but fail
  the check digit are dropped.
