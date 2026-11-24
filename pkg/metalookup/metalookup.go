package metalookup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/opf"
)

// Sources, as Lookup takes them and Result names them.
const (
	OpenLibrary = "Open Library"
	GoogleBooks = "Google Books"
	Hardcover   = "Hardcover"
)

// maxSearchMatches is how many books a title search offers.
const maxSearchMatches = 5

// Config configures a Client. Every field is optional.
type Config struct {
	HTTPClient *http.Client // default: a client with a 15s timeout
	// UserAgent is sent with every request. Identify your software, not
	// your user.
	UserAgent string

	OpenLibraryURL string // default https://openlibrary.org
	CoversURL      string // default https://covers.openlibrary.org
	GoogleURL      string // default https://www.googleapis.com/books/v1
	// GoogleKey is a Google Books API key. Without one, Google counts
	// requests against a shared quota that's usually used up.
	GoogleKey string

	// Hardcover makes Hardcover a source; nil: it isn't one. Its own budget
	// and cache apply, not Limits and Cache.
	Hardcover *hardcover.Client

	// Limits holds a Limiter per source (OpenLibrary, GoogleBooks). A
	// missing one gets the default: Open Library 1/s, burst 20, 1000 a day
	// (a lookup is up to 7 requests); Google Books 0.1/s, burst 5, 100 a day.
	Limits map[string]*Limiter
	// Cache keeps answers. Default: 24 hours, 500 answers.
	Cache *Cache
	// Logf logs best-effort failures (a work or author that couldn't be
	// fetched). Default: log.Printf.
	Logf func(format string, args ...any)
}

// Client looks books up. It is safe for concurrent use.
type Client struct {
	client      *http.Client
	userAgent   string
	openLibrary string
	olCovers    string
	googleBooks string
	googleKey   string
	hardcover   *hardcover.Client
	cache       *Cache
	limits      map[string]*Limiter // by source name
	logf        func(format string, args ...any)
}

// New makes a Client.
func New(cfg Config) *Client {
	or := func(s, def string) string {
		if s == "" {
			return def
		}
		return strings.TrimSuffix(s, "/")
	}
	c := &Client{
		client:      cfg.HTTPClient,
		userAgent:   cfg.UserAgent,
		openLibrary: or(cfg.OpenLibraryURL, "https://openlibrary.org"),
		olCovers:    or(cfg.CoversURL, "https://covers.openlibrary.org"),
		googleBooks: or(cfg.GoogleURL, "https://www.googleapis.com/books/v1"),
		googleKey:   cfg.GoogleKey,
		hardcover:   cfg.Hardcover,
		cache:       cfg.Cache,
		limits:      map[string]*Limiter{},
		logf:        cfg.Logf,
	}
	if c.client == nil {
		c.client = &http.Client{Timeout: 15 * time.Second}
	}
	if c.cache == nil {
		c.cache = NewCache(24*time.Hour, 500)
	}
	if c.logf == nil {
		c.logf = log.Printf
	}
	maps.Copy(c.limits, cfg.Limits)
	if c.limits[OpenLibrary] == nil {
		c.limits[OpenLibrary] = NewLimiter(1, 20, 1000)
	}
	if c.limits[GoogleBooks] == nil {
		c.limits[GoogleBooks] = NewLimiter(0.1, 5, 100)
	}
	return c
}

// Usage reports a source's limits at now (OpenLibrary, GoogleBooks); false
// for a source it doesn't know.
func (c *Client) Usage(source string, now time.Time) (Usage, bool) {
	l := c.limits[source]
	if l == nil {
		return Usage{}, false
	}
	return l.Usage(now), true
}

// Query is what's sent: an ISBN-13, or a title and an author.
type Query struct {
	ISBN, Title, Author string
}

func (q Query) key() string {
	if q.ISBN != "" {
		return "isbn:" + q.ISBN
	}
	return "q:" + strings.ToLower(q.Title) + "\x00" + strings.ToLower(q.Author)
}

// Match is one book a source found.
type Match struct {
	Meta *opf.Metadata
	// CoverID is the id FetchCover takes for this source's cover (Open
	// Library: a cover id; Hardcover: the book id), or 0 for none.
	CoverID int64
	// HasISBN: one of its editions has the ISBN given to MarkISBN.
	HasISBN bool
	// isbns are a search match's editions' ISBN-13s, for MarkISBN.
	isbns map[string]bool
}

// Result is a source's answer.
type Result struct {
	Source  string  // OpenLibrary, GoogleBooks or Hardcover
	Matches []Match // best first; none: no match, or an error
	Error   string
}

// MarkISBN sets HasISBN on the matches with an edition of isbn13 (only a
// title search knows its matches' editions), on a copy: res may be the
// cache's.
func (res Result) MarkISBN(isbn13 string) Result {
	res.Matches = slices.Clone(res.Matches)
	for i := range res.Matches {
		res.Matches[i].HasISBN = res.Matches[i].isbns[isbn13]
	}
	return res
}

// Lookup asks one source (OpenLibrary, GoogleBooks or Hardcover). Answers,
// including "no match", are cached (Hardcover's by its own client); errors
// aren't.
func (c *Client) Lookup(ctx context.Context, source string, q Query) Result {
	key := source + "\x00" + q.key()
	if res, ok := c.cache.get(key); ok {
		return res
	}
	fn := map[string]func(context.Context, Query) ([]Match, error){
		OpenLibrary: c.openLibraryLookup,
		GoogleBooks: c.googleBooksLookup,
		Hardcover:   c.hardcoverLookup,
	}[source]
	if fn == nil {
		return Result{Source: source, Error: "unknown source"}
	}
	matches, err := fn(ctx, q)
	if err != nil {
		c.logf("lookup: %s: %v", source, err)
		return Result{Source: source, Error: err.Error()}
	}
	for _, m := range matches {
		m.Meta.Subjects = opf.UniqueTags(m.Meta.Subjects)
	}
	res := Result{Source: source, Matches: matches} // none: no match
	if source != Hardcover { // its client caches answers with the covers they name
		c.cache.put(key, res)
	}
	return res
}

// statusError is a non-200, non-404 answer.
type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

// do sends a request to a service within its limits, with the generic
// User-Agent. A 429 pauses the service for its Retry-After.
func (c *Client) do(service string, req *http.Request) (*http.Response, error) {
	limit := c.limits[service]
	if err := limit.Take(time.Now()); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		// Drop the URL: the error is shown in the editor.
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		wait := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		limit.Pause(time.Now().Add(wait))
		return nil, &rateLimitedError{wait}
	}
	return resp, nil
}

// rateLimitedError: the service answered 429.
type rateLimitedError struct{ wait time.Duration }

func (e *rateLimitedError) Error() string {
	return fmt.Sprintf("asked to slow down (HTTP 429); try again in %s", e.wait)
}

// getJSON fetches rawURL from a service into v. found is false on a 404.
func (c *Client) getJSON(ctx context.Context, service, rawURL string, v any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.do(service, req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
	case http.StatusNotFound:
		return false, nil
	}
	return false, statusError(resp.StatusCode)
}

// retryAfter reads a Retry-After header (seconds or a date): at least a
// second, at most a day, and a minute when missing.
func retryAfter(h string, now time.Time) time.Duration {
	d := time.Minute
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, time.Second), 24*time.Hour).Round(time.Second)
}
