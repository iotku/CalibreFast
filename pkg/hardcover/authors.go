package hardcover

// Authors: who wrote a library's books, for the profile at the top of an
// author's page. AuthorsByName is one request; when it finds nothing,
// SearchAuthors (one more) finds those whose name or alternate names come
// close, and AuthorsByID (one more) fetches them.

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
)

var (
	qAuthorsByName = query{`query AuthorsByName($names: [String!]!) {
  authors(where: {name: {_in: $names}, canonical_id: {_is_null: true}}, order_by: {users_count: desc}, limit: 10) {
    id
    name
    slug
    bio
    born_year
    death_year
    cached_image
    books_count
    users_count
    contributions(where: {contributable_type: {_eq: "Book"}}, order_by: {book: {users_count: desc}}, limit: 40) { book { title } }
  }
}`, 1}

	qAuthorsByID = query{`query AuthorsByID($ids: [Int!]!) {
  authors(where: {id: {_in: $ids}}, limit: 10) {
    id
    name
    slug
    bio
    born_year
    death_year
    cached_image
    books_count
    users_count
    contributions(where: {contributable_type: {_eq: "Book"}}, order_by: {book: {users_count: desc}}, limit: 40) { book { title } }
  }
}`, 1}

	qSearchAuthors = query{`query SearchAuthors($q: String!) {
  search(query: $q, query_type: "Author", per_page: 10, page: 1) { results }
}`, 1}
)

func init() {
	allQueries["AuthorsByName"] = qAuthorsByName
	allQueries["AuthorsByID"] = qAuthorsByID
	allQueries["SearchAuthors"] = qSearchAuthors
}

// maxBio bounds an author's bio, in bytes.
const maxBio = 8000

var (
	bioFootnoteRe = regexp.MustCompile(`(?is)<sup\b[^>]*>.*?</sup>`)
	bioBreakRe    = regexp.MustCompile(`(?i)<br\s*/?>|</p>`)
	bioTagRe      = regexp.MustCompile(`<[^>]*>`)
	bioCiteRe     = regexp.MustCompile(`\[(\d{1,3}|citation needed)\]`)
)

// Author is a Hardcover author. Every string is someone else's data.
type Author struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Slug       string   `json:"slug,omitempty"` // "" unless ValidSlug
	Bio        string   `json:"bio,omitempty"`
	BornYear   int      `json:"bornYear,omitempty"`
	DeathYear  int      `json:"deathYear,omitempty"`
	ImageURL   string   `json:"imageUrl,omitempty"` // https on hardcover.app, or ""
	BooksCount int      `json:"booksCount"`
	UsersCount int      `json:"usersCount"`
	Titles     []string `json:"titles,omitempty"` // their most read books
}

// AuthorHit is a search result: an author's id and the names it's known by.
type AuthorHit struct {
	ID         int64
	Name       string
	Alternates []string
	BooksCount int
}

type rawAuthor struct {
	ID            flexInt         `json:"id"`
	Name          string          `json:"name"`
	Slug          string          `json:"slug"`
	Bio           *string         `json:"bio"`
	BornYear      *int            `json:"born_year"`
	DeathYear     *int            `json:"death_year"`
	CachedImage   json.RawMessage `json:"cached_image"`
	BooksCount    int             `json:"books_count"`
	UsersCount    int             `json:"users_count"`
	Contributions []struct {
		Book *struct {
			Title string `json:"title"`
		} `json:"book"`
	} `json:"contributions"`
}

func (r rawAuthor) author() Author {
	a := Author{
		ID:         int64(r.ID),
		Name:       strings.TrimSpace(r.Name),
		Slug:       cleanSlug(r.Slug),
		ImageURL:   imageURL(r.CachedImage),
		BooksCount: r.BooksCount,
		UsersCount: r.UsersCount,
	}
	if r.Bio != nil {
		a.Bio = cleanBio(*r.Bio)
	}
	if r.BornYear != nil && *r.BornYear > 0 && *r.BornYear < 3000 {
		a.BornYear = *r.BornYear
	}
	if r.DeathYear != nil && *r.DeathYear > 0 && *r.DeathYear < 3000 {
		a.DeathYear = *r.DeathYear
	}
	for _, c := range r.Contributions {
		if c.Book != nil {
			if t := strings.TrimSpace(c.Book.Title); t != "" && !slices.Contains(a.Titles, t) {
				a.Titles = append(a.Titles, t)
			}
		}
	}
	return a
}

// cleanBio: line ends made "\n", at most one blank line between
// paragraphs, trimmed, and cut to maxBio at a rune boundary.
func cleanBio(s string) string {
	// Plain text: some bios carry HTML ("<sup>[1]</sup>" footnote marks,
	// "<br>"). The tags are dropped, never rendered.
	s = bioFootnoteRe.ReplaceAllString(s, "")
	s = bioBreakRe.ReplaceAllString(s, "\n")
	s = bioTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = bioCiteRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	var paras []string
	for _, p := range strings.Split(s, "\n\n") {
		lines := strings.Split(p, "\n")
		for i, l := range lines {
			lines[i] = strings.Join(strings.Fields(l), " ")
		}
		if p = strings.TrimSpace(strings.Join(lines, "\n")); p != "" {
			paras = append(paras, p)
		}
	}
	s = strings.Join(paras, "\n\n")
	if len(s) > maxBio {
		cut := maxBio
		for cut > 0 && !utf8RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut]) + "…"
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// AuthorsByName finds the authors named exactly one of names (at most 10
// names), most read first.
func (c *Client) AuthorsByName(ctx context.Context, names []string) ([]Author, error) {
	var clean []string
	for _, n := range names {
		if n = strings.Join(strings.Fields(n), " "); n != "" && !slices.Contains(clean, n) && len(clean) < 10 {
			clean = append(clean, n)
		}
	}
	if len(clean) == 0 {
		return nil, nil
	}
	return c.authors(ctx, qAuthorsByName, map[string]any{"names": clean})
}

// AuthorsByID fetches the authors with these ids (at most 10).
func (c *Client) AuthorsByID(ctx context.Context, ids []int64) ([]Author, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 10 {
		ids = ids[:10]
	}
	out, err := c.authors(ctx, qAuthorsByID, map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b Author) int { return slices.Index(ids, a.ID) - slices.Index(ids, b.ID) })
	return out, nil
}

func (c *Client) authors(ctx context.Context, q query, vars map[string]any) ([]Author, error) {
	var data struct {
		Authors []rawAuthor `json:"authors"`
	}
	if err := c.post(ctx, q, vars, &data); err != nil {
		return nil, err
	}
	var out []Author
	for _, r := range data.Authors {
		if a := r.author(); a.ID > 0 && a.Name != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

// SearchAuthors searches Hardcover's authors by name, best first.
func (c *Client) SearchAuthors(ctx context.Context, name string) ([]AuthorHit, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return nil, nil
	}
	var data struct {
		Search struct {
			Results json.RawMessage `json:"results"`
		} `json:"search"`
	}
	if err := c.post(ctx, qSearchAuthors, map[string]any{"q": name}, &data); err != nil {
		return nil, err
	}
	var results struct {
		Hits []struct {
			Document struct {
				ID         flexInt  `json:"id"`
				Name       string   `json:"name"`
				Alternates []string `json:"alternate_names"`
				BooksCount int      `json:"books_count"`
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
	var out []AuthorHit
	for _, h := range results.Hits {
		d := h.Document
		if d.ID <= 0 || strings.TrimSpace(d.Name) == "" {
			continue
		}
		if len(d.Alternates) > 50 {
			d.Alternates = d.Alternates[:50]
		}
		out = append(out, AuthorHit{ID: int64(d.ID), Name: strings.TrimSpace(d.Name), Alternates: d.Alternates, BooksCount: d.BooksCount})
	}
	return out, nil
}
