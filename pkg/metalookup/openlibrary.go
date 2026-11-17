package metalookup

import (
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

	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/langcodes"
	"github.com/iotku/CalibreFast/pkg/opf"
)

// Open Library: https://openlibrary.org/developers/api

type olKey struct {
	Key string `json:"key"`
}

// olText is a description: a string, or {"type": "/type/text", "value": …}.
type olText string

func (t *olText) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = olText(s)
		return nil
	}
	var v struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*t = olText(v.Value)
	return nil
}

type olEdition struct {
	Key          string   `json:"key"` // "/books/OL…M"
	Title        string   `json:"title"`
	Subtitle     string   `json:"subtitle"`
	Publishers   []string `json:"publishers"`
	PublishDate  string   `json:"publish_date"`
	Languages    []olKey  `json:"languages"` // "/languages/eng"
	Authors      []olKey  `json:"authors"`
	Works        []olKey  `json:"works"`
	Covers       []int64  `json:"covers"`
	Subjects     []string `json:"subjects"`
	Description  olText   `json:"description"`
	Contributors []struct {
		Role string `json:"role"`
		Name string `json:"name"`
	} `json:"contributors"`
}

type olWork struct {
	Key         string   `json:"key"` // "/works/OL…W"
	Description olText   `json:"description"`
	Subjects    []string `json:"subjects"`
	Authors     []struct {
		Author olKey `json:"author"`
	} `json:"authors"`
	Covers []int64 `json:"covers"`
}

type olSearchDoc struct {
	Key              string   `json:"key"` // "/works/OL…W"
	Title            string   `json:"title"`
	Subtitle         string   `json:"subtitle"`
	AuthorName       []string `json:"author_name"`
	FirstPublishYear int      `json:"first_publish_year"`
	Publisher        []string `json:"publisher"`
	Language         []string `json:"language"`
	Subject          []string `json:"subject"`
	CoverI           int64    `json:"cover_i"`
	ISBN             []string `json:"isbn"` // every edition's
}

// olKeyRe checks keys from Open Library before they go into a URL.
var olKeyRe = regexp.MustCompile(`^/(books|works|authors)/OL[0-9]+[MWA]$`)

func (c *Client) openLibraryLookup(ctx context.Context, q Query) ([]Match, error) {
	if q.ISBN == "" {
		return c.openLibrarySearch(ctx, q)
	}
	var ed olEdition
	if found, err := c.getJSON(ctx, OpenLibrary, c.openLibrary+"/isbn/"+q.ISBN+".json", &ed); err != nil || !found {
		return nil, err
	}
	var work olWork
	if len(ed.Works) > 0 && olKeyRe.MatchString(ed.Works[0].Key) {
		// Best effort: the edition alone is still worth showing.
		if _, err := c.getJSON(ctx, OpenLibrary, c.openLibrary+ed.Works[0].Key+".json", &work); err != nil {
			c.logf("lookup: Open Library work %s: %v", ed.Works[0].Key, err)
		}
	}

	authorKeys := ed.Authors
	if len(authorKeys) == 0 {
		for _, a := range work.Authors {
			authorKeys = append(authorKeys, a.Author)
		}
	}
	authors := c.olAuthorNames(ctx, authorKeys)
	if len(authors) == 0 {
		for _, c := range ed.Contributors {
			if strings.EqualFold(c.Role, "author") && c.Name != "" {
				authors = append(authors, c.Name)
			}
		}
	}

	m := &opf.Metadata{
		Title:       joinSubtitle(ed.Title, ed.Subtitle),
		Creators:    authors,
		Publisher:   firstNonEmpty(ed.Publishers...),
		Date:        lookupDate(ed.PublishDate),
		Description: textToHTML(firstNonEmpty(string(ed.Description), string(work.Description))),
		Subjects:    cleanOLSubjects(append(slices.Clone(work.Subjects), ed.Subjects...)),
		Identifiers: []opf.Identifier{{Scheme: "isbn", Value: q.ISBN}},
	}
	for _, lang := range ed.Languages {
		if code := strings.TrimPrefix(lang.Key, "/languages/"); code != "" {
			m.Languages = append(m.Languages, langcodes.Canonical(code))
		}
	}
	if olKeyRe.MatchString(ed.Key) {
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: "olid", Value: strings.TrimPrefix(ed.Key, "/books/")})
	}

	return []Match{{Meta: m, CoverID: firstCover(ed.Covers, work.Covers)}}, nil
}

// openLibrarySearch finds the best matches for a title and author (up to
// maxSearchMatches), as works (all their editions together), so without an
// ISBN. Their works are fetched at once, for their descriptions.
func (c *Client) openLibrarySearch(ctx context.Context, q Query) ([]Match, error) {
	v := url.Values{}
	v.Set("title", q.Title)
	if q.Author != "" {
		v.Set("author", q.Author)
	}
	v.Set("limit", strconv.Itoa(maxSearchMatches))
	v.Set("fields", "key,title,subtitle,author_name,first_publish_year,publisher,language,subject,cover_i,isbn")
	var found struct {
		Docs []olSearchDoc `json:"docs"`
	}
	if ok, err := c.getJSON(ctx, OpenLibrary, c.openLibrary+"/search.json?"+v.Encode(), &found); err != nil || !ok {
		return nil, err
	}
	docs := found.Docs[:min(len(found.Docs), maxSearchMatches)]
	works := make([]olWork, len(docs))
	var wg sync.WaitGroup
	for i, doc := range docs {
		if !olKeyRe.MatchString(doc.Key) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Best effort: the search's values are still worth showing.
			if _, err := c.getJSON(ctx, OpenLibrary, c.openLibrary+doc.Key+".json", &works[i]); err != nil {
				c.logf("lookup: Open Library work %s: %v", doc.Key, err)
			}
		}()
	}
	wg.Wait()

	matches := make([]Match, 0, len(docs))
	for i, doc := range docs {
		m := &opf.Metadata{
			Title:       joinSubtitle(doc.Title, doc.Subtitle),
			Creators:    doc.AuthorName,
			Publisher:   firstNonEmpty(doc.Publisher...),
			Description: textToHTML(string(works[i].Description)),
			Subjects:    cleanOLSubjects(doc.Subject),
		}
		if doc.FirstPublishYear > 0 {
			m.Date = fmt.Sprintf("%04d-01-01", doc.FirstPublishYear)
		}
		if len(doc.Language) == 1 { // more than one: editions in several languages
			m.Languages = []string{langcodes.Canonical(doc.Language[0])}
		}
		if olKeyRe.MatchString(doc.Key) {
			m.Identifiers = []opf.Identifier{{Scheme: "olid", Value: strings.TrimPrefix(doc.Key, "/works/")}}
		}
		isbns := map[string]bool{}
		for _, raw := range doc.ISBN {
			if n := isbn.Normalize(raw); n != "" {
				isbns[n] = true
			}
		}
		matches = append(matches, Match{Meta: m, CoverID: firstCover([]int64{doc.CoverI}, works[i].Covers), isbns: isbns})
	}
	return matches, nil
}

// olAuthorNames fetches author records (at most 5, at once) for their names.
func (c *Client) olAuthorNames(ctx context.Context, keys []olKey) []string {
	keys = keys[:min(len(keys), 5)]
	names := make([]string, len(keys))
	var wg sync.WaitGroup
	for i, k := range keys {
		if !olKeyRe.MatchString(k.Key) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var a struct {
				Name string `json:"name"`
			}
			if _, err := c.getJSON(ctx, OpenLibrary, c.openLibrary+k.Key+".json", &a); err != nil {
				c.logf("lookup: Open Library author %s: %v", k.Key, err)
			}
			names[i] = strings.TrimSpace(a.Name)
		}()
	}
	wg.Wait()
	return slices.DeleteFunc(names, func(s string) bool { return s == "" })
}

// olJunkSubjects are Open Library's own shelving notes, not subjects.
var olJunkSubjects = map[string]bool{
	"accessible book": true, "protected daisy": true, "in library": true, "lending library": true,
	"large type books": true, "overdrive": true, "internet archive wishlist": true,
}

// cleanOLSubjects drops shelving notes and tagged entries ("nyt:…",
// "collectionID:…"), and repeats, keeping the first 15.
func cleanOLSubjects(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		k := strings.ToLower(s)
		if s == "" || strings.Contains(s, ":") || olJunkSubjects[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
		if len(out) == 15 {
			break
		}
	}
	return out
}

func firstCover(lists ...[]int64) int64 {
	for _, l := range lists {
		for _, id := range l {
			if id > 0 { // -1 marks a deleted cover
				return id
			}
		}
	}
	return 0
}

var olCoverIDRe = regexp.MustCompile(`^[0-9]{1,12}$`)

// Errors from FetchCover.
var (
	// ErrBadCoverID: the id isn't an Open Library cover id.
	ErrBadCoverID = errors.New("unknown cover")
	// ErrNoCover: Open Library has no image for the id.
	ErrNoCover = errors.New("no cover")
	// ErrCoverUnreadable: the image couldn't be read, or is over 10 MiB.
	ErrCoverUnreadable = errors.New("cover unavailable")
)

// FetchCover fetches Open Library's large cover image for a cover id (a
// Match's CoverID, as a string), within Open Library's limits. The URL is
// built here from the id, so it can't be pointed anywhere else. It returns
// the image and its Content-Type.
func (c *Client) FetchCover(ctx context.Context, id string) ([]byte, string, error) {
	if !olCoverIDRe.MatchString(id) {
		return nil, "", ErrBadCoverID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.olCovers+"/b/id/"+id+"-L.jpg?default=false", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.do(OpenLibrary, req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	ctype := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ctype, "image/") {
		return nil, "", ErrNoCover
	}
	const maxCover = 10 << 20
	img, err := io.ReadAll(io.LimitReader(resp.Body, maxCover+1))
	if err != nil || len(img) > maxCover {
		return nil, "", ErrCoverUnreadable
	}
	return img, ctype, nil
}
