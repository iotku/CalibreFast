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
	URL   string // default DefaultURL
	Token string // a Hardcover API token with read:catalog
	// TokenFunc, instead of Token, gives a token for each request (an
	// OAuth access token that's refreshed). force: Hardcover refused the
	// last one; get a new one.
	TokenFunc  func(ctx context.Context, force bool) (string, error)
	UserAgent  string // identify the software, not the user
	HTTPClient *http.Client
	Budget     *Budget          // default FreePlan()
	Now        func() time.Time // default time.Now (tests)
}

// Client asks Hardcover. It is safe for concurrent use.
type Client struct {
	url, auth, userAgent string
	tokenFunc            func(ctx context.Context, force bool) (string, error)
	http                 *http.Client
	Budget               *Budget
	now                  func() time.Time

	mu     sync.Mutex
	search map[string]searchEntry // cached SearchSeries answers

	lookups lookups      // cached LookupBooks answers (books.go)
	images  *http.Client // for covers: no token, only hardcover.app
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
		tokenFunc: cfg.TokenFunc,
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
	c.images = &http.Client{
		Transport: c.http.Transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !coverHostOK(req.URL.String()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
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

// ErrSignedOut: a TokenFunc client's token was refused even after a new one
// was got: the account was unlinked or its access revoked on Hardcover.
var ErrSignedOut = errors.New("Hardcover refused this account's sign-in (revoked or expired): link Hardcover again")

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

	qSeriesByName = query{`query SeriesByName($names: [String!]!) {
  series(where: {name: {_in: $names}, canonical_id: {_is_null: true}, books_count: {_gt: 0}}, order_by: {books_count: desc}, limit: 60) {
    id
    name
    slug
    books_count
    author { name }
  }
}`, 1}

	qSeriesBooks = query{`query SeriesBooks($ids: [Int!]!) {
  series(where: {id: {_in: $ids}}, limit: 60) {
    id
    name
    slug
    description
    books_count
    author { name }
    book_series(
      distinct_on: position
      order_by: [{position: asc}, {book: {users_count: desc}}]
      where: {compilation: {_eq: false}, book: {canonical_id: {_is_null: true}, is_partial_book: {_eq: false}}}
    ) {
      position
      details
      book { id slug title subtitle release_date users_count }
    }
  }
}`, 1}

	qBookDetails = query{`query BookDetails($ids: [Int!]!) {
  books(where: {id: {_in: $ids}}, limit: 300) {
    id
    contributions(where: {contributor_role: {slug: {_eq: "author"}}}, order_by: {id: asc}) { author { name } }
    editions(where: {isbn_13: {_is_null: false}}, limit: 40) { isbn_13 }
  }
}`, 1}
)

// allQueries lists every query (tests check their depth and cost).
var allQueries = map[string]query{
	"SearchSeries": qSearchSeries, "EditionsByISBN": qEditionsByISBN, "SeriesOfBooks": qSeriesOfBooks, "SeriesByName": qSeriesByName,
	"SeriesBooks": qSeriesBooks, "BookDetails": qBookDetails,
}

type gqlError struct {
	Message string `json:"message"`
}

// post sends q with vars and decodes its data into out. A TokenFunc
// client that's refused asks for a new token, once.
func (c *Client) post(ctx context.Context, q query, vars map[string]any, out any) error {
	if c.tokenFunc == nil {
		if c.auth == "" {
			return ErrUnauthorized
		}
		return c.send(ctx, c.auth, q, vars, out)
	}
	for force := range 2 {
		tok, err := c.tokenFunc(ctx, force == 1)
		if err != nil {
			return err
		}
		auth := authHeader(tok)
		if auth == "" {
			return ErrSignedOut
		}
		err = c.send(ctx, auth, q, vars, out)
		if !errors.Is(err, ErrUnauthorized) {
			return err
		}
	}
	return ErrSignedOut
}

// send is one request, with auth.
func (c *Client) send(ctx context.Context, auth string, q query, vars map[string]any, out any) error {
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
	req.Header.Set("Authorization", auth)
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
			scope := clip(e.Scope)
			if scope == "" {
				scope = "read:catalog"
			}
			return fmt.Errorf("the Hardcover token lacks the %s permission", scope)
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
	// Books: titles of some of its books (search only), for telling
	// same-named series apart.
	Books []string `json:"-"`
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
				ID                flexInt         `json:"id"`
				Name              string          `json:"name"`
				Slug              string          `json:"slug"`
				AuthorName        string          `json:"author_name"`
				BooksCount        int             `json:"books_count"`
				PrimaryBooksCount int             `json:"primary_books_count"`
				Books             json.RawMessage `json:"books"`
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
			BooksCount: d.BooksCount, Primary: d.PrimaryBooksCount, Books: bookTitles(d.Books)})
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

// bookTitles reads a search document's "books": titles, or objects with one.
func bookTitles(raw json.RawMessage) []string {
	var titles []string
	if json.Unmarshal(raw, &titles) == nil {
		return titles
	}
	var objs []struct {
		Title string `json:"title"`
	}
	if json.Unmarshal(raw, &objs) == nil {
		for _, o := range objs {
			if o.Title != "" {
				titles = append(titles, o.Title)
			}
		}
	}
	return titles
}

// SeriesByName finds the series with exactly these names. Hardcover
// sometimes splits one series into several of the same name, one per
// author ("Doctor Who: Target Books"); this finds all of them. One request.
func (c *Client) SeriesByName(ctx context.Context, names []string) ([]Series, error) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > 10 {
		names = names[:10]
	}
	var data struct {
		Series []struct {
			ID         flexInt `json:"id"`
			Name       string  `json:"name"`
			Slug       string  `json:"slug"`
			BooksCount int     `json:"books_count"`
			Author     *struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"series"`
	}
	if err := c.post(ctx, qSeriesByName, map[string]any{"names": names}, &data); err != nil {
		return nil, err
	}
	var out []Series
	for _, x := range data.Series {
		if x.ID <= 0 || strings.TrimSpace(x.Name) == "" {
			continue
		}
		s := Series{ID: int64(x.ID), Name: x.Name, Slug: cleanSlug(x.Slug), BooksCount: x.BooksCount}
		if x.Author != nil {
			s.Author = x.Author.Name
		}
		out = append(out, s)
	}
	return out, nil
}

// Entry is one book of a series.
type Entry struct {
	BookID   int64    `json:"bookId"`
	SeriesID int64    `json:"seriesId,omitempty"` // the part it came from
	Slug     string   `json:"slug,omitempty"`
	Title    string   `json:"title"`
	Subtitle string   `json:"subtitle,omitempty"`
	Position *float64 `json:"position,omitempty"` // nil: not numbered
	Details  string   `json:"details,omitempty"`  // the position as text
	Released string   `json:"released,omitempty"` // YYYY-MM-DD, or less
	Authors  []string `json:"authors,omitempty"`
	ISBNs    []string `json:"isbns,omitempty"` // ISBN-13s of its editions
	// AltTitles: other Hardcover books at the same position in another
	// part of the series (the same work, entered twice).
	AltTitles []string `json:"altTitles,omitempty"`
	// AltIDs: those books' ids. Which of them is shown can change from
	// one fetch to the next, so a decision about the entry (a dismissal)
	// is kept against any of them.
	AltIDs     []int64 `json:"altIds,omitempty"`
	users      int
	altAuthors []string
}

// IDs: the entry's book id, then the ids of the books merged into it.
func (e *Entry) IDs() []int64 { return append([]int64{e.BookID}, e.AltIDs...) }

// SeriesDetail is a series and its books in order (compilations and
// partial books left out, one book per position: its most read). A
// series Hardcover splits into parts (same name, one per author) is all
// its parts' books together; Series is the part with the most books.
type SeriesDetail struct {
	Series
	// Description: Hardcover's, as plain text (the biggest part's that
	// has one); "" for none.
	Description string    `json:"description,omitempty"`
	Parts       []Series  `json:"parts,omitempty"`
	Entries     []Entry   `json:"entries"`
	Fetched     time.Time `json:"fetched"`
}

// AllParts is the series' parts (the series itself when it has one).
func (d *SeriesDetail) AllParts() []Series {
	if len(d.Parts) > 0 {
		return d.Parts
	}
	return []Series{d.Series}
}

// ErrNoSeries: Hardcover has no series with that id (any more).
var ErrNoSeries = errors.New("Hardcover has no such series (it may have been merged or removed)")

// maxParts: the most parts one library series is linked to.
const maxParts = 60

type seriesBooksRow struct {
	ID          flexInt `json:"id"`
	Name        string  `json:"name"`
	Slug        string  `json:"slug"`
	BooksCount  int     `json:"books_count"`
	Description *string `json:"description"`
	Author      *struct {
		Name string `json:"name"`
	} `json:"author"`
	BookSeries []struct {
		Position *float64 `json:"position"`
		Details  *string  `json:"details"`
		Book     *struct {
			ID          flexInt `json:"id"`
			Slug        string  `json:"slug"`
			Title       string  `json:"title"`
			Subtitle    *string `json:"subtitle"`
			ReleaseDate *string `json:"release_date"`
			UsersCount  int     `json:"users_count"`
		} `json:"book"`
	} `json:"book_series"`
}

// SeriesBooks fetches the books of a series, or of all the parts of one
// (several ids), with their authors and ISBNs. Books at the same position
// in two parts are taken as one: the most read is shown, the others' titles
// and ISBNs count for matching. Two requests (one more per 300 books
// beyond the first 300); not cached.
func (c *Client) SeriesBooks(ctx context.Context, ids ...int64) (*SeriesDetail, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return nil, ErrNoSeries
	}
	if len(ids) > maxParts {
		ids = ids[:maxParts]
	}
	var data struct {
		Series []seriesBooksRow `json:"series"`
	}
	if err := c.post(ctx, qSeriesBooks, map[string]any{"ids": ids}, &data); err != nil {
		return nil, err
	}
	if len(data.Series) == 0 {
		return nil, ErrNoSeries
	}
	// The biggest part first: it names the whole.
	slices.SortStableFunc(data.Series, func(a, b seriesBooksRow) int { return b.BooksCount - a.BooksCount })
	d := &SeriesDetail{Fetched: c.now().UTC()}
	owner := map[int64]int{} // any book id -> its entry's index
	byPos := map[float64]int{}
	for _, s := range data.Series {
		part := Series{ID: int64(s.ID), Name: s.Name, Slug: cleanSlug(s.Slug), BooksCount: s.BooksCount}
		if s.Author != nil {
			part.Author = s.Author.Name
		}
		d.Parts = append(d.Parts, part)
		if d.Description == "" && s.Description != nil {
			d.Description = cleanBio(*s.Description)
		}
		for _, bs := range s.BookSeries {
			b := bs.Book
			if b == nil || b.ID <= 0 || strings.TrimSpace(b.Title) == "" {
				continue
			}
			if _, seen := owner[int64(b.ID)]; seen {
				continue
			}
			e := Entry{BookID: int64(b.ID), SeriesID: part.ID, Slug: cleanSlug(b.Slug), Title: b.Title, Position: bs.Position, users: b.UsersCount}
			if bs.Details != nil {
				e.Details = *bs.Details
			}
			if b.Subtitle != nil {
				e.Subtitle = *b.Subtitle
			}
			if b.ReleaseDate != nil {
				e.Released = *b.ReleaseDate
			}
			if e.Position != nil {
				if i, ok := byPos[*e.Position]; ok {
					old := &d.Entries[i]
					if e.users > old.users { // the most read one is shown
						e.AltTitles = append(old.AltTitles, old.Title)
						e.AltIDs = append(old.AltIDs, old.BookID)
						*old = e
					} else {
						old.AltIDs = append(old.AltIDs, e.BookID)
						if e.Title != old.Title && !slices.Contains(old.AltTitles, e.Title) {
							old.AltTitles = append(old.AltTitles, e.Title)
						}
					}
					owner[int64(b.ID)] = i
					continue
				}
				byPos[*e.Position] = len(d.Entries)
			}
			owner[e.BookID] = len(d.Entries)
			d.Entries = append(d.Entries, e)
		}
	}
	d.Series = d.Parts[0]
	if len(d.Parts) == 1 {
		d.Parts = nil
	}
	if len(d.Entries) > 0 {
		if err := c.fillBookDetails(ctx, d, owner); err != nil {
			return nil, err
		}
	}
	// In order; unnumbered last.
	slices.SortStableFunc(d.Entries, func(a, b Entry) int {
		switch {
		case a.Position == nil && b.Position == nil:
			return 0
		case a.Position == nil:
			return 1
		case b.Position == nil:
			return -1
		case *a.Position < *b.Position:
			return -1
		case *a.Position > *b.Position:
			return 1
		}
		return 0
	})
	return d, nil
}

// fillBookDetails adds the authors and ISBNs of every book (merged ones
// too: owner maps each book id to its entry).
func (c *Client) fillBookDetails(ctx context.Context, d *SeriesDetail, owner map[int64]int) error {
	ids := make([]int64, 0, len(owner))
	for id := range owner {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for chunk := range slices.Chunk(ids, 300) {
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
		if err := c.post(ctx, qBookDetails, map[string]any{"ids": chunk}, &books); err != nil {
			return err
		}
		for _, b := range books.Books {
			i, ok := owner[int64(b.ID)]
			if !ok {
				continue
			}
			e := &d.Entries[i]
			for _, c := range b.Contributions {
				if c.Author == nil || strings.TrimSpace(c.Author.Name) == "" {
					continue
				}
				if int64(b.ID) == e.BookID {
					if !slices.Contains(e.Authors, c.Author.Name) {
						e.Authors = append(e.Authors, c.Author.Name)
					}
				} else if !slices.Contains(e.altAuthors, c.Author.Name) {
					e.altAuthors = append(e.altAuthors, c.Author.Name)
				}
			}
			for _, ed := range b.Editions {
				if ed.ISBN13 != nil && *ed.ISBN13 != "" && !slices.Contains(e.ISBNs, *ed.ISBN13) {
					e.ISBNs = append(e.ISBNs, *ed.ISBN13)
				}
			}
		}
	}
	// A merged duplicate's authors count only when the shown book has none.
	for i := range d.Entries {
		if e := &d.Entries[i]; len(e.Authors) == 0 {
			e.Authors = e.altAuthors
		}
	}
	return nil
}
