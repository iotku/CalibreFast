package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestParseSearch(t *testing.T) {
	type want struct {
		field  searchField
		phrase string
		neg    bool
	}
	cases := map[string][]want{
		"tolkien hobbit":           {{fieldAny, "tolkien", false}, {fieldAny, "hobbit", false}},
		`"Lord of the Rings"`:      {{fieldAny, "lord of the rings", false}},
		"-abridged dune":           {{fieldAny, "abridged", true}, {fieldAny, "dune", false}},
		`author:"Terry Pratchett"`: {{fieldAuthor, "terry pratchett", false}},
		"Series:Discworld":         {{fieldSeries, "discworld", false}},
		"foo:bar":                  {{fieldAny, "foo bar", false}}, // not a field: plain words
		"100% _x_":                 {{fieldAny, "100", false}, {fieldAny, "x", false}},
		"  ... - ":                 nil,
		`"unclosed quote`:          {{fieldAny, "unclosed quote", false}},
		"Émile":                    {{fieldAny, "emile", false}},
	}
	for q, w := range cases {
		got := parseSearch(q)
		var g []want
		for _, t := range got {
			g = append(g, want{t.field, t.phrase, t.neg})
		}
		if !slices.Equal(g, w) {
			t.Errorf("parseSearch(%q) = %+v, want %+v", q, g, w)
		}
	}
	if ts := parseSearch("978-0-261-10221-7"); len(ts) != 1 || ts[0].isbn != "9780261102217" {
		t.Errorf("ISBN with dashes: %+v", ts)
	}
	if ts := parseSearch("isbn:0261102214"); len(ts) != 1 || ts[0].field != fieldISBN || ts[0].isbn != "9780261102217" {
		t.Errorf("ISBN-10: %+v", ts)
	}
	if n := len(parseSearch(strings.Repeat("a ", 100))); n != searchMaxTerms {
		t.Errorf("%d terms, want at most %d", n, searchMaxTerms)
	}
}

// searchFixture: a small library in the catalog, as the server has it.
func searchFixture(t *testing.T) map[string]string {
	t.Helper()
	db := catalogFixture(t)
	ids := map[string]string{
		"hobbit":   addTestBook(t, db, "The Hobbit", []string{"J. R. R. Tolkien"}, []string{"Fantasy", "Classics"}, "Middle-earth", "epub"),
		"fellow":   addTestBook(t, db, "The Fellowship of the Ring", []string{"J. R. R. Tolkien"}, []string{"Fantasy"}, "The Lord of the Rings", "epub", "pdf"),
		"towers":   addTestBook(t, db, "The Two Towers", []string{"J. R. R. Tolkien"}, []string{"Fantasy"}, "The Lord of the Rings", "epub"),
		"colour":   addTestBook(t, db, "The Colour of Magic", []string{"Terry Pratchett"}, []string{"Fantasy", "Humour"}, "Discworld", "epub"),
		"mort":     addTestBook(t, db, "Mort", []string{"Terry Pratchett"}, []string{"Fantasy"}, "Discworld", "pdf"),
		"emile":    addTestBook(t, db, "Émile, ou De l'éducation", []string{"Jean-Jacques Rousseau"}, []string{"Philosophy"}, "", "epub"),
		"tolkienB": addTestBook(t, db, "Tolkien: A Biography", []string{"Humphrey Carpenter"}, []string{"Biography"}, "", "epub"),
		"pct":      addTestBook(t, db, "100% Fun_Facts", []string{"Anon"}, nil, ""),
	}
	set := func(book, q string, args ...any) {
		mustExec(t, db, q, append(args, ids[book])...)
	}
	// Reading order within a series, opposite to title order.
	set("fellow", `UPDATE books SET series_index = 1 WHERE uuid = ?`)
	set("towers", `UPDATE books SET series_index = 2 WHERE uuid = ?`)
	set("colour", `UPDATE books SET series_index = 1 WHERE uuid = ?`)
	set("mort", `UPDATE books SET series_index = 4 WHERE uuid = ?`)
	mustExec(t, db, `INSERT INTO publishers (name) VALUES ('Allen & Unwin'), ('Gollancz')`)
	set("hobbit", `INSERT INTO books_publishers_link (book, publisher) SELECT b.id, p.id FROM books b, publishers p WHERE p.name = 'Allen & Unwin' AND b.uuid = ?`)
	set("colour", `INSERT INTO books_publishers_link (book, publisher) SELECT b.id, p.id FROM books b, publishers p WHERE p.name = 'Gollancz' AND b.uuid = ?`)
	mustExec(t, db, `INSERT INTO languages (lang_code) VALUES ('eng'), ('fra')`)
	for _, b := range []string{"hobbit", "fellow", "towers", "colour", "mort", "tolkienB"} {
		set(b, `INSERT INTO books_languages_link (book, lang_code, item_order) SELECT b.id, l.id, 0 FROM books b, languages l WHERE l.lang_code = 'eng' AND b.uuid = ?`)
	}
	set("emile", `INSERT INTO books_languages_link (book, lang_code, item_order) SELECT b.id, l.id, 0 FROM books b, languages l WHERE l.lang_code = 'fra' AND b.uuid = ?`)
	set("hobbit", `INSERT INTO identifiers (book, type, val) SELECT id, 'isbn', '0-261-10221-4' FROM books WHERE uuid = ?`)
	set("mort", `INSERT INTO identifiers (book, type, val) SELECT id, 'ISBN', '9780552131063' FROM books WHERE uuid = ?`)
	set("towers", `INSERT INTO identifiers (book, type, val) SELECT id, 'amazon', '0261102214' FROM books WHERE uuid = ?`)

	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	return ids
}

func searchNames(t *testing.T, ids map[string]string, q string) []string {
	t.Helper()
	byUUID := map[string]string{}
	for k, v := range ids {
		byUUID[v] = k
	}
	res := runSearch(q)
	var got []string
	for _, s := range res.slots {
		got = append(got, byUUID[res.ix.cat.books[s].UUID])
	}
	return got
}

func TestSearchCatalog(t *testing.T) {
	ids := searchFixture(t)
	cases := []struct {
		q    string
		want []string
	}{
		// Title matches outrank author matches; equal ones go series by series in reading order.
		{"tolkien", []string{"tolkienB", "hobbit", "fellow", "towers"}},
		{"tolkien hobbit", []string{"hobbit"}},
		{"tolk", []string{"tolkienB", "hobbit", "fellow", "towers"}},
		{"emile", []string{"emile"}},
		{"EDUCATION", []string{"emile"}},
		{"discworld", []string{"colour", "mort"}},
		{`"lord of the rings"`, []string{"fellow", "towers"}},
		{"rings", []string{"fellow", "towers"}},
		{"ring", []string{"fellow", "towers"}},
		{"tolkien -fellowship", []string{"tolkienB", "hobbit", "towers"}},
		{"humour", []string{"colour"}},
		{"gollancz", []string{"colour"}},
		{"allen unwin", []string{"hobbit"}},
		{"author:tolkien", []string{"hobbit", "fellow", "towers"}},
		{"title:tolkien", []string{"tolkienB"}},
		{"series:discworld", []string{"colour", "mort"}},
		{"tag:fantasy pratchett", []string{"colour", "mort"}},
		{"publisher:gollancz", []string{"colour"}},
		{"lang:fra", []string{"emile"}},
		{"lang:fr", []string{"emile"}},
		{"lang:french", []string{"emile"}},
		{"format:pdf", []string{"mort", "fellow"}}, // Discworld, then The Lord of the Rings
		{"format:.PDF", []string{"mort", "fellow"}},
		{"978-0-261-10221-7", []string{"hobbit"}}, // stored as an ISBN-10
		{"0261102214", []string{"hobbit"}},        // not the Amazon id of the same digits
		{"isbn:9780552131063", []string{"mort"}},
		{"isbn:131063", []string{"mort"}},
		{"100%", []string{"pct"}},
		{"_", nil},
		{"%", nil},
		{"fun_facts", []string{"pct"}},
		{"-fantasy", nil},
		{"zzz", nil},
		{"the", []string{"colour", "hobbit", "fellow", "towers"}},
	}
	for _, c := range cases {
		got := searchNames(t, ids, c.q)
		if !slices.Equal(got, c.want) {
			t.Errorf("search %q = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestSearchFollowsCatalogChanges(t *testing.T) {
	ids := searchFixture(t)
	if got := searchNames(t, ids, "mort"); len(got) != 1 {
		t.Fatalf("before: %v", got)
	}
	catalogRemoved(ids["mort"])
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, ids, "mort"); len(got) != 0 {
		t.Errorf("a removed book is still found: %v", got)
	}

	mustExec(t, calibreDB, `UPDATE books SET title = 'Smaug' WHERE uuid = ?`, ids["hobbit"])
	mustExec(t, calibreDB, `INSERT INTO publishers (name) VALUES ('Brand New Press')`)
	mustExec(t, calibreDB, `INSERT INTO books_publishers_link (book, publisher) SELECT b.id, p.id FROM books b, publishers p WHERE p.name = 'Brand New Press' AND b.uuid = ?`, ids["emile"])
	catalogChanged(ids["hobbit"], ids["emile"])
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if got := searchNames(t, ids, "smaug"); !slices.Equal(got, []string{"hobbit"}) {
		t.Errorf("edited title: %v", got)
	}
	if got := searchNames(t, ids, "brand new press"); !slices.Equal(got, []string{"emile"}) {
		t.Errorf("new publisher: %v", got)
	}
	if got := searchNames(t, ids, "isbn:9780261102217"); !slices.Equal(got, []string{"hobbit"}) {
		t.Errorf("ISBN lost on re-read: %v", got)
	}
}

func TestSearchLinks(t *testing.T) {
	searchFixture(t)
	name := func(q string) string {
		var parts []string
		for _, g := range runSearch(q).links() {
			var ns []string
			for _, l := range g.Links {
				ns = append(ns, l.Name+"="+l.Href)
			}
			parts = append(parts, g.Label+": "+strings.Join(ns, ", "))
		}
		return strings.Join(parts, "; ")
	}
	for q, want := range map[string]string{
		"discworld":      "Series: Discworld=/series/Discworld",
		"tolkien":        "Authors: J. R. R. Tolkien=/author/J.%20R.%20R.%20Tolkien",
		"lord rings":     "Series: The Lord of the Rings=/series/The%20Lord%20of%20the%20Rings",
		"fantasy":        "Tags: Fantasy=/tag/Fantasy",
		"tolkien hobbit": "",
		"author:tolkien": "",
	} {
		if got := name(q); got != want {
			t.Errorf("links for %q = %q, want %q", q, got, want)
		}
	}
}

func TestSearchHandlers(t *testing.T) {
	ids := searchFixture(t)
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q="+url.QueryEscape("pratchett"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var books []Book
	if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
		t.Fatal(err)
	}
	if len(books) != 2 || books[0].UUID != ids["colour"] || books[0].Authors != "Terry Pratchett" || books[0].Path == "" {
		t.Errorf("got %+v", books)
	}
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=pratchett&page=2", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("page past the end: %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q=+", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty q: status %d", rec.Code)
	}
}

// The search page escapes what it shows: a query or a name with markup in it
// stays text.
func TestSearchPageEscapes(t *testing.T) {
	db := catalogFixture(t)
	addTestBook(t, db, `x"><script>alert(1)</script>`, []string{`<img src=x onerror=alert(1)>`}, []string{`javascript:alert(1)`}, "")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	res := runSearch("alert")
	data := SearchPageData{Title: "T", Query: `alert"><script>`, TotalBooks: len(res.slots), Links: res.links()}
	if len(data.Links) == 0 {
		t.Fatal("expected links")
	}
	var b strings.Builder
	if err := templates.ExecuteTemplate(&b, "search.html", data); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, bad := range []string{"<script>alert", "<img src=x", `href="javascript:`} {
		if strings.Contains(out, bad) {
			t.Errorf("unescaped %q in the search page", bad)
		}
	}
}

func BenchmarkSearch(b *testing.B) {
	c := newCatalog()
	for i := range 50000 {
		c.authorNames[int32(i%5000+1)] = fmt.Sprintf("Author %d Surname%d", i%5000, i%97)
		c.tagNames[int32(i%300+1)] = fmt.Sprintf("Tag %d", i%300)
		c.seriesNames[int32(i%2000+1)] = fmt.Sprintf("Series %d Saga", i%2000)
		c.books = append(c.books, catalogBook{
			Book:      Book{Title: fmt.Sprintf("The Book of Things Number %d", i), UUID: fmt.Sprint(i)},
			Sort:      fmt.Sprintf("Book of Things Number %d", i),
			AuthorIDs: []int32{int32(i%5000 + 1)},
			TagIDs:    []int32{int32(i%300 + 1), int32((i+7)%300 + 1)},
			Series:    int32(i%2000 + 1),
		})
		c.byUUID[fmt.Sprint(i)] = int32(i)
	}
	ix := newSearchIndex(c)
	b.Run("index", func(b *testing.B) {
		for b.Loop() {
			newSearchIndex(c)
		}
	})
	for _, q := range []string{"surname42 things", "saga", "zzzz", `"number 4999"`} {
		terms := parseSearch(q)
		b.Run(q, func(b *testing.B) {
			for b.Loop() {
				ix.search(terms)
			}
		})
	}
}
