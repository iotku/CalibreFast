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

// fakeLookupServers stands in for Open Library and its covers. status, if
// set, is what the edition request answers instead (with Retry-After: 30).
type fakeLookupServers struct {
	lookup      *bookLookup
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

	f.lookup = &bookLookup{
		client:      &http.Client{Timeout: 5 * time.Second},
		openLibrary: olServer.URL,
		olCovers:    olServer.URL,
		googleBooks: gbServer.URL,
		cache:       newLookupCache(time.Hour, 100),
		limits: map[string]*serviceLimit{
			openLibraryName: newServiceLimit(100, 100, 1000),
			googleBooksName: newServiceLimit(100, 100, 1000),
		},
	}
	return f
}

// only is a result's one match, or nil.
func only(t *testing.T, res lookupResult) *lookupMatch {
	t.Helper()
	switch len(res.Matches) {
	case 0:
		return nil
	case 1:
		return &res.Matches[0]
	}
	t.Fatalf("%d matches, want one: %+v", len(res.Matches), res)
	return nil
}

func TestLookupByISBN(t *testing.T) {
	f := newFakeLookupServers(t)
	ol := f.lookup.lookup(context.Background(), openLibraryName, lookupQuery{ISBN: "9780134190440"})
	want := OPFMetadata{
		Title:       "The Go Programming Language: Second Edition",
		Creators:    []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
		Publisher:   "Addison-Wesley",
		Date:        "2015-10-26",
		Languages:   []string{"deu"}, // MARC "ger" → Calibre's "deu"
		Description: "<p>Line one.</p><p>Line &lt;two&gt;<br>and more.</p>",
		Subjects:    []string{"Go (Computer program language)", "Programming"},
		Identifiers: []opfIdentifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "olid", Value: "OL25925901M"}},
	}
	m := only(t, ol)
	if ol.Source != "Open Library" || ol.Error != "" || m == nil || m.Option.Format != "Open Library" {
		t.Fatalf("Open Library: %+v", ol)
	}
	if !reflect.DeepEqual(m.Option.OPFMetadata, want) {
		t.Errorf("Open Library option:\n got  %+v\n want %+v", m.Option.OPFMetadata, want)
	}
	if m.Cover != "/metadata/lookup/cover?src=openlibrary&id=7427319" {
		t.Errorf("Open Library cover: %q", m.Cover)
	}
	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent or with cookies", n)
	}
	if !strings.HasPrefix(lookupUserAgent, "CalibreFast/") || !strings.Contains(lookupUserAgent, "github.com/iotku/CalibreFast") {
		t.Errorf("User-Agent %q", lookupUserAgent)
	}
	if n := f.hits["author"].Load(); n != 2 {
		t.Errorf("fetched %d authors, want 2 (bad keys are skipped)", n)
	}

	// Asked again: answered from the cache.
	f.lookup.lookup(context.Background(), openLibraryName, lookupQuery{ISBN: "9780134190440"})
	if e := f.hits["edition"].Load(); e != 1 {
		t.Errorf("second lookup wasn't cached: %d edition requests", e)
	}

	// No match is an answer too.
	if res := f.lookup.lookup(context.Background(), openLibraryName, lookupQuery{ISBN: "9780306406157"}); len(res.Matches) != 0 || res.Error != "" {
		t.Errorf("404 should be no match: %+v", res)
	}
}

func TestLookupRateLimits(t *testing.T) {
	f := newFakeLookupServers(t)
	q := lookupQuery{ISBN: "9780134190440"}

	// A 429 pauses lookups for its Retry-After, and isn't cached.
	f.status.Store(http.StatusTooManyRequests)
	if res := f.lookup.lookup(context.Background(), openLibraryName, q); len(res.Matches) != 0 || !strings.Contains(res.Error, "30s") {
		t.Fatalf("429: %+v", res)
	}
	f.status.Store(0)
	if res := f.lookup.lookup(context.Background(), openLibraryName, q); !strings.Contains(res.Error, "slow down") || f.hits["edition"].Load() != 0 {
		t.Errorf("while paused, Open Library was asked anyway: %+v", res)
	}
	f.lookup.limits[openLibraryName].pausedTil = time.Time{}
	if res := f.lookup.lookup(context.Background(), openLibraryName, q); len(res.Matches) != 1 {
		t.Errorf("after the pause: %+v", res)
	}

	// Token bucket and daily cap: refused, not queued.
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	s := newServiceLimit(1, 2, 4)
	if s.take(now) != nil || s.take(now) != nil {
		t.Fatal("burst refused")
	}
	if err := s.take(now); err == nil || !strings.Contains(err.Error(), "try again in 1s") {
		t.Errorf("over the burst: %v", err)
	}
	if s.take(now.Add(time.Second)) != nil || s.take(now.Add(2*time.Second)) != nil {
		t.Error("refill refused")
	}
	if err := s.take(now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "a day") {
		t.Errorf("over the daily cap: %v", err)
	}
	if s.take(now.Add(24*time.Hour)) != nil {
		t.Error("the daily cap didn't reset the next day")
	}

	for in, want := range map[string]time.Duration{"120": 2 * time.Minute, "": time.Minute, "0": time.Second, "999999": 24 * time.Hour} {
		if got := retryAfter(in, now); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", in, got, want)
		}
	}
	if got := retryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got != 90*time.Second {
		t.Errorf("retryAfter(date) = %s", got)
	}
}

func TestLookupByTitle(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.lookup(context.Background(), openLibraryName, lookupQuery{Title: "the go programming language", Author: "Donovan"})
	if len(res.Matches) != 3 {
		t.Fatalf("Open Library search: %d matches, want 3: %+v", len(res.Matches), res)
	}
	ol := res.Matches[0].Option
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
	if !reflect.DeepEqual(ol.OPFMetadata, want) || ol.Format != "Open Library" {
		t.Errorf("search option:\n got  %+v\n want %+v", ol.OPFMetadata, want)
	}
	if c := res.Matches[0].Cover; c != "/metadata/lookup/cover?src=openlibrary&id=7427319" {
		t.Errorf("first match's cover: %q", c)
	}
	// The others in Open Library's order; a work that couldn't be fetched
	// (or has a bad key) still offers the search's values.
	second, third := res.Matches[1], res.Matches[2]
	if second.Option.Title != "Go Programming Language Workbook" || second.Cover != "" ||
		!reflect.DeepEqual(second.Option.Languages, []string{"eng"}) || second.Option.Description != "" ||
		!reflect.DeepEqual(second.Option.Identifiers, []opfIdentifier{{Scheme: "olid", Value: "OL99W"}}) {
		t.Errorf("second match: %+v", second.Option)
	}
	if third.Option.Title != "Odd key" || third.Option.Identifiers != nil {
		t.Errorf("third match: %+v", third.Option)
	}
	if n := f.hits["work"].Load(); n != 1 { // OL99W is a 404
		t.Errorf("fetched %d works, want 1 (bad keys are skipped)", n)
	}
	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent", n)
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
	noGoogleBooks = true
	if rec := get("source=google&isbn=9780134190440"); rec.Code != http.StatusForbidden {
		t.Errorf("-no-google-books: got %d, want 403", rec.Code)
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

func TestGoogleBooksLookup(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.lookup(context.Background(), googleBooksName, lookupQuery{ISBN: "9780134190440"})
	m := only(t, res)
	if m == nil || m.Option.Format != "Google Books" || m.Option.Description != "<p>The <b>authoritative</b> resource.</p>" || m.Cover != "" ||
		!reflect.DeepEqual(m.Option.Subjects, []string{"Computers", "Programming Languages"}) ||
		!reflect.DeepEqual(m.Option.Identifiers, []opfIdentifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "google", Value: "SJHvCgAAQBAJ"}}) {
		t.Fatalf("Google Books: %+v", res)
	}
}
