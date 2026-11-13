package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDiscoverPage(t *testing.T) {
	db := catalogFixture(t)
	t.Chdir(t.TempDir())
	for i := range 3 {
		addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"Author " + strconv.Itoa(i)}, []string{"T"}, "")
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	preloadCaches()
	t.Cleanup(refreshAllCaches)
	origReg, origCat, origRead := allowRegistrations, publicCatalog, publicRead
	t.Cleanup(func() { allowRegistrations, publicCatalog, publicRead = origReg, origCat, origRead })

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux := http.NewServeMux()
		mux.HandleFunc("/", indexHandler)
		mux.HandleFunc("GET /latest", latestHandler)
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	allowRegistrations, publicCatalog, publicRead = false, true, false
	body := get("/").Body.String()
	for _, want := range []string{
		`id="greeting"`, `id="home-edit"`, `id="home-rows"`,
		`data-total-books="3"`, `data-shuffle-total="3"`, `data-version="` + currentPagesVersion() + `"`,
		`id="row-shuffle"`, `id="row-latest"`, `href="/latest"`, `href="/shuffle"`, `href="/me/reading"`, `href="/me/saved"`, `id="shuffle-next"`, `id="shuffle-back"`,
		"Reading books here needs one too.", // -public without -public-read
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Discover page lacks %q", want)
		}
	}
	if strings.Contains(body, "Create an account") {
		t.Error("offers registration when it's off")
	}
	allowRegistrations, publicRead = true, true
	body = get("/").Body.String()
	if !strings.Contains(body, "Create an account") || strings.Contains(body, "needs one too") {
		t.Error("registration open, -public-read: wrong account line")
	}

	// The grid moved to /latest; its old URLs follow it.
	if rec := get("/?page=3"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/latest?page=3" {
		t.Errorf("/?page=3: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	body = get("/latest").Body.String()
	if !strings.Contains(body, `data-page-url="/pages/page{n}.json?v=`) || !strings.Contains(body, `data-total-books="3"`) ||
		!strings.Contains(body, `href="/latest" aria-current="page"`) {
		t.Errorf("/latest: %s", body)
	}
	if rec := get("/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

func TestCountLabel(t *testing.T) {
	for n, want := range map[int]string{0: "0 books", 1: "1 book", 999: "999 books", 1000: "1,000 books", 48211: "48,211 books", 1234567: "1,234,567 books"} {
		if got := countLabel(n, "book", "books"); got != want {
			t.Errorf("countLabel(%d) = %q, want %q", n, got, want)
		}
	}
}
