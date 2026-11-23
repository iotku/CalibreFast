package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// sortFixture: books whose orders all differ. Added in this order (so
// Latest, newest first, is the reverse).
func sortFixture(t *testing.T) map[string]string {
	t.Helper()
	db := catalogFixture(t)
	ids := map[string]string{
		"zebra":  addTestBook(t, db, "Zebra Tales", []string{"Amy Adams"}, nil, "Beta Saga"),
		"apple":  addTestBook(t, db, "The Apple", []string{"Zed Zulu"}, nil, "Alpha Cycle"),
		"mango":  addTestBook(t, db, "Mango", []string{"Molly Moss"}, nil, "Alpha Cycle"),
		"noauth": addTestBook(t, db, "Kiwi", []string{"Temp Author"}, nil, ""),
		"nodate": addTestBook(t, db, "Banana", []string{"Bob Brown"}, nil, ""),
	}
	set := func(book, q string, args ...any) { mustExec(t, db, q, append(args, ids[book])...) }
	set("apple", `UPDATE books SET series_index = 2, pubdate = '1990-05-01 00:00:00+00:00' WHERE uuid = ?`)
	set("mango", `UPDATE books SET series_index = 1, pubdate = '2020-01-01 00:00:00+00:00' WHERE uuid = ?`)
	set("zebra", `UPDATE books SET series_index = 1, pubdate = '2001-01-01 00:00:00+00:00' WHERE uuid = ?`)
	set("noauth", `UPDATE books SET pubdate = '1950-01-01 00:00:00+00:00' WHERE uuid = ?`)
	set("nodate", `UPDATE books SET pubdate = '0101-01-01 00:00:00+00:00' WHERE uuid = ?`) // Calibre's "undefined"
	mustExec(t, db, `DELETE FROM books_authors_link WHERE book = (SELECT id FROM books WHERE uuid = ?)`, ids["noauth"])
	set("noauth", `UPDATE books SET author_sort = '' WHERE uuid = ?`)
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	return ids
}

func names(ids map[string]string, uuids []string) []string {
	by := map[string]string{}
	for k, v := range ids {
		by[v] = k
	}
	var out []string
	for _, u := range uuids {
		out = append(out, by[u])
	}
	return out
}

func TestSortSlots(t *testing.T) {
	ids := sortFixture(t)
	c := currentCatalog()
	want := map[string][]string{
		sortByAdded:     {"nodate", "noauth", "mango", "apple", "zebra"},
		sortByTitle:     {"apple", "nodate", "noauth", "mango", "zebra"}, // "Apple, The"
		sortByAuthor:    {"zebra", "nodate", "mango", "apple", "noauth"}, // Adams, Brown, Moss, Zulu; none last
		sortBySeries:    {"mango", "apple", "zebra", "nodate", "noauth"}, // Alpha 1, 2; Beta; none (by title)
		sortByPublished: {"mango", "zebra", "apple", "noauth", "nodate"}, // newest first; undefined last
	}
	for key, w := range want {
		order := latestOrder(c, key)
		var uuids []string
		for _, s := range order {
			uuids = append(uuids, c.books[s].UUID)
		}
		if got := names(ids, uuids); !slices.Equal(got, w) {
			t.Errorf("%s: %v, want %v", key, got, w)
		}
	}
}

func TestLatestSortedAPI(t *testing.T) {
	ids := sortFixture(t)
	get := func(q string) []string {
		rec := httptest.NewRecorder()
		latestSortedHandler(rec, httptest.NewRequest("GET", "/api/latest?"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", q, rec.Code)
		}
		var books []Book
		if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
			t.Fatal(err)
		}
		var u []string
		for _, b := range books {
			u = append(u, b.UUID)
		}
		return names(ids, u)
	}
	io := libraryIO.Load()
	if got := get("sort=title&page=1"); !slices.Equal(got, []string{"apple", "nodate", "noauth", "mango", "zebra"}) {
		t.Errorf("title: %v", got)
	}
	if got := get("sort=title&page=2"); len(got) != 0 {
		t.Errorf("page 2: %v", got)
	}
	// Anything else is the default order.
	if got := get("sort=b.id;DROP%20TABLE%20books&page=1"); !slices.Equal(got, []string{"nodate", "noauth", "mango", "apple", "zebra"}) {
		t.Errorf("unknown sort: %v", got)
	}
	if libraryIO.Load() != io {
		t.Error("sorted Latest read the library")
	}
}

func TestSearchSorted(t *testing.T) {
	ids := sortFixture(t)
	get := func(q string) []string {
		rec := httptest.NewRecorder()
		searchHandler(rec, httptest.NewRequest("GET", "/api/search?"+q, nil))
		var books []Book
		if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
			t.Fatal(err)
		}
		var u []string
		for _, b := range books {
			u = append(u, b.UUID)
		}
		return names(ids, u)
	}
	io := libraryIO.Load()
	if got := get("q=alpha&sort=published"); !slices.Equal(got, []string{"mango", "apple"}) {
		t.Errorf("published: %v", got)
	}
	if got := get("q=alpha&sort=title"); !slices.Equal(got, []string{"apple", "mango"}) {
		t.Errorf("title: %v", got)
	}
	if got := get("q=alpha"); !slices.Equal(got, []string{"mango", "apple"}) { // relevance: the series in order
		t.Errorf("best: %v", got)
	}
	if libraryIO.Load() != io {
		t.Error("sorted search read the library")
	}
}

// Every sort a filtered page offers runs, in the order it says; an unknown
// one is the page's default.
func TestFilteredSorts(t *testing.T) {
	ids := sortFixture(t)
	mustExec(t, calibreDB, `INSERT INTO tags (name) VALUES ('Fruit')`)
	mustExec(t, calibreDB, `INSERT INTO books_tags_link (book, tag) SELECT id, (SELECT id FROM tags WHERE name = 'Fruit') FROM books`)
	get := func(path string) []string {
		c := categoryByKey(strings.Split(path, "/")[1])
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc("/"+c.URLPrefix+"/", filteredPageHandler(c))
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var books []Book
		if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var u []string
		for _, b := range books {
			u = append(u, b.UUID)
		}
		return names(ids, u)
	}
	for q, want := range map[string][]string{
		"":                {"apple", "nodate", "noauth", "mango", "zebra"}, // title
		"&sort=title":     {"apple", "nodate", "noauth", "mango", "zebra"},
		"&sort=author":    {"zebra", "nodate", "mango", "apple", "noauth"},
		"&sort=series":    {"mango", "apple", "zebra", "nodate", "noauth"},
		"&sort=added":     {"nodate", "noauth", "mango", "apple", "zebra"},
		"&sort=published": {"mango", "zebra", "apple", "noauth", "nodate"},
		"&sort=nope":      {"apple", "nodate", "noauth", "mango", "zebra"},
	} {
		if got := get("/tag/Fruit?api=1" + q); !slices.Equal(got, want) {
			t.Errorf("tag%s: %v, want %v", q, got, want)
		}
	}
	// A series page reads in order by default.
	if got := get("/series/Alpha%20Cycle?api=1"); !slices.Equal(got, []string{"mango", "apple"}) {
		t.Errorf("series default: %v", got)
	}
	if got := get("/series/Alpha%20Cycle?api=1&sort=title"); !slices.Equal(got, []string{"apple", "mango"}) {
		t.Errorf("series by title: %v", got)
	}
}

// Latest is always newest added first, from the page files: it names its
// order, with no select, and whatever ?sort= holds never reaches the page.
func TestGridSortTemplate(t *testing.T) {
	sortFixture(t)
	t.Chdir(t.TempDir())
	for _, q := range []string{"", "?sort=title", `?sort="><script>alert(1)</script>`} {
		rec := httptest.NewRecorder()
		latestHandler(rec, httptest.NewRequest("GET", "/latest"+q, nil))
		out := rec.Body.String()
		if strings.Contains(out, "<script>alert") {
			t.Errorf("%q reached the page", q)
		}
		if !strings.Contains(out, `Sorted by <span class="grid-sort-fixed">Recently added</span>`) || strings.Contains(out, "grid-sort-select") {
			t.Errorf("%q: not a fixed sort", q)
		}
		if !strings.Contains(out, `data-page-url="/pages/page{n}.json`) {
			i := strings.Index(out, "data-page-url")
			t.Errorf("%q: page URL not the page files': %.120s", q, out[max(i, 0):])
		}
	}
	// Pages with a choice still offer the select, with only allow-listed values.
	c := categoryByKey("series")
	mux := http.NewServeMux()
	mux.HandleFunc("/series/", filteredPageHandler(c))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", `/series/Alpha%20Cycle?sort="><script>alert(1)</script>`, nil))
	if out := rec.Body.String(); strings.Contains(out, "<script>alert") || !strings.Contains(out, "grid-sort-select") {
		t.Errorf("series page sort select: %v", strings.Contains(out, "grid-sort-select"))
	}
}

// The filtered and search pages keep {n} as the grid expects it, with the
// sort before it.
func TestSortedPageURLs(t *testing.T) {
	sortFixture(t)
	rec := httptest.NewRecorder()
	searchPageHandler(rec, httptest.NewRequest("GET", "/search?q=alpha&sort=title", nil))
	if !strings.Contains(rec.Body.String(), `data-page-url="/api/search?sort=title&q=alpha&page={n}"`) {
		i := strings.Index(rec.Body.String(), "data-page-url")
		t.Errorf("search: %.100s", rec.Body.String()[max(i, 0):])
	}
	c := categoryByKey("series")
	rec = httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/series/", filteredPageHandler(c))
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/series/Alpha%20Cycle?sort=title", nil))
	if !strings.Contains(rec.Body.String(), `?api=1&sort=title&page={n}"`) || !strings.Contains(rec.Body.String(), `data-sort-kind="series"`) {
		i := strings.Index(rec.Body.String(), "data-page-url")
		t.Errorf("series: %.100s", rec.Body.String()[max(i, 0):])
	}
}
