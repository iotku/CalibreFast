package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func getSuggest(t *testing.T, q string) (int, map[string]json.RawMessage, suggestResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	searchSuggestHandler(rec, httptest.NewRequest("GET", "/api/search/suggest?q="+url.QueryEscape(q), nil))
	var raw map[string]json.RawMessage
	var res suggestResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("%q: %v (%s)", q, err, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, raw, res
}

// The top bar's suggestions: the books and links /search finds, cut to a
// few, as JSON of a fixed shape.
func TestSearchSuggest(t *testing.T) {
	ids := searchFixture(t)

	code, raw, res := getSuggest(t, "tolkien")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(raw) != 2 || raw["books"] == nil || raw["links"] == nil {
		t.Errorf("keys: %v", raw)
	}
	var books []map[string]any
	if err := json.Unmarshal(raw["books"], &books); err != nil || len(books) == 0 {
		t.Fatalf("books: %s", raw["books"])
	}
	for _, k := range []string{"uuid", "title", "authors", "series", "series_index"} {
		if _, ok := books[0][k]; !ok {
			t.Errorf("book has no %q: %v", k, books[0])
		}
	}
	if len(books[0]) != 5 {
		t.Errorf("book fields: %v", books[0])
	}
	var links []map[string]any
	if err := json.Unmarshal(raw["links"], &links); err != nil || len(links) == 0 {
		t.Fatalf("links: %s", raw["links"])
	}
	if l := links[0]; len(l) != 4 || l["kind"] != "author" || l["name"] != "J. R. R. Tolkien" ||
		l["href"] != "/author/J.%20R.%20R.%20Tolkien" || l["count"] != float64(3) {
		t.Errorf("link: %v", l)
	}
	found := map[string]suggestBook{}
	for _, b := range res.Books {
		found[b.UUID] = b
	}
	if b := found[ids["towers"]]; b.Title != "The Two Towers" || b.Series != "The Lord of the Rings" || b.SeriesIndex != 2 || b.Authors != "J. R. R. Tolkien" {
		t.Errorf("towers: %+v", b)
	}

	// Nothing to look for, or nothing found: empty lists, not null.
	for _, q := range []string{"", "  ", "zzzzqqq"} {
		code, raw, _ := getSuggest(t, q)
		if code != http.StatusOK || string(raw["books"]) != "[]" || string(raw["links"]) != "[]" {
			t.Errorf("%q: %d %s %s", q, code, raw["books"], raw["links"])
		}
	}
}

// At most suggestBooks books and suggestLinks links, the links shared
// between kinds; a long query is cut as /search cuts it.
func TestSearchSuggestLimits(t *testing.T) {
	db := catalogFixture(t)
	for i := range 30 {
		addTestBook(t, db, fmt.Sprintf("Zebra Book %d", i), []string{fmt.Sprintf("Zebra Writer %d", i)}, []string{fmt.Sprintf("Zebra Tag %d", i)}, fmt.Sprintf("Zebra Saga %d", i))
	}
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)

	code, _, res := getSuggest(t, "zebra")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(res.Books) != suggestBooks {
		t.Errorf("%d books, want %d", len(res.Books), suggestBooks)
	}
	if len(res.Links) != suggestLinks {
		t.Fatalf("%d links, want %d", len(res.Links), suggestLinks)
	}
	kinds := map[string]int{}
	for _, l := range res.Links {
		kinds[l.Kind]++
		if !strings.HasPrefix(l.Href, "/") || strings.HasPrefix(l.Href, "//") || l.Count != 1 {
			t.Errorf("link %+v", l)
		}
	}
	if kinds["author"] != 2 || kinds["series"] != 2 || kinds["tag"] != 2 {
		t.Errorf("kinds %v, want two of each", kinds)
	}
	// Kinds in order: authors, then series, then tags.
	if res.Links[0].Kind != "author" || res.Links[5].Kind != "tag" {
		t.Errorf("order: %+v", res.Links)
	}

	if code, _, _ := getSuggest(t, strings.Repeat("zebra ", 500)); code != http.StatusOK {
		t.Errorf("long query: %d", code)
	}
}

// An author whose name has a comma (stored with "|" in metadata.db) comes
// back with the comma, in the book's authors and in the link.
func TestSearchSuggestAuthorWithComma(t *testing.T) {
	db := catalogFixture(t)
	uuid := addTestBook(t, db, "A Commonplace Book", []string{"Ashton, Hugh"}, nil, "")
	var stored string
	if err := db.QueryRow(`SELECT name FROM authors WHERE name LIKE 'Ashton%'`).Scan(&stored); err != nil || stored != "Ashton| Hugh" {
		t.Fatalf("stored as %q (%v)", stored, err)
	}
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)

	_, _, res := getSuggest(t, "ashton")
	if len(res.Books) != 1 || res.Books[0].UUID != uuid || res.Books[0].Authors != "Ashton, Hugh" {
		t.Errorf("books: %+v", res.Books)
	}
	if len(res.Links) != 1 || res.Links[0].Name != "Ashton, Hugh" || res.Links[0].Href != "/author/Ashton%2C%20Hugh" {
		t.Errorf("links: %+v", res.Links)
	}
}

// The suggestions need the same sign-in as /api/search: none on a -public
// server, a session on a private one.
func TestSearchSuggestAuthMatchesSearch(t *testing.T) {
	setupTestAppDB(t)
	searchFixture(t)
	orig := publicCatalog
	t.Cleanup(func() { publicCatalog = orig })

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/search", searchHandler)
	mux.HandleFunc("GET /api/search/suggest", searchSuggestHandler)
	h := withAuth(mux)

	u, err := createUser("reader", "longenough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := startSession(rec, httptest.NewRequest("POST", "/login", nil), u.ID); err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()

	get := func(path string, signedIn bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if signedIn {
			for _, c := range cookies {
				req.AddCookie(c)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, public := range []bool{false, true} {
		publicCatalog = public
		for _, signedIn := range []bool{false, true} {
			search := get("/api/search?q=tolkien", signedIn)
			suggest := get("/api/search/suggest?q=tolkien", signedIn)
			if search.Code != suggest.Code {
				t.Errorf("public=%v signedIn=%v: /api/search %d, suggest %d", public, signedIn, search.Code, suggest.Code)
			}
			if suggest.Code != http.StatusOK && strings.Contains(suggest.Body.String(), "Hobbit") {
				t.Errorf("public=%v signedIn=%v: refused but showed books", public, signedIn)
			}
		}
	}
	publicCatalog = false
	if rec := get("/api/search/suggest?q=tolkien", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out, private: %d", rec.Code)
	}

	// Only GET.
	req := httptest.NewRequest("POST", "/api/search/suggest?q=tolkien", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}
