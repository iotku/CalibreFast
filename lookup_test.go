package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLookupServers stands in for Open Library, its covers and Google Books.
// googleStatus, if set, is what the Google Books API answers instead.
type fakeLookupServers struct {
	lookup       *bookLookup
	hits         map[string]*atomic.Int32
	googleStatus atomic.Int32
	badRequests  atomic.Int32 // requests without the generic User-Agent, or with cookies
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
		reply(w, `{"docs": [{"key": "/works/OL17348524W", "title": "The Go Programming Language",
			"author_name": ["Alan A. A. Donovan", "Brian W. Kernighan"], "first_publish_year": 2015,
			"publisher": ["Addison-Wesley", "Pearson"], "language": ["chi", "eng"], "subject": ["Programming"], "cover_i": 7427319}]}`)
	})
	ol.HandleFunc("/b/id/7427319-L.jpg", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		if r.URL.Query().Get("default") != "false" {
			t.Error("cover requested without default=false")
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("\xff\xd8\xff fake jpeg"))
	})
	ol.HandleFunc("/b/isbn/{file}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	})
	olServer := httptest.NewServer(ol)
	t.Cleanup(olServer.Close)

	gb := http.NewServeMux()
	gb.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["google"].Add(1)
		if code := f.googleStatus.Load(); code != 0 {
			http.Error(w, `{"error": {"code": 429}}`, int(code))
			return
		}
		q := r.URL.Query().Get("q")
		if q != "isbn:9780134190440" && q != `intitle:"the go programming language" inauthor:"Donovan"` {
			reply(w, `{"totalItems": 0}`)
			return
		}
		reply(w, `{"items": [{"id": "SJHvCgAAQBAJ", "volumeInfo": {
			"title": "The Go Programming Language", "authors": ["Alan A. A. Donovan", "Brian W. Kernighan"],
			"publisher": "Addison-Wesley Professional", "publishedDate": "2015-10-26",
			"description": "<p>The <b>authoritative</b> resource.</p>",
			"industryIdentifiers": [{"type": "ISBN_10", "identifier": "0134190440"}, {"type": "ISBN_13", "identifier": "9780134190440"}],
			"categories": ["Computers / Programming Languages / General", "Computers / Programming / General"],
			"language": "en",
			"imageLinks": {"thumbnail": "http://books.google.com/books/content?id=SJHvCgAAQBAJ&zoom=1"}}}]}`)
	})
	gb.HandleFunc("/books/content", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG fake"))
	})
	gbServer := httptest.NewServer(gb)
	t.Cleanup(gbServer.Close)

	f.lookup = &bookLookup{
		client:       &http.Client{Timeout: 5 * time.Second},
		openLibrary:  olServer.URL,
		olCovers:     olServer.URL,
		googleBooks:  gbServer.URL,
		googleCovers: gbServer.URL,
		cache:        newLookupCache(time.Hour, 100),
	}
	return f
}

func TestLookupByISBN(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.lookup(context.Background(), lookupQuery{ISBN: "9780134190440"})
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %+v", res)
	}

	ol := res[0]
	wantOL := OPFMetadata{
		Title:       "The Go Programming Language: Second Edition",
		Creators:    []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
		Publisher:   "Addison-Wesley",
		Date:        "2015-10-26",
		Languages:   []string{"deu"}, // MARC "ger" → Calibre's "deu"
		Description: "<p>Line one.</p><p>Line &lt;two&gt;<br>and more.</p>",
		Subjects:    []string{"Go (Computer program language)", "Programming"},
		Identifiers: []opfIdentifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "olid", Value: "OL25925901M"}},
	}
	if ol.Source != "Open Library" || ol.Error != "" || ol.Option == nil || ol.Option.Format != "Open Library" {
		t.Fatalf("Open Library: %+v", ol)
	}
	if !reflect.DeepEqual(ol.Option.OPFMetadata, wantOL) {
		t.Errorf("Open Library option:\n got  %+v\n want %+v", ol.Option.OPFMetadata, wantOL)
	}
	if ol.Cover != "/metadata/lookup/cover?src=openlibrary&id=7427319" {
		t.Errorf("Open Library cover: %q", ol.Cover)
	}

	gb := res[1]
	wantGB := OPFMetadata{
		Title:       "The Go Programming Language",
		Creators:    []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
		Publisher:   "Addison-Wesley Professional",
		Date:        "2015-10-26",
		Languages:   []string{"eng"},
		Description: "<p>The <b>authoritative</b> resource.</p>",
		Subjects:    []string{"Computers", "Programming Languages", "Programming"},
		Identifiers: []opfIdentifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "google", Value: "SJHvCgAAQBAJ"}},
	}
	if gb.Source != "Google Books" || gb.Option == nil || gb.Option.Format != "Google Books" {
		t.Fatalf("Google Books: %+v", gb)
	}
	if !reflect.DeepEqual(gb.Option.OPFMetadata, wantGB) {
		t.Errorf("Google Books option:\n got  %+v\n want %+v", gb.Option.OPFMetadata, wantGB)
	}
	if gb.Cover != "/metadata/lookup/cover?src=google&id=SJHvCgAAQBAJ" {
		t.Errorf("Google Books cover: %q", gb.Cover)
	}

	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent or with cookies", n)
	}
	if n := f.hits["author"].Load(); n != 2 {
		t.Errorf("fetched %d authors, want 2 (bad keys are skipped)", n)
	}

	// Asked again: answered from the cache.
	f.lookup.lookup(context.Background(), lookupQuery{ISBN: "9780134190440"})
	if e, g := f.hits["edition"].Load(), f.hits["google"].Load(); e != 1 || g != 1 {
		t.Errorf("second lookup wasn't cached: edition %d, google %d requests", e, g)
	}
}

func TestLookupNoMatchAndErrors(t *testing.T) {
	f := newFakeLookupServers(t)
	f.googleStatus.Store(http.StatusTooManyRequests)
	res := f.lookup.lookup(context.Background(), lookupQuery{ISBN: "9780306406157"})
	if res[0].Option != nil || res[0].Error != "" {
		t.Errorf("Open Library 404 should be no match: %+v", res[0])
	}
	if res[1].Option != nil || !strings.Contains(res[1].Error, "CF_GOOGLE_BOOKS_KEY") {
		t.Errorf("Google 429 should explain the quota: %+v", res[1])
	}

	// Errors aren't cached; "no match" is.
	f.googleStatus.Store(0)
	f.lookup.lookup(context.Background(), lookupQuery{ISBN: "9780306406157"})
	if g := f.hits["google"].Load(); g != 2 {
		t.Errorf("Google asked %d times, want 2 (the error isn't cached)", g)
	}
}

func TestLookupByTitle(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.lookup(context.Background(), lookupQuery{Title: "the go programming language", Author: "Donovan"})
	ol := res[0].Option
	if ol == nil {
		t.Fatalf("Open Library search: %+v", res[0])
	}
	want := OPFMetadata{
		Title:       "The Go Programming Language",
		Creators:    []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
		Publisher:   "Addison-Wesley",
		Date:        "2015-01-01",
		Languages:   nil, // editions in Chinese and English: not one language
		Description: "<p>Line one.</p><p>Line &lt;two&gt;<br>and more.</p>",
		Subjects:    []string{"Programming"},
		Identifiers: []opfIdentifier{{Scheme: "olid", Value: "OL17348524W"}},
	}
	if !reflect.DeepEqual(ol.OPFMetadata, want) {
		t.Errorf("search option:\n got  %+v\n want %+v", ol.OPFMetadata, want)
	}
	if gb := res[1].Option; gb == nil || gb.Identifiers[0].Value != "9780134190440" {
		t.Errorf("Google title search should take the volume's ISBN: %+v", res[1])
	}
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
	if rec := get("src=google&id=SJHvCgAAQBAJ"); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("Google cover: %d", rec.Code)
	}
	if rec := get("src=openlibrary&isbn=9780134190440"); rec.Code != http.StatusNotFound {
		t.Errorf("non-image answer should be 404, got %d", rec.Code)
	}
	for _, q := range []string{"src=openlibrary&id=../../x", "src=google&id=a/b", "src=elsewhere&id=1", "src=openlibrary&isbn=123"} {
		if rec := get(q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", q, rec.Code)
		}
	}
}

func TestLookupHandler(t *testing.T) {
	f := newFakeLookupServers(t)
	orig, origNo := bookLookups, noLookups
	bookLookups = f.lookup
	defer func() { bookLookups, noLookups = orig, origNo }()

	h := requireLookups(lookupHandler)
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/metadata/lookup?"+query, nil))
		return rec
	}
	rec := get("isbn=0-13-419044-0") // ISBN-10, looked up as its ISBN-13
	var res []lookupResult
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || len(res) != 2 || res[0].Option == nil {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"isbn=9780134190441", "", "author=Donovan"} {
		if rec := get(q); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400", q, rec.Code)
		}
	}

	noLookups = true
	if rec := get("isbn=9780134190440"); rec.Code != http.StatusForbidden {
		t.Errorf("-no-lookups: got %d, want 403", rec.Code)
	}
}

func TestLookupHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"2016": "2016-01-01", "October 26, 2015": "2015-10-26", "Oct 2015": "2015-10-01",
		"October 2015": "2015-10-01", "c1995": "1995-01-01", "2015-10-26": "2015-10-26", "": "", "n.d.": "",
	} {
		if got := lookupDate(in); got != want {
			t.Errorf("lookupDate(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"en": "eng", "eng": "eng", "ger": "deu", "fre": "fra", "pt-BR": "por", "zh_TW": "zho", "fil": "fil", "xx": "xx",
	} {
		if got := canonicalLanguage(in); got != want {
			t.Errorf("canonicalLanguage(%q) = %q, want %q", in, got, want)
		}
	}

	c := newLookupCache(time.Hour, 2)
	c.put("a", lookupResult{Source: "a"})
	c.put("b", lookupResult{Source: "b"})
	c.put("c", lookupResult{Source: "c"})
	if len(c.entries) != 2 {
		t.Errorf("cache grew past its limit: %d", len(c.entries))
	}
	if r, ok := c.get("c"); !ok || r.Source != "c" {
		t.Error("newest entry missing")
	}
	c.entries["c"] = lookupCacheEntry{res: lookupResult{Source: "c"}, at: time.Now().Add(-2 * time.Hour)}
	if _, ok := c.get("c"); ok {
		t.Error("expired entry returned")
	}
}
