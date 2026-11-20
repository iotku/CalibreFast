package hardcover

// Looking one book up, for the metadata editor: by ISBN (the edition with
// that ISBN, and its book) or by title and author (Hardcover's search, then
// its best books). Two requests either way.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	qEditionByISBN = query{`query EditionByISBN($isbn13: String!, $isbn10: String!) {
  editions(where: {_or: [{isbn_13: {_eq: $isbn13}}, {isbn_10: {_eq: $isbn10}}]}, order_by: {users_count: desc}, limit: 1) {
    id
    book_id
    title
    subtitle
    isbn_13
    release_date
    pages
    cached_image
    publisher { name }
    language { code2 code3 }
  }
}`, 1}

	qSearchBooks = query{`query SearchBooks($q: String!) {
  search(query: $q, query_type: "Book", per_page: 5, page: 1) { results }
}`, 1}

	qBooksForLookup = query{`query BooksForLookup($ids: [Int!]!) {
  books(where: {id: {_in: $ids}}, limit: 10) {
    id
    slug
    title
    subtitle
    description
    release_date
    pages
    cached_image
    cached_tags
    contributions(where: {contributor_role: {slug: {_eq: "author"}}}, order_by: {id: asc}) { author { name } }
    book_series(where: {compilation: {_eq: false}}, order_by: [{featured: desc}, {id: asc}], limit: 3) { position series { name } }
  }
}`, 1}
)

func init() {
	allQueries["EditionByISBN"] = qEditionByISBN
	allQueries["SearchBooks"] = qSearchBooks
	allQueries["BooksForLookup"] = qBooksForLookup
}

// BookQuery is what's sent: an ISBN-13, or a title and an author.
type BookQuery struct {
	ISBN, Title, Author string
}

// Book is a book (or, by ISBN, an edition of it) as Hardcover has it.
type Book struct {
	ID       int64
	Slug     string // valid (ValidSlug) or ""
	Title    string
	Subtitle string
	Authors  []string
	// Description is plain text.
	Description string
	Released    string // YYYY-MM-DD, or less
	Pages       int
	Publisher   string   // an edition's only
	Languages   []string // an edition's: ISO 639-1 or -2 codes
	ISBN13      string   // the edition's
	Series      string
	Position    *float64
	Genres      []string
	// image is the cover's URL, for FetchCover; only an https URL on
	// hardcover.app.
	image string
}

// HasCover: Hardcover has a cover image for it.
func (b *Book) HasCover() bool { return b.image != "" }

const (
	lookupTTL  = 24 * time.Hour
	maxLookups = 500
	maxGenres  = 8
)

type lookupEntry struct {
	at    time.Time
	books []Book
}

// lookups caches LookupBooks answers (no match included) and the covers
// they named, so FetchCover takes a book id and never a URL.
type lookups struct {
	mu      sync.Mutex
	answers map[string]lookupEntry
	covers  map[int64]string // book id -> cover URL
}

func (q BookQuery) key() string {
	if q.ISBN != "" {
		return "isbn:" + q.ISBN
	}
	return "q:" + strings.ToLower(q.Title) + "\x00" + strings.ToLower(q.Author)
}

// LookupBooks finds a book: by ISBN-13, the edition with it (and its
// book); else up to five books for the title and author, best first.
// Answers, "no match" included, are cached for a day.
func (c *Client) LookupBooks(ctx context.Context, q BookQuery) ([]Book, error) {
	q.ISBN = strings.TrimSpace(q.ISBN)
	q.Title = strings.Join(strings.Fields(q.Title), " ")
	q.Author = strings.Join(strings.Fields(q.Author), " ")
	if q.ISBN == "" && q.Title == "" {
		return nil, errors.New("need an ISBN or a title")
	}
	key := q.key()
	c.lookups.mu.Lock()
	if e, ok := c.lookups.answers[key]; ok && c.now().Sub(e.at) < lookupTTL {
		c.lookups.mu.Unlock()
		return e.books, nil
	}
	c.lookups.mu.Unlock()
	var books []Book
	var err error
	if q.ISBN != "" {
		books, err = c.bookByISBN(ctx, q.ISBN)
	} else {
		books, err = c.searchBooks(ctx, q.Title, q.Author)
	}
	if err != nil {
		return nil, err
	}
	c.lookups.mu.Lock()
	defer c.lookups.mu.Unlock()
	if c.lookups.answers == nil || len(c.lookups.answers) >= maxLookups {
		c.lookups.answers = map[string]lookupEntry{}
	}
	if c.lookups.covers == nil || len(c.lookups.covers) >= 10*maxLookups {
		c.lookups.covers = map[int64]string{}
	}
	c.lookups.answers[key] = lookupEntry{at: c.now(), books: books}
	for _, b := range books {
		if b.image != "" {
			c.lookups.covers[b.ID] = b.image
		}
	}
	return books, nil
}

// isbn10Of is the ISBN-10 form of a 978 ISBN-13, or "".
func isbn10Of(isbn13 string) string {
	if len(isbn13) != 13 || !strings.HasPrefix(isbn13, "978") {
		return ""
	}
	body := isbn13[3:12]
	sum := 0
	for i, r := range body {
		if r < '0' || r > '9' {
			return ""
		}
		sum += (10 - i) * int(r-'0')
	}
	check := (11 - sum%11) % 11
	if check == 10 {
		return body + "X"
	}
	return body + strconv.Itoa(check)
}

func (c *Client) bookByISBN(ctx context.Context, isbn13 string) ([]Book, error) {
	var data struct {
		Editions []struct {
			ID          flexInt         `json:"id"`
			BookID      flexInt         `json:"book_id"`
			Title       string          `json:"title"`
			Subtitle    string          `json:"subtitle"`
			ISBN13      string          `json:"isbn_13"`
			ReleaseDate string          `json:"release_date"`
			Pages       int             `json:"pages"`
			CachedImage json.RawMessage `json:"cached_image"`
			Publisher   *struct {
				Name string `json:"name"`
			} `json:"publisher"`
			Language *struct {
				Code2 string `json:"code2"`
				Code3 string `json:"code3"`
			} `json:"language"`
		} `json:"editions"`
	}
	isbn10 := isbn10Of(isbn13)
	if isbn10 == "" {
		isbn10 = isbn13 // matches no ISBN-10
	}
	if err := c.post(ctx, qEditionByISBN, map[string]any{"isbn13": isbn13, "isbn10": isbn10}, &data); err != nil {
		return nil, err
	}
	if len(data.Editions) == 0 || data.Editions[0].BookID <= 0 {
		return nil, nil
	}
	ed := data.Editions[0]
	books, err := c.booksForLookup(ctx, []int64{int64(ed.BookID)})
	if err != nil || len(books) == 0 {
		return nil, err
	}
	b := books[0]
	// The edition's own values, where it has them.
	if t := strings.TrimSpace(ed.Title); t != "" {
		b.Title, b.Subtitle = t, strings.TrimSpace(ed.Subtitle)
	}
	if ed.ReleaseDate != "" {
		b.Released = ed.ReleaseDate
	}
	if ed.Pages > 0 {
		b.Pages = ed.Pages
	}
	if img := imageURL(ed.CachedImage); img != "" {
		b.image = img
	}
	if ed.Publisher != nil {
		b.Publisher = strings.TrimSpace(ed.Publisher.Name)
	}
	if ed.Language != nil {
		if l := firstNonEmpty(ed.Language.Code3, ed.Language.Code2); l != "" {
			b.Languages = []string{l}
		}
	}
	b.ISBN13 = isbn13
	return []Book{b}, nil
}

func (c *Client) searchBooks(ctx context.Context, title, author string) ([]Book, error) {
	var data struct {
		Search struct {
			Results json.RawMessage `json:"results"`
		} `json:"search"`
	}
	if err := c.post(ctx, qSearchBooks, map[string]any{"q": strings.TrimSpace(title + " " + author)}, &data); err != nil {
		return nil, err
	}
	var results struct {
		Hits []struct {
			Document struct {
				ID flexInt `json:"id"`
			} `json:"document"`
		} `json:"hits"`
	}
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
	var ids []int64
	for _, h := range results.Hits {
		if id := int64(h.Document.ID); id > 0 && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	books, err := c.booksForLookup(ctx, ids)
	if err != nil {
		return nil, err
	}
	// In the search's order.
	slices.SortStableFunc(books, func(a, b Book) int { return slices.Index(ids, a.ID) - slices.Index(ids, b.ID) })
	return books, nil
}

func (c *Client) booksForLookup(ctx context.Context, ids []int64) ([]Book, error) {
	var data struct {
		Books []struct {
			ID            flexInt         `json:"id"`
			Slug          string          `json:"slug"`
			Title         string          `json:"title"`
			Subtitle      string          `json:"subtitle"`
			Description   string          `json:"description"`
			ReleaseDate   string          `json:"release_date"`
			Pages         int             `json:"pages"`
			CachedImage   json.RawMessage `json:"cached_image"`
			CachedTags    json.RawMessage `json:"cached_tags"`
			Contributions []struct {
				Author *struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"contributions"`
			BookSeries []struct {
				Position *float64 `json:"position"`
				Series   *struct {
					Name string `json:"name"`
				} `json:"series"`
			} `json:"book_series"`
		} `json:"books"`
	}
	if err := c.post(ctx, qBooksForLookup, map[string]any{"ids": ids}, &data); err != nil {
		return nil, err
	}
	var out []Book
	for _, x := range data.Books {
		if x.ID <= 0 || strings.TrimSpace(x.Title) == "" {
			continue
		}
		b := Book{
			ID:          int64(x.ID),
			Slug:        cleanSlug(x.Slug),
			Title:       strings.TrimSpace(x.Title),
			Subtitle:    strings.TrimSpace(x.Subtitle),
			Description: strings.TrimSpace(x.Description),
			Released:    x.ReleaseDate,
			Pages:       x.Pages,
			Genres:      genres(x.CachedTags),
			image:       imageURL(x.CachedImage),
		}
		for _, ct := range x.Contributions {
			if ct.Author != nil {
				if n := strings.TrimSpace(ct.Author.Name); n != "" && !slices.Contains(b.Authors, n) {
					b.Authors = append(b.Authors, n)
				}
			}
		}
		for _, bs := range x.BookSeries {
			if bs.Series != nil && strings.TrimSpace(bs.Series.Name) != "" {
				b.Series, b.Position = strings.TrimSpace(bs.Series.Name), bs.Position
				break
			}
		}
		out = append(out, b)
	}
	return out, nil
}

// imageURL is the url in a cached_image, if it's an https URL on
// hardcover.app (where Hardcover keeps its covers); else "".
func imageURL(raw json.RawMessage) string {
	var img struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &img) != nil {
		return ""
	}
	if !coverHostOK(img.URL) {
		return ""
	}
	return img.URL
}

func coverHostOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "hardcover.app" || strings.HasSuffix(h, ".hardcover.app")
}

// genres are a book's Genre tags, most used first.
func genres(raw json.RawMessage) []string {
	var tags map[string][]struct {
		Tag string `json:"tag"`
	}
	if json.Unmarshal(raw, &tags) != nil {
		return nil
	}
	var out []string
	for _, t := range tags["Genre"] {
		if n := strings.TrimSpace(t.Tag); n != "" && !slices.Contains(out, n) && len(out) < maxGenres {
			out = append(out, n)
		}
	}
	return out
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// Errors from FetchCover.
var (
	// ErrNoCover: no cover is known for the book (look it up first), or
	// Hardcover has no image.
	ErrNoCover = errors.New("no cover")
	// ErrCoverUnreadable: the image couldn't be read, or is over 10 MiB.
	ErrCoverUnreadable = errors.New("cover unavailable")
)

// FetchCover fetches the cover of a book a LookupBooks answer named, by its
// id: the URL is the one Hardcover gave, never one from the caller. It's
// sent without the API token. It returns the image and its Content-Type.
func (c *Client) FetchCover(ctx context.Context, bookID int64) ([]byte, string, error) {
	c.lookups.mu.Lock()
	u := c.lookups.covers[bookID]
	c.lookups.mu.Unlock()
	return c.FetchImage(ctx, u)
}

// FetchImage fetches an image Hardcover named (a cover, an author's
// picture): only an https URL on hardcover.app, sent without the API
// token, and only a raster image of at most 10 MiB.
func (c *Client) FetchImage(ctx context.Context, u string) ([]byte, string, error) {
	if u == "" || !coverHostOK(u) {
		return nil, "", ErrNoCover
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", ErrNoCover
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.images.Do(req)
	if err != nil {
		return nil, "", ErrCoverUnreadable
	}
	defer resp.Body.Close()
	// Raster images only: an SVG served from our origin could run script.
	ctype, _, _ := strings.Cut(strings.ToLower(resp.Header.Get("Content-Type")), ";")
	ctype = strings.TrimSpace(ctype)
	if resp.StatusCode != http.StatusOK || !slices.Contains([]string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif"}, ctype) {
		return nil, "", ErrNoCover
	}
	const maxCover = 10 << 20
	img, err := readLimited(resp, maxCover)
	if err != nil {
		return nil, "", ErrCoverUnreadable
	}
	return img, ctype, nil
}

func readLimited(resp *http.Response, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("too big")
	}
	return b, nil
}
