package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Placeholders that match a search are listed after every library book,
// whatever the order.
func TestSearchPlaceholders(t *testing.T) {
	searchFixture(t)
	setupTestAppDB(t)
	placeholdersChanged()
	t.Cleanup(placeholdersChanged)
	t.Chdir(t.TempDir())
	newPH := func(m OPFMetadata) *placeholder {
		t.Helper()
		p := &placeholder{Meta: m}
		if err := savePlaceholder(p, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// A title match: it would rank above author matches among library books.
	king := newPH(OPFMetadata{Title: "The Return of the King", Creators: []string{"J. R. R. Tolkien"}, Series: "The Lord of the Rings", SeriesIndex: "3"})
	silm := newPH(OPFMetadata{Title: "Tolkien's Silmarillion", Creators: []string{"J. R. R. Tolkien"}, Languages: []string{"eng"},
		Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "978-0-261-10273-6"}}})
	newPH(OPFMetadata{Title: "Night Watch", Creators: []string{"Terry Pratchett"}, Series: "Discworld", SeriesIndex: "29"})

	titles := func(q, sort string, page int) []string {
		t.Helper()
		rec := httptest.NewRecorder()
		searchHandler(rec, httptest.NewRequest("GET", "/api/search?q="+url.QueryEscape(q)+"&sort="+sort+"&page="+strconv.Itoa(page), nil))
		var books []Book
		if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
			t.Fatal(err, rec.Body)
		}
		var out []string
		for _, b := range books {
			if b.Placeholder != nil {
				out = append(out, "ph:"+b.Title)
			} else {
				out = append(out, b.Title)
			}
		}
		return out
	}

	got := titles("tolkien", "", 1)
	want := []string{"Tolkien: A Biography", "The Hobbit", "The Fellowship of the Ring", "The Two Towers",
		"ph:Tolkien's Silmarillion", "ph:The Return of the King"}
	if !slices.Equal(got, want) {
		t.Errorf("tolkien: %q, want %q", got, want)
	}
	for _, sort := range []string{"title", "added", "author"} {
		got := titles("tolkien", sort, 1)
		if len(got) != 6 || !strings.HasPrefix(got[4], "ph:") || !strings.HasPrefix(got[5], "ph:") {
			t.Errorf("tolkien by %s: %q: placeholders not last", sort, got)
		}
	}
	for q, want := range map[string][]string{
		"discworld":                  {"The Colour of Magic", "Mort", "ph:Night Watch"},
		"tolkien -silmarillion":      {"Tolkien: A Biography", "The Hobbit", "The Fellowship of the Ring", "The Two Towers", "ph:The Return of the King"},
		"isbn:9780261102736":         {"ph:Tolkien's Silmarillion"},
		"lang:english silmarillion":  {"ph:Tolkien's Silmarillion"},
		"format:epub return":         nil,
		`series:"lord of the rings"`: {"The Fellowship of the Ring", "The Two Towers", "ph:The Return of the King"},
	} {
		if got := titles(q, "", 1); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	// Its card leads to its page.
	res, _ := runSearch(t.Context(), "king")
	if phs := res.placeholders(); len(phs) != 1 || phs[0].UUID != king.UUID || phs[0].book().Placeholder.Href != "/missing/"+king.UUID {
		t.Errorf("king: %+v", phs)
	}

	// Edited or deleted: the cached search sees it.
	if err := deletePlaceholder(silm.UUID); err != nil {
		t.Fatal(err)
	}
	placeholdersChanged()
	if got := titles("silmarillion", "", 1); got != nil {
		t.Errorf("deleted placeholder still found: %q", got)
	}
}

// Paging runs from the library's books into the placeholders.
func TestSearchPlaceholderPages(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	placeholdersChanged()
	t.Cleanup(placeholdersChanged)
	t.Chdir(t.TempDir())
	for range booksPerPage + 2 {
		addTestBook(t, db, "Saga", []string{"W"}, nil, "", "epub")
	}
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	for range 3 {
		if err := savePlaceholder(&placeholder{Meta: OPFMetadata{Title: "Saga", Creators: []string{"W"}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	res, err := runSearch(t.Context(), "saga")
	if err != nil {
		t.Fatal(err)
	}
	p1, p2 := res.page(sortByBest, 1), res.page(sortByBest, 2)
	if len(p1) != booksPerPage || slices.ContainsFunc(p1, func(b Book) bool { return b.Placeholder != nil }) {
		t.Errorf("page 1: %d books, placeholders among them", len(p1))
	}
	if len(p2) != 5 || p2[0].Placeholder != nil || p2[1].Placeholder != nil || p2[2].Placeholder == nil || p2[4].Placeholder == nil {
		t.Errorf("page 2: %+v", p2)
	}
	if p3 := res.page(sortByBest, 3); len(p3) != 0 {
		t.Errorf("page 3: %d", len(p3))
	}
}
