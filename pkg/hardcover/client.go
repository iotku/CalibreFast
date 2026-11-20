// Package hardcover asks Hardcover (hardcover.app) about book series: which
// series match a name or a set of ISBNs, and which books are in a series,
// in order. It speaks Hardcover's GraphQL API with a personal API token,
// server-side only, and keeps inside the plan's limits with a Budget that
// holds work rather than refusing it.
//
// Every query is a constant with GraphQL variables; no value is ever put
// into a query's text. Every query is at most three selection levels deep
// (Hardcover plans to enforce that) and costs the number of top-level
// fields it has (cost, checked by a test).
//
// Everything Hardcover answers is someone else's data: callers must treat
// titles, names and slugs as untrusted.
package hardcover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is Hardcover's GraphQL endpoint.
const DefaultURL = "https://api.hardcover.app/v1/graphql"

// Config configures a Client. Token is required.
type Config struct {
	URL        string // default DefaultURL
	Token      string // a Hardcover API token with read:catalog
	UserAgent  string // identify the software, not the user
	HTTPClient *http.Client
	Budget     *Budget          // default FreePlan()
	Now        func() time.Time // default time.Now (tests)
}

// Client asks Hardcover. It is safe for concurrent use.
type Client struct {
	url, auth, userAgent string
	http                 *http.Client
	Budget               *Budget
	now                  func() time.Time

	mu     sync.Mutex
	search map[string]searchEntry // cached SearchSeries answers
}

type searchEntry struct {
	at  time.Time
	res []Series
}

const searchTTL = 24 * time.Hour

// New makes a Client.
func New(cfg Config) *Client {
	c := &Client{
		url:       cfg.URL,
		auth:      authHeader(cfg.Token),
		userAgent: cfg.UserAgent,
		http:      cfg.HTTPClient,
		Budget:    cfg.Budget,
		now:       cfg.Now,
		search:    map[string]searchEntry{},
	}
	if c.url == "" {
		c.url = DefaultURL
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 35 * time.Second}
	}
	if c.Budget == nil {
		c.Budget = FreePlan()
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// authHeader is the token as Hardcover takes it: "Bearer <token>". A token
// pasted with its "Bearer " already is taken as it is.
func authHeader(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return "Bearer " + strings.TrimSpace(token[len("bearer "):])
	}
	return "Bearer " + token
}

// WaitError: the work can't be done now without going over the plan's
// limits (or Hardcover asked us to slow down). Hold it until Until.
type WaitError struct {
	Until  time.Time
	Reason string
}

func (e *WaitError) Error() string {
	return fmt.Sprintf("%s; waiting until %s", e.Reason, e.Until.UTC().Format("15:04 UTC"))
}

// ErrUnauthorized: the token is missing, wrong or expired.
var ErrUnauthorized = errors.New("Hardcover refused the API token (missing, wrong or expired): set CF_HARDCOVER_TOKEN to a current one")

// query is a GraphQL query and what it costs (its top-level fields).
type query struct {
	text string
	cost int
}

var (
	qSearchSeries = query{`query SearchSeries($q: String!) {
  search(query: $q, query_type: "Series", per_page: 10, page: 1) { results }
}`, 1}

	qEditionsByISBN = query{`query EditionsByISBN($isbns: [String!]!) {
  editions(where: {isbn_13: {_in: $isbns}}, limit: 200) { book_id }
}`, 1}

	qSeriesOfBooks = query{`query SeriesOfBooks($ids: [Int!]!) {
  book_series(where: {book_id: {_in: $ids}, compilation: {_eq: false}, series: {canonical_id: {_is_null: true}}}, limit: 500) {
    book_id
    position
    series { id name slug books_count }
  }
}`, 1}

	qSeriesBooks = query{`query SeriesBooks($id: Int!) {
  series(where: {id: {_eq: $id}}, limit: 1) {
    id
    name
    slug
    books_count
    book_series(
      distinct_on: position
      order_by: [{position: asc}, {book: {users_count: desc}}]
      where: {compilation: {_eq: false}, book: {canonical_id: {_is_null: true}, is_partial_book: {_eq: false}}}
    ) {
      position
      details
      book { id slug title subtitle release_date }
    }
  }
}`, 1}

	qBookDetails = query{`query BookDetails($ids: [Int!]!) {
  books(where: {id: {_in: $ids}}, limit: 300) {
    id
    contributions(where: {contributor_role: {slug: {_eq: "author"}}}) { author { name } }
    editions(where: {isbn_13: {_is_null: false}}, limit: 40) { isbn_13 }
  }
}`, 1}
)

// allQueries lists every query (tests check their depth and cost).
var allQueries = map[string]query{
	"SearchSeries": qSearchSeries, "EditionsByISBN": qEditionsByISBN, "SeriesOfBooks": qSeriesOfBooks,
	"SeriesBooks": qSeriesBooks, "BookDetails": qBookDetails,
}

type gqlError struct {
	Message string `json:"message"`
}

// post sends q with vars and decodes its data into out.
func (c *Client) post(ctx context.Context, q query, vars map[string]any, out any) error {
	if c.auth == "" {
		return ErrUnauthorized
	}
	if until := c.Budget.Reserve(c.now(), q.cost); !until.IsZero() {
		return &WaitError{Until: until, Reason: "Hardcover's request limit is used up for now"}
	}
	body, err := json.Marshal(map[string]any{"query": q.text, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Authorization", c.auth)
	resp, err := c.http.Do(req)
	if err != nil {
		// No URL or address in the error: it is shown in the UI.
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return errors.New("couldn't reach Hardcover: timed out")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("couldn't reach Hardcover")
	}
	defer resp.Body.Close()
	now := c.now()
	c.Budget.Observe(now, resp.Header.Get("RateLimit"))
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("reading Hardcover's answer: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusTooManyRequests:
		until := now.Add(retryAfter(resp.Header.Get("Retry-After")))
		c.Budget.Pause(until)
		return &WaitError{Until: until, Reason: "Hardcover asked us to slow down (HTTP 429)"}
	case http.StatusServiceUnavailable, http.StatusRequestTimeout, http.StatusBadGateway, http.StatusGatewayTimeout:
		until := now.Add(time.Minute)
		return &WaitError{Until: until, Reason: fmt.Sprintf("Hardcover is busy (HTTP %d)", resp.StatusCode)}
	case http.StatusForbidden:
		var e struct {
			Error  string   `json:"error"`
			Errors []string `json:"errors"`
			Scope  string   `json:"scope"`
		}
		_ = json.Unmarshal(raw, &e)
		switch {
		case e.Error == "insufficient_scope":
			return errors.New("the Hardcover API token lacks the read:catalog permission")
		case slices.Contains(e.Errors, "request_exceeds_capacity"):
			until := now.Add(time.Minute)
			c.Budget.Pause(until)
			return &WaitError{Until: until, Reason: "Hardcover said the request was too big for the plan's burst"}
		}
		return fmt.Errorf("Hardcover refused the request (HTTP 403): %s", clip(e.Error))
	default:
		return fmt.Errorf("Hardcover answered HTTP %d", resp.StatusCode)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("Hardcover's answer isn't JSON: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("Hardcover: %s", clip(env.Errors[0].Message))
	}
	return json.Unmarshal(env.Data, out)
}

// clip shortens a message from Hardcover for an error shown in the UI.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func retryAfter(h string) time.Duration {
	d := time.Minute
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = time.Until(t)
	}
	return min(max(d, time.Second), 24*time.Hour)
}

// flexInt is an id Hardcover may send as a number or a string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not an id: %q", clip(s))
	}
	*f = flexInt(n)
	return nil
}

// slugRe is what a Hardcover slug looks like; others aren't linked to.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,199}$`)

// ValidSlug: s is safe to put in a hardcover.app link.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

func cleanSlug(s string) string {
	if ValidSlug(s) {
		return s
	}
	return ""
}

// Series is a Hardcover series.
type Series struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug,omitempty"`
	Author     string `json:"author,omitempty"`
	BooksCount int    `json:"booksCount"`
	// Primary: books with a whole-number position (search only).
	Primary int `json:"primary,omitempty"`
}

// SearchSeries searches Hardcover's series by name, best first. Answers
// are cached for a day.
func (c *Client) SearchSeries(ctx context.Context, name string) ([]Series, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return nil, nil
	}
	key := strings.ToLower(name)
	c.mu.Lock()
	if e, ok := c.search[key]; ok && c.now().Sub(e.at) < searchTTL {
		c.mu.Unlock()
		return e.res, nil
	}
	c.mu.Unlock()
	var data struct {
		Search struct {
			Results json.RawMessage `json:"results"`
		} `json:"search"`
	}
	if err := c.post(ctx, qSearchSeries, map[string]any{"q": name}, &data); err != nil {
		return nil, err
	}
	var results struct {
		Hits []struct {
			Document struct {
				ID                flexInt `json:"id"`
				Name              string  `json:"name"`
				Slug              string  `json:"slug"`
				AuthorName        string  `json:"author_name"`
				BooksCount        int     `json:"books_count"`
				PrimaryBooksCount int     `json:"primary_books_count"`
			} `json:"document"`
		} `json:"hits"`
	}
	// results is Typesense's answer as JSON (sometimes as a string of it).
	raw := data.Search.Results
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &results); err != nil {
			return nil, fmt.Errorf("Hardcover's search answer: %w", err)
		}
	}
	var out []Series
	for _, h := range results.Hits {
		d := h.Document
		if d.ID <= 0 || strings.TrimSpace(d.Name) == "" {
			continue
		}
		out = append(out, Series{ID: int64(d.ID), Name: d.Name, Slug: cleanSlug(d.Slug), Author: d.AuthorName,
			BooksCount: d.BooksCount, Primary: d.PrimaryBooksCount})
	}
	c.mu.Lock()
	if len(c.search) >= 500 {
		clear(c.search)
	}
	c.search[key] = searchEntry{at: c.now(), res: out}
	c.mu.Unlock()
	return out, nil
}

// ISBNCandidate is a series some of the given ISBNs' books are in.
type ISBNCandidate struct {
	Series
	Hits int `json:"hits"` // how many of the books found are in it
}

// SeriesByISBNs finds the series the books with these ISBN-13s are in,
// most books first. Two requests.
func (c *Client) SeriesByISBNs(ctx context.Context, isbns []string) ([]ISBNCandidate, error) {
	if len(isbns) == 0 {
		return nil, nil
	}
	isbns = slices.Compact(slices.Sorted(slices.Values(isbns)))
	if len(isbns) > 100 {
		isbns = isbns[:100]
	}
	var eds struct {
		Editions []struct {
			BookID flexInt `json:"book_id"`
		} `json:"editions"`
	}
	if err := c.post(ctx, qEditionsByISBN, map[string]any{"isbns": isbns}, &eds); err != nil {
		return nil, err
	}
	var ids []int64
	for _, e := range eds.Editions {
		if e.BookID > 0 {
			ids = append(ids, int64(e.BookID))
		}
	}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return nil, nil
	}
	var bs struct {
		BookSeries []struct {
			BookID flexInt `json:"book_id"`
			Series struct {
				ID         flexInt `json:"id"`
				Name       string  `json:"name"`
				Slug       string  `json:"slug"`
				BooksCount int     `json:"books_count"`
			} `json:"series"`
		} `json:"book_series"`
	}
	if err := c.post(ctx, qSeriesOfBooks, map[string]any{"ids": ids}, &bs); err != nil {
		return nil, err
	}
	by := map[int64]*ISBNCandidate{}
	seen := map[[2]int64]bool{}
	var order []int64
	for _, x := range bs.BookSeries {
		sid := int64(x.Series.ID)
		if sid <= 0 || strings.TrimSpace(x.Series.Name) == "" || seen[[2]int64{sid, int64(x.BookID)}] {
			continue
		}
		seen[[2]int64{sid, int64(x.BookID)}] = true
		cand := by[sid]
		if cand == nil {
			cand = &ISBNCandidate{Series: Series{ID: sid, Name: x.Series.Name, Slug: cleanSlug(x.Series.Slug), BooksCount: x.Series.BooksCount}}
			by[sid] = cand
			order = append(order, sid)
		}
		cand.Hits++
	}
	out := make([]ISBNCandidate, 0, len(order))
	for _, id := range order {
		out = append(out, *by[id])
	}
	slices.SortStableFunc(out, func(a, b ISBNCandidate) int { return b.Hits - a.Hits })
	return out, nil
}

// Entry is one book of a series.
type Entry struct {
	BookID   int64    `json:"bookId"`
	Slug     string   `json:"slug,omitempty"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Position *float64 `json:"position,omitempty"` // nil: not numbered
	Details  string   `json:"details,omitempty"`  // the position as text
	Released string   `json:"released,omitempty"` // YYYY-MM-DD, or less
	Authors  []string `json:"authors,omitempty"`
	ISBNs    []string `json:"isbns,omitempty"` // ISBN-13s of its editions
}

// SeriesDetail is a series and its books in order (compilations and
// partial books left out, one book per position: its most read).
type SeriesDetail struct {
	Series
	Entries []Entry   `json:"entries"`
	Fetched time.Time `json:"fetched"`
}

// ErrNoSeries: Hardcover has no series with that id (any more).
var ErrNoSeries = errors.New("Hardcover has no such series (it may have been merged or removed)")

// SeriesBooks fetches a series' books, with their authors and ISBNs. Two
// requests; not cached.
func (c *Client) SeriesBooks(ctx context.Context, id int64) (*SeriesDetail, error) {
	var data struct {
		Series []struct {
			ID         flexInt `json:"id"`
			Name       string  `json:"name"`
			Slug       string  `json:"slug"`
			BooksCount int     `json:"books_count"`
			BookSeries []struct {
				Position *float64 `json:"position"`
				Details  *string  `json:"details"`
				Book     *struct {
					ID          flexInt `json:"id"`
					Slug        string  `json:"slug"`
					Title       string  `json:"title"`
					Subtitle    *string `json:"subtitle"`
					ReleaseDate *string `json:"release_date"`
				} `json:"book"`
			} `json:"book_series"`
		} `json:"series"`
	}
	if err := c.post(ctx, qSeriesBooks, map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}
	if len(data.Series) == 0 {
		return nil, ErrNoSeries
	}
	s := data.Series[0]
	d := &SeriesDetail{Series: Series{ID: int64(s.ID), Name: s.Name, Slug: cleanSlug(s.Slug), BooksCount: s.BooksCount}, Fetched: c.now().UTC()}
	byID := map[int64]int{}
	for _, bs := range s.BookSeries {
		b := bs.Book
		if b == nil || b.ID <= 0 || strings.TrimSpace(b.Title) == "" || byID[int64(b.ID)] > 0 {
			continue
		}
		e := Entry{BookID: int64(b.ID), Slug: cleanSlug(b.Slug), Title: b.Title, Position: bs.Position}
		if bs.Details != nil {
			e.Details = *bs.Details
		}
		if b.Subtitle != nil {
			e.Subtitle = *b.Subtitle
		}
		if b.ReleaseDate != nil {
			e.Released = *b.ReleaseDate
		}
		d.Entries = append(d.Entries, e)
		byID[e.BookID] = len(d.Entries)
	}
	if len(d.Entries) == 0 {
		return d, nil
	}
	ids := make([]int64, 0, len(d.Entries))
	for _, e := range d.Entries {
		ids = append(ids, e.BookID)
	}
	var books struct {
		Books []struct {
			ID            flexInt `json:"id"`
			Contributions []struct {
				Author *struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"contributions"`
			Editions []struct {
				ISBN13 *string `json:"isbn_13"`
			} `json:"editions"`
		} `json:"books"`
	}
	if err := c.post(ctx, qBookDetails, map[string]any{"ids": ids}, &books); err != nil {
		return nil, err
	}
	for _, b := range books.Books {
		i := byID[int64(b.ID)]
		if i == 0 {
			continue
		}
		e := &d.Entries[i-1]
		for _, c := range b.Contributions {
			if c.Author != nil && strings.TrimSpace(c.Author.Name) != "" && !slices.Contains(e.Authors, c.Author.Name) {
				e.Authors = append(e.Authors, c.Author.Name)
			}
		}
		for _, ed := range b.Editions {
			if ed.ISBN13 != nil && *ed.ISBN13 != "" && !slices.Contains(e.ISBNs, *ed.ISBN13) {
				e.ISBNs = append(e.ISBNs, *ed.ISBN13)
			}
		}
	}
	return d, nil
}
