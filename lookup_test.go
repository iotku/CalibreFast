package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
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
	for _, k := range []string{"edition", "work", "author", "search", "wikidata", "google"} {
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
	ol.HandleFunc("/b/id/1-L.jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	})
	olServer := httptest.NewServer(ol)
	t.Cleanup(olServer.Close)

	wd := http.NewServeMux()
	wd.HandleFunc("/sparql", func(w http.ResponseWriter, r *http.Request) {
		check(r)
		f.hits["wikidata"].Add(1)
		q := r.URL.Query().Get("query")
		switch {
		case strings.Contains(q, `"978-0-13-419044-0"`) && strings.Contains(q, "wdt:P212"): // found by its hyphenated ISBN
			reply(w, `{"results": {"bindings": [{"item": {"value": "http://www.wikidata.org/entity/Q1"}, "work": {"value": "http://www.wikidata.org/entity/Q2"}}]}}`)
		case strings.Contains(q, "wd:Q1 wdt:P1476") && strings.Contains(q, "wd:Q2 wdt:P1476"):
			reply(w, wikidataValuesFixture)
		default:
			reply(w, `{"results": {"bindings": []}}`)
		}
	})
	wdServer := httptest.NewServer(wd)
	t.Cleanup(wdServer.Close)

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
		wikidata:    wdServer.URL + "/sparql",
		googleBooks: gbServer.URL,
		cache:       newLookupCache(time.Hour, 100),
		limits: map[string]*serviceLimit{
			openLibraryName: newServiceLimit(100, 100, 1000),
			wikidataName:    newServiceLimit(100, 100, 1000),
			googleBooksName: newServiceLimit(100, 100, 1000),
		},
	}
	return f
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
	if ol.Source != "Open Library" || ol.Error != "" || ol.Option == nil || ol.Option.Format != "Open Library" {
		t.Fatalf("Open Library: %+v", ol)
	}
	if !reflect.DeepEqual(ol.Option.OPFMetadata, want) {
		t.Errorf("Open Library option:\n got  %+v\n want %+v", ol.Option.OPFMetadata, want)
	}
	if ol.Cover != "/metadata/lookup/cover?src=openlibrary&id=7427319" {
		t.Errorf("Open Library cover: %q", ol.Cover)
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
	if res := f.lookup.lookup(context.Background(), openLibraryName, lookupQuery{ISBN: "9780306406157"}); res.Option != nil || res.Error != "" {
		t.Errorf("404 should be no match: %+v", res)
	}
}

func TestLookupRateLimits(t *testing.T) {
	f := newFakeLookupServers(t)
	q := lookupQuery{ISBN: "9780134190440"}

	// A 429 pauses lookups for its Retry-After, and isn't cached.
	f.status.Store(http.StatusTooManyRequests)
	if res := f.lookup.lookup(context.Background(), openLibraryName, q); res.Option != nil || !strings.Contains(res.Error, "30s") {
		t.Fatalf("429: %+v", res)
	}
	f.status.Store(0)
	if res := f.lookup.lookup(context.Background(), openLibraryName, q); !strings.Contains(res.Error, "slow down") || f.hits["edition"].Load() != 0 {
		t.Errorf("while paused, Open Library was asked anyway: %+v", res)
	}
	f.lookup.limits[openLibraryName].pausedTil = time.Time{}
	if res := f.lookup.lookup(context.Background(), openLibraryName, q); res.Option == nil {
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
	ol := res.Option
	if ol == nil {
		t.Fatalf("Open Library search: %+v", res)
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
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil || res.Source != "Open Library" || res.Option == nil {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	// Only Open Library was asked: the others only when the editor asks.
	if w, g := f.hits["wikidata"].Load(), f.hits["google"].Load(); w != 0 || g != 0 {
		t.Errorf("other sources asked: wikidata %d, google %d", w, g)
	}
	if rec := get("source=wikidata&isbn=9780134190440"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"Wikidata"`) {
		t.Errorf("wikidata: %d %s", rec.Code, rec.Body)
	}
	if rec := get("source=google&isbn=9780134190440"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "authoritative") {
		t.Errorf("google: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"isbn=9780134190441", "", "author=Donovan", "source=wikidata&title=Go", "source=elsewhere&isbn=9780134190440"} {
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

// wikidataValuesFixture answers wikidataValuesQuery for an edition (Q1) of a
// work (Q2): authors are on the work, in P1545 order; labels come in English
// and multilingual.
const wikidataValuesFixture = `{"results": {"bindings": [
	{"from": {"value": "Q1"}, "kind": {"value": "label"}, "value": {"value": "The Go Programming Language", "xml:lang": "mul"}},
	{"from": {"value": "Q1"}, "kind": {"value": "title"}, "value": {"value": "The Go Programming Language", "xml:lang": "en"}},
	{"from": {"value": "Q1"}, "kind": {"value": "date"}, "value": {"value": "2015-10-26T00:00:00Z"}},
	{"from": {"value": "Q2"}, "kind": {"value": "date"}, "value": {"value": "2015-01-01T00:00:00Z"}},
	{"from": {"value": "Q1"}, "kind": {"value": "publisher"}, "ref": {"value": "http://www.wikidata.org/entity/Q3"}, "value": {"value": "Addison-Wesley", "xml:lang": "en"}},
	{"from": {"value": "Q1"}, "kind": {"value": "language"}, "ref": {"value": "http://www.wikidata.org/entity/Q4"}, "value": {"value": "en"}},
	{"from": {"value": "Q1"}, "kind": {"value": "language"}, "ref": {"value": "http://www.wikidata.org/entity/Q4"}, "value": {"value": "eng"}},
	{"from": {"value": "Q2"}, "kind": {"value": "author"}, "ref": {"value": "http://www.wikidata.org/entity/Q6"}, "value": {"value": "Brian Kernighan", "xml:lang": "en"}, "ord": {"value": "2"}},
	{"from": {"value": "Q2"}, "kind": {"value": "author"}, "ref": {"value": "http://www.wikidata.org/entity/Q5"}, "value": {"value": "Alan Donovan", "xml:lang": "mul"}, "ord": {"value": "1"}},
	{"from": {"value": "Q2"}, "kind": {"value": "author"}, "ref": {"value": "http://www.wikidata.org/entity/Q5"}, "value": {"value": "Alan A. A. Donovan", "xml:lang": "en"}, "ord": {"value": "1"}},
	{"from": {"value": "Q2"}, "kind": {"value": "subject"}, "ref": {"value": "http://www.wikidata.org/entity/Q7"}, "value": {"value": "Go", "xml:lang": "en"}},
	{"from": {"value": "Q1"}, "kind": {"value": "subject"}, "ref": {"value": "http://www.wikidata.org/entity/Q8"}, "value": {"value": "textbook", "xml:lang": "en"}}
]}}`

func TestWikidataLookup(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.lookup(context.Background(), wikidataName, lookupQuery{ISBN: "9780134190440"})
	want := OPFMetadata{
		Title:       "The Go Programming Language",
		Creators:    []string{"Alan A. A. Donovan", "Brian Kernighan"},
		Publisher:   "Addison-Wesley",
		Date:        "2015-10-26", // the edition's, not the work's
		Languages:   []string{"eng"},
		Subjects:    []string{"Go", "textbook"},
		Identifiers: []opfIdentifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "wikidata", Value: "Q1"}},
	}
	if res.Option == nil || res.Option.Format != "Wikidata" {
		t.Fatalf("Wikidata: %+v", res)
	}
	if !reflect.DeepEqual(res.Option.OPFMetadata, want) {
		t.Errorf("Wikidata option:\n got  %+v\n want %+v", res.Option.OPFMetadata, want)
	}
	if res := f.lookup.lookup(context.Background(), wikidataName, lookupQuery{ISBN: "9780306406157"}); res.Option != nil || res.Error != "" {
		t.Errorf("unknown ISBN should be no match: %+v", res)
	}
	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent", n)
	}
}

func TestISBNHyphenations(t *testing.T) {
	got := isbnHyphenations("9780134190440")
	for _, want := range []string{"9780134190440", "978-0-13-419044-0", "0134190440", "0-13-419044-0", "978-0134-19-044-0"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(got) > 60 {
		t.Errorf("%d variants: too many for one query", len(got))
	}
	if x := isbnHyphenations("9780804429573"); !slices.Contains(x, "0-8044-2957-X") {
		t.Errorf("ISBN-10 with X missing: %q", x)
	}
	if n := isbnHyphenations("9791090636071"); slices.ContainsFunc(n, func(s string) bool { return len(strings.ReplaceAll(s, "-", "")) == 10 }) {
		t.Error("979 ISBNs have no ISBN-10")
	}
}

func TestGoogleBooksLookup(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.lookup(context.Background(), googleBooksName, lookupQuery{ISBN: "9780134190440"})
	if res.Option == nil || res.Option.Description != "<p>The <b>authoritative</b> resource.</p>" || res.Cover != "" ||
		!reflect.DeepEqual(res.Option.Subjects, []string{"Computers", "Programming Languages"}) ||
		!reflect.DeepEqual(res.Option.Identifiers, []opfIdentifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "google", Value: "SJHvCgAAQBAJ"}}) {
		t.Errorf("Google Books: %+v", res.Option)
	}
}
