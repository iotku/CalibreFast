# metalookup

`import "github.com/iotku/CalibreFast/pkg/metalookup"`

Clients for [Open Library](https://openlibrary.org/developers/api),
[Google Books](https://developers.google.com/books/docs/v1/using) and, given a
`pkg/hardcover` client, [Hardcover](https://hardcover.app) that return book
metadata as `*opf.Metadata` (see `pkg/opf`).

## Usage

```go
c := metalookup.New(metalookup.Config{
	UserAgent: "MyApp/1.2 (+https://example.org/myapp)",
	GoogleKey: os.Getenv("GOOGLE_BOOKS_KEY"), // optional
	Hardcover: hc,                              // optional *hardcover.Client
})

res := c.Lookup(ctx, metalookup.OpenLibrary, metalookup.Query{ISBN: "9780134190440"})
if res.Error != "" { ... }
for _, m := range res.Matches {
	fmt.Println(m.Meta.Title, m.Meta.Creators, m.Meta.Identifiers)
	if m.CoverID != 0 {
		img, contentType, err := c.FetchCover(ctx, res.Source, strconv.FormatInt(m.CoverID, 10))
		...
	}
}

// Without an ISBN: up to 5 works, best first.
res = c.Lookup(ctx, metalookup.OpenLibrary, metalookup.Query{Title: "The Go Programming Language", Author: "Donovan"})
// Which of them has an edition with this ISBN? (Compared locally, not sent.)
res = res.MarkISBN("9780134190440")
```

The ISBN in a `Query` must already be a normalised ISBN-13 (see
`pkg/isbn.Normalize`).

## What you get

| Field | Open Library (ISBN) | Open Library (search) | Google Books | Hardcover |
| --- | --- | --- | --- | --- |
| Title (with subtitle) | edition | work | volume | book (edition by ISBN) |
| Creators | author records (≤5), else contributors | `author_name` | `authors` | contributions |
| Publisher, Date | edition | first publisher, first year | volume | edition |
| Description | edition or work, as HTML | work, as HTML | as given (HTML) or converted | plain text, converted |
| Languages | ISO 639-2/T (via `pkg/langcodes`) | only if one language | volume | edition |
| Series | no | no | no | featured series and number |
| Subjects | work + edition, minus shelving notes, ≤15 | search | categories split on `/` | Genre tags, ≤8 |
| Identifiers | `isbn`, `olid` | `olid` (work) | `isbn`, `google` | `isbn`, `hardcover` (slug), `hardcover-id` |
| CoverID | cover id | cover id | no | book id |

## Limits and defaults

- Default limits: Open Library 1 request/s with a burst of 20 and 1000 a day
  (an ISBN lookup is up to 7 requests); Google Books 0.1/s, burst 5, 100 a
  day. Pass your own `Limiter`s in `Config.Limits`.
- Default cache: 24 hours, 500 answers (`Config.Cache`). Callers that share
  results should treat them as read-only: `MarkISBN` returns a copy for that
  reason.
- Covers come from Open Library (large size, `default=false`, at most
  10 MiB, served as `image/*`) or Hardcover (only the image its lookup named
  for that book id; see `pkg/hardcover`).
- Hardcover uses its client's own `Budget` and cache, not `Limits` and
  `Cache`: over its budget, the lookup's error says when to try again.
- No retries. A 429 pauses that source for its `Retry-After` (1 s to 1 day,
  1 minute if missing).
