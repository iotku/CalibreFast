# hardcover

`import "github.com/iotku/CalibreFast/pkg/hardcover"`

A client for [Hardcover's GraphQL API](https://docs.hardcover.app/api/getting-started/)
that finds book series and lists their books in order. It runs server-side
only, with a personal API token that has the `read:catalog` scope.

## Usage

```go
c := hardcover.New(hardcover.Config{
	Token:     os.Getenv("HARDCOVER_TOKEN"),
	UserAgent: "MyApp/1.2 (+https://example.org/myapp)",
	Budget:    hardcover.FreePlan(), // 60/min, burst 10, 4,500 a day (UTC)
})

found, err := c.SearchSeries(ctx, "The Expanse")        // by name (cached a day)
byISBN, err := c.SeriesByISBNs(ctx, []string{"978…"})   // the series those books are in
d, err := c.SeriesBooks(ctx, found[0].ID)              // its books: position, title, authors, ISBNs

var wait *hardcover.WaitError
if errors.As(err, &wait) {
	// Over the plan's limits (or Hardcover answered 429): nothing was sent.
	// Hold the work until wait.Until and try again.
}
```

## Limits

Hardcover counts **each top-level field** of a query as one request. Every
query here has one top-level field and is at most three selection levels deep
(tests check both). `Budget` is a token bucket plus a daily count that resets
at midnight UTC, as Hardcover's does. Unlike `metalookup.Limiter`, it never
refuses work: it says how long to wait. It also reads Hardcover's `RateLimit`
header, so use of the same token by other tools counts too. A 429 pauses it
for the `Retry-After`. `Usage` and `Restore` let a caller keep the day's
count across restarts.

## Data

Everything Hardcover returns is user-contributed and untrusted. Slugs that
don't match `^[a-z0-9-]+$` are dropped (`ValidSlug`). Errors never contain
the token or the URL.
