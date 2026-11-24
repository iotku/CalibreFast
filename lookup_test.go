package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/metalookup"
)

// fakeLookupServers stands in for Open Library and its covers. status, if
// set, is what the edition request answers instead (with Retry-After: 30).
type fakeLookupServers struct {
	lookup      *metalookup.Client
	hits        map[string]*atomic.Int32
	status      atomic.Int32
	badRequests atomic.Int32 // requests without the generic User-Agent, or with cookies
}

func newFakeLookupServers(t *testing.T) *fakeLookupServers {
	f := &fakeLookupServers{hits: map[string]*atomic.Int32{}}
	for _, k := range []string{"edition", "work", "author", "search", "google"} {
		f.hits[k] = &atomic.Int32{}
	}
	check := func(r *http.Request) {
		if r.UserAgent() != lookupUserAgent || r.Header.Get("Cookie") != "" {
			f.badRequests.Add(1)
		}
	}
	reply := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}

	ol := http.NewServeMux()
	ol.HandleFunc("/isbn/9780134190440.json", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		if code := f.status.Load(); code != 0 {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "slow down", int(code))
			return
		}
		http.Redirect(w, r, "/books/OL25925901M.json", http.StatusFound) // as Open Library does
	})
	ol.HandleFunc("/books/OL25925901M.json", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["edition"].Add(1)
		reply(w, `{"key": "/books/OL25925901M", "title": "The Go Programming Language",
			"subtitle": "Second Edition", "publishers": ["Addison-Wesley"], "publish_date": "October 26, 2015",
			"languages": [{"key": "/languages/ger"}], "works": [{"key": "/works/OL17348524W"}],
			"covers": [-1, 7427319], "isbn_13": ["9780134190440"]}`)
	})
	ol.HandleFunc("/works/OL17348524W.json", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["work"].Add(1)
		reply(w, `{"key": "/works/OL17348524W",
			"description": {"type": "/type/text", "value": "Line one.\r\n\r\nLine <two>\nand more."},
			"subjects": ["Go (Computer program language)", "Accessible book", "nyt:hardcover-advice", "Programming", "programming"],
			"authors": [{"author": {"key": "/authors/OL1A"}}, {"author": {"key": "/authors/OL2A"}}, {"author": {"key": "../../etc/passwd"}}]}`)
	})
	ol.HandleFunc("/authors/{key}", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["author"].Add(1)
		names := map[string]string{"OL1A.json": "Alan A. A. Donovan", "OL2A.json": "Brian W. Kernighan"}
		reply(w, `{"name": "`+names[r.PathValue("key")]+`"}`)
	})
	ol.HandleFunc("/search.json", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["search"].Add(1)
		if r.URL.Query().Get("title") != "the go programming language" || r.URL.Query().Get("author") != "Donovan" {
			reply(w, `{"docs": []}`)
			return
		}
		if r.URL.Query().Get("limit") != "5" {
			t.Errorf("search limit %q, want 5", r.URL.Query().Get("limit"))
		}
		reply(w, `{"docs": [{"key": "/works/OL17348524W", "title": "The Go Programming Language",
			"author_name": ["Alan A. A. Donovan", "Brian W. Kernighan"], "first_publish_year": 2015,
			"publisher": ["Addison-Wesley", "Pearson"], "language": ["chi", "eng"], "subject": ["Programming"], "cover_i": 7427319,
			"isbn": ["0134190440", "9787111558422", "not an isbn"]},
			{"key": "/works/OL99W", "title": "Go Programming Language Workbook", "author_name": ["Someone Else"],
			"language": ["eng"]},
			{"key": "../../etc/passwd", "title": "Odd key"}]}`)
	})
	ol.HandleFunc("/b/id/7427319-L.jpg", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		if r.URL.Query().Get("default") != "false" {
			t.Error("cover requested without default=false")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("\xff\xd8\xff fake jpeg"))
	})
	ol.HandleFunc("/b/id/1-L.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	})
	olServer := httptest.NewServer(ol)
	t.Cleanup(olServer.Close)

	gb := http.NewServeMux()
	gb.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["google"].Add(1)
		if r.URL.Query().Get("q") != "isbn:9780134190440" {
			reply(w, `{"totalItems": 0}`)
			return
		}
		reply(w, `{"items": [{"id": "SJHvCgAAQBAJ", "volumeInfo": {
			"title": "The Go Programming Language", "authors": ["Alan A. A. Donovan", "Brian W. Kernighan"],
			"publisher": "Addison-Wesley Professional", "publishedDate": "2015-10-26",
			"description": "<p>The <b>authoritative</b> resource.</p>",
			"industryIdentifiers": [{"type": "ISBN_13", "identifier": "9780134190440"}],
			"categories": ["Computers / Programming Languages / General"], "language": "en"}}]}`)
	})
	gbServer := httptest.NewServer(gb)
	t.Cleanup(gbServer.Close)

	f.lookup = metalookup.New(metalookup.Config{
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		UserAgent:      lookupUserAgent,
		OpenLibraryURL: olServer.URL,
		CoversURL:      olServer.URL,
		GoogleURL:      gbServer.URL,
		GoogleKey:      "test-key",
		Cache:          metalookup.NewCache(time.Hour, 100),
		Limits: map[string]*metalookup.Limiter{
			openLibraryName: metalookup.NewLimiter(100, 100, 1000),
			googleBooksName: metalookup.NewLimiter(100, 100, 1000),
		},
	})
	return f
}

func TestLookupCoverHandler(t *testing.T) {
	f := newFakeLookupServers(t)
	orig := bookLookups
	bookLookups = f.lookup
	defer func() { bookLookups = orig }()

	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		lookupCoverHandler(rec, httptest.NewRequest("GET", "/metadata/lookup/cover?"+query, nil))
		return rec
	}
	if rec := get("src=openlibrary&id=7427319"); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(rec.Body.String(), "fake jpeg") {
		t.Errorf("Open Library cover: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := get("src=openlibrary&id=1"); rec.Code != http.StatusNotFound {
		t.Errorf("non-image answer should be 404, got %d", rec.Code)
	}
	for _, q := range []string{"src=openlibrary&id=../../x", "src=google&id=abc", "src=elsewhere&id=1", "src=openlibrary&isbn=123"} {
		if rec := get(q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

func TestLookupHandler(t *testing.T) {
	f := newFakeLookupServers(t)
	orig, origNo, origKey := bookLookups, noLookups, googleBooksKey
	bookLookups, googleBooksKey = f.lookup, "test-key"
	defer func() { bookLookups, noLookups, googleBooksKey = orig, origNo, origKey }()

	h := requireLookups(lookupHandler)
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/metadata/lookup?"+query, nil))
		return rec
	}
	rec := get("isbn=0-13-419044-0") // ISBN-10, looked up as its ISBN-13
	var res lookupResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || res.Source != "Open Library" || len(res.Matches) != 1 {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	// Only Open Library was asked: the others only when the editor asks.
	if g := f.hits["google"].Load(); g != 0 {
		t.Errorf("Google Books asked: %d", g)
	}
	// A title search marks the matches with an edition of match_isbn
	// (an ISBN-10 here: compared as ISBN-13s), without sending it anywhere.
	rec = get("title=the+go+programming+language&author=Donovan&match_isbn=0-13-419044-0")
	res = lookupResult{}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || len(res.Matches) != 3 {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	if !res.Matches[0].HasISBN || res.Matches[1].HasISBN || res.Matches[2].HasISBN {
		t.Errorf("hasIsbn: %v %v %v", res.Matches[0].HasISBN, res.Matches[1].HasISBN, res.Matches[2].HasISBN)
	}
	if strings.Contains(rec.Body.String(), "9787111558422") {
		t.Error("the editions' ISBNs went to the browser")
	}
	// From the cache, for another ISBN: the cached answer wasn't marked.
	rec = get("title=the+go+programming+language&author=Donovan&match_isbn=9780306406157")
	if strings.Contains(rec.Body.String(), "hasIsbn") || f.hits["search"].Load() != 1 {
		t.Errorf("second search: %d searches, %s", f.hits["search"].Load(), rec.Body)
	}
	if rec := get("source=google&isbn=9780134190440"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "authoritative") {
		t.Errorf("google: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"isbn=9780134190441", "", "author=Donovan", "source=wikidata&isbn=9780134190440", "source=elsewhere&isbn=9780134190440"} {
		if rec := get(q); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400", q, rec.Code)
		}
	}

	origNoGoogle := noGoogleBooks
	defer func() { noGoogleBooks = origNoGoogle }()
	googleBooksKey = "" // no API key: not offered
	if rec := get("source=google&isbn=9780134190440"); rec.Code != http.StatusForbidden || googleBooksEnabled() {
		t.Errorf("no CF_GOOGLE_BOOKS_KEY: got %d, want 403", rec.Code)
	}
	googleBooksKey = "test-key"
	noGoogleBooks = true
	if rec := get("source=google&isbn=9780134190440"); rec.Code != http.StatusForbidden {
		t.Errorf("-no-google-books: got %d, want 403", rec.Code)
	}
	noLookups = true
	if rec := get("isbn=9780134190440"); rec.Code != http.StatusForbidden {
		t.Errorf("-no-lookups: got %d, want 403", rec.Code)
	}
}

// The editor's JSON: each match an option named after its source, with its
// cover through /metadata/lookup/cover, and the generic User-Agent.
func TestLookupResultJSON(t *testing.T) {
	f := newFakeLookupServers(t)
	res := newLookupResult(f.lookup.Lookup(context.Background(), openLibraryName, metalookup.Query{ISBN: "9780134190440"}))
	if len(res.Matches) != 1 || res.Source != "Open Library" {
		t.Fatalf("result: %+v", res)
	}
	m := res.Matches[0]
	if m.Option.Format != "Open Library" || m.Option.Title != "The Go Programming Language: Second Edition" {
		t.Errorf("option: %+v", m.Option)
	}
	if m.Cover != "/metadata/lookup/cover?src=openlibrary&id=7427319" {
		t.Errorf("cover: %q", m.Cover)
	}
	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent or with cookies", n)
	}
	if !strings.HasPrefix(lookupUserAgent, "CalibreFast/") || !strings.Contains(lookupUserAgent, "github.com/iotku/CalibreFast") {
		t.Errorf("User-Agent %q", lookupUserAgent)
	}
}

func TestHardcoverLookupHandler(t *testing.T) {
	origClient, origNo, origLookups := hcClient, noHardcover, bookLookups
	t.Cleanup(func() { hcClient, noHardcover, bookLookups = origClient, origNo, origLookups })
	h := requireLookups(lookupHandler)
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/metadata/lookup?"+query, nil))
		return rec
	}

	hcClient = nil // no CF_HARDCOVER_TOKEN
	if rec := get("source=hardcover&isbn=9780552152938"); rec.Code != http.StatusForbidden {
		t.Fatalf("without a token: %d", rec.Code)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "EditionByISBN"):
			_, _ = io.WriteString(w, `{"data":{"editions":[{"id":9,"book_id":42,"title":"Mort","isbn_13":"9780552152938","release_date":"1987-11-12",
				"publisher":{"name":"Corgi"},"language":{"code2":"en","code3":"eng"},"cached_image":{"url":"https://assets.hardcover.app/e/9.jpg"}}]}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"books":[{"id":42,"slug":"mort","title":"Mort","description":"Death takes <b>an</b> apprentice.",
				"cached_tags":{"Genre":[{"tag":"Fantasy"}]},"contributions":[{"author":{"name":"Terry Pratchett"}}],
				"book_series":[{"position":4,"series":{"name":"Discworld"}}]}]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	hcClient = hardcover.New(hardcover.Config{URL: srv.URL, Token: "test-token", UserAgent: "test"})
	bookLookups = newBookLookups("", hcClient)

	noHardcover = true
	if rec := get("source=hardcover&isbn=9780552152938"); rec.Code != http.StatusForbidden {
		t.Fatalf("-no-hardcover: %d", rec.Code)
	}
	noHardcover = false

	rec := get("source=hardcover&isbn=9780552152938")
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	var res lookupResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Matches) != 1 {
		t.Fatalf("result %s, err %v", rec.Body, err)
	}
	m := res.Matches[0]
	if res.Source != "Hardcover" || m.Option.Format != "Hardcover" || m.Option.Title != "Mort" { // the rest: pkg/metalookup
		t.Errorf("result %s", rec.Body)
	}
	if m.Cover != "/metadata/lookup/cover?src=hardcover&id=42" {
		t.Errorf("cover %q", m.Cover)
	}

	// The cover route takes only a book id.
	for _, q := range []string{"src=hardcover&id=x", "src=hardcover&id=-1", "src=hardcover&id=https://evil.example/"} {
		rec := httptest.NewRecorder()
		lookupCoverHandler(rec, httptest.NewRequest("GET", "/metadata/lookup/cover?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}
