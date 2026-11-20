package metalookup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/opf"
)

const testUserAgent = "metalookup-test/1.0"

// fakeLookupServers stands in for Open Library and its covers. status, if
// set, is what the edition request answers instead (with Retry-After: 30).
type fakeLookupServers struct {
	lookup      *Client
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
		if r.UserAgent() != testUserAgent || r.Header.Get("Cookie") != "" {
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

	f.lookup = New(Config{
		HTTPClient:     &http.Client{Timeout: 5 * time.Second},
		UserAgent:      testUserAgent,
		OpenLibraryURL: olServer.URL,
		CoversURL:      olServer.URL,
		GoogleURL:      gbServer.URL,
		GoogleKey:      "test-key",
		Cache:          NewCache(time.Hour, 100),
		Limits: map[string]*Limiter{
			OpenLibrary: NewLimiter(100, 100, 1000),
			GoogleBooks: NewLimiter(100, 100, 1000),
		},
	})
	return f
}

// only is a result's one match, or nil.
func only(t *testing.T, res Result) *Match {
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
	ol := f.lookup.Lookup(context.Background(), OpenLibrary, Query{ISBN: "9780134190440"})
	want := opf.Metadata{
		Title:       "The Go Programming Language: Second Edition",
		Creators:    []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
		Publisher:   "Addison-Wesley",
		Date:        "2015-10-26",
		Languages:   []string{"deu"}, // MARC "ger" → Calibre's "deu"
		Description: "<p>Line one.</p><p>Line &lt;two&gt;<br>and more.</p>",
		Subjects:    []string{"Go (Computer program language)", "Programming"},
		Identifiers: []opf.Identifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "olid", Value: "OL25925901M"}},
	}
	m := only(t, ol)
	if ol.Source != "Open Library" || ol.Error != "" || m == nil {
		t.Fatalf("Open Library: %+v", ol)
	}
	if !reflect.DeepEqual(*m.Meta, want) {
		t.Errorf("Open Library option:\n got  %+v\n want %+v", *m.Meta, want)
	}
	if m.CoverID != 7427319 {
		t.Errorf("Open Library cover: %d", m.CoverID)
	}
	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent or with cookies", n)
	}
	if n := f.hits["author"].Load(); n != 2 {
		t.Errorf("fetched %d authors, want 2 (bad keys are skipped)", n)
	}

	// Asked again: answered from the cache.
	f.lookup.Lookup(context.Background(), OpenLibrary, Query{ISBN: "9780134190440"})
	if e := f.hits["edition"].Load(); e != 1 {
		t.Errorf("second lookup wasn't cached: %d edition requests", e)
	}

	// No match is an answer too.
	if res := f.lookup.Lookup(context.Background(), OpenLibrary, Query{ISBN: "9780306406157"}); len(res.Matches) != 0 || res.Error != "" {
		t.Errorf("404 should be no match: %+v", res)
	}
}

func TestLookupRateLimits(t *testing.T) {
	f := newFakeLookupServers(t)
	q := Query{ISBN: "9780134190440"}

	// A 429 pauses lookups for its Retry-After, and isn't cached.
	f.status.Store(http.StatusTooManyRequests)
	if res := f.lookup.Lookup(context.Background(), OpenLibrary, q); len(res.Matches) != 0 || !strings.Contains(res.Error, "30s") {
		t.Fatalf("429: %+v", res)
	}
	f.status.Store(0)
	if res := f.lookup.Lookup(context.Background(), OpenLibrary, q); !strings.Contains(res.Error, "slow down") || f.hits["edition"].Load() != 0 {
		t.Errorf("while paused, Open Library was asked anyway: %+v", res)
	}
	f.lookup.limits[OpenLibrary].pausedTil = time.Time{}
	if res := f.lookup.Lookup(context.Background(), OpenLibrary, q); len(res.Matches) != 1 {
		t.Errorf("after the pause: %+v", res)
	}

	// Token bucket and daily cap: refused, not queued.
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	s := NewLimiter(1, 2, 4)
	if s.Take(now) != nil || s.Take(now) != nil {
		t.Fatal("burst refused")
	}
	if err := s.Take(now); err == nil || !strings.Contains(err.Error(), "try again in 1s") {
		t.Errorf("over the burst: %v", err)
	}
	if s.Take(now.Add(time.Second)) != nil || s.Take(now.Add(2*time.Second)) != nil {
		t.Error("refill refused")
	}
	if err := s.Take(now.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "a day") {
		t.Errorf("over the daily cap: %v", err)
	}
	if s.Take(now.Add(24*time.Hour)) != nil {
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
	res := f.lookup.Lookup(context.Background(), OpenLibrary, Query{Title: "the go programming language", Author: "Donovan"})
	if len(res.Matches) != 3 {
		t.Fatalf("Open Library search: %d matches, want 3: %+v", len(res.Matches), res)
	}
	ol := res.Matches[0].Meta
	want := opf.Metadata{
		Title:       "The Go Programming Language",
		Creators:    []string{"Alan A. A. Donovan", "Brian W. Kernighan"},
		Publisher:   "Addison-Wesley",
		Date:        "2015-01-01",
		Languages:   nil, // editions in Chinese and English: not one language
		Description: "<p>Line one.</p><p>Line &lt;two&gt;<br>and more.</p>",
		Subjects:    []string{"Programming"},
		Identifiers: []opf.Identifier{{Scheme: "olid", Value: "OL17348524W"}},
	}
	if !reflect.DeepEqual(*ol, want) {
		t.Errorf("search option:\n got  %+v\n want %+v", *ol, want)
	}
	if c := res.Matches[0].CoverID; c != 7427319 {
		t.Errorf("first match's cover: %d", c)
	}
	// The others in Open Library's order; a work that couldn't be fetched
	// (or has a bad key) still offers the search's values.
	second, third := res.Matches[1], res.Matches[2]
	if second.Meta.Title != "Go Programming Language Workbook" || second.CoverID != 0 ||
		!reflect.DeepEqual(second.Meta.Languages, []string{"eng"}) || second.Meta.Description != "" ||
		!reflect.DeepEqual(second.Meta.Identifiers, []opf.Identifier{{Scheme: "olid", Value: "OL99W"}}) {
		t.Errorf("second match: %+v", second.Meta)
	}
	if third.Meta.Title != "Odd key" || third.Meta.Identifiers != nil {
		t.Errorf("third match: %+v", third.Meta)
	}
	if n := f.hits["work"].Load(); n != 1 { // OL99W is a 404
		t.Errorf("fetched %d works, want 1 (bad keys are skipped)", n)
	}
	if n := f.badRequests.Load(); n != 0 {
		t.Errorf("%d requests without the generic User-Agent", n)
	}
}

func TestMarkISBN(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.Lookup(context.Background(), OpenLibrary, Query{Title: "the go programming language", Author: "Donovan"})
	marked := res.MarkISBN("9780134190440")
	if len(marked.Matches) != 3 || !marked.Matches[0].HasISBN || marked.Matches[1].HasISBN || marked.Matches[2].HasISBN {
		t.Fatalf("marked: %+v", marked.Matches)
	}
	if res.Matches[0].HasISBN {
		t.Error("MarkISBN changed the original result")
	}
}

func TestFetchCover(t *testing.T) {
	f := newFakeLookupServers(t)
	img, ctype, err := f.lookup.FetchCover(context.Background(), "7427319")
	if err != nil || ctype != "image/jpeg" || !strings.Contains(string(img), "fake jpeg") {
		t.Errorf("cover: %q %q %v", img, ctype, err)
	}
	if _, _, err := f.lookup.FetchCover(context.Background(), "1"); !errors.Is(err, ErrNoCover) {
		t.Errorf("non-image answer: %v, want ErrNoCover", err)
	}
	if _, _, err := f.lookup.FetchCover(context.Background(), "../../x"); !errors.Is(err, ErrBadCoverID) {
		t.Errorf("bad id: %v, want ErrBadCoverID", err)
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
	c := NewCache(time.Hour, 2)
	c.put("a", Result{Source: "a"})
	c.put("b", Result{Source: "b"})
	c.put("c", Result{Source: "c"})
	if len(c.entries) != 2 {
		t.Errorf("cache grew past its limit: %d", len(c.entries))
	}
	if r, ok := c.get("c"); !ok || r.Source != "c" {
		t.Error("newest entry missing")
	}
	c.entries["c"] = cacheEntry{res: Result{Source: "c"}, at: time.Now().Add(-2 * time.Hour)}
	if _, ok := c.get("c"); ok {
		t.Error("expired entry returned")
	}
}

func TestGoogleBooksLookup(t *testing.T) {
	f := newFakeLookupServers(t)
	res := f.lookup.Lookup(context.Background(), GoogleBooks, Query{ISBN: "9780134190440"})
	m := only(t, res)
	if m == nil || res.Source != "Google Books" || m.Meta.Description != "<p>The <b>authoritative</b> resource.</p>" || m.CoverID != 0 ||
		!reflect.DeepEqual(m.Meta.Subjects, []string{"Computers", "Programming Languages"}) ||
		!reflect.DeepEqual(m.Meta.Identifiers, []opf.Identifier{{Scheme: "isbn", Value: "9780134190440"}, {Scheme: "google", Value: "SJHvCgAAQBAJ"}}) {
		t.Fatalf("Google Books: %+v", res)
	}
}

// Usage reports without taking: the bucket refills, a new day starts over,
// and a pause shows until it ends.
func TestLimiterUsage(t *testing.T) {
	l := NewLimiter(1, 5, 100)
	now := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	for range 3 {
		if err := l.Take(now); err != nil {
			t.Fatal(err)
		}
	}
	u := l.Usage(now)
	if u.Used != 3 || u.Daily != 100 || u.Tokens != 2 || u.Burst != 5 {
		t.Fatalf("after 3: %+v", u)
	}
	if again := l.Usage(now); again != u {
		t.Fatalf("Usage changed the limiter: %+v then %+v", u, again)
	}
	if u := l.Usage(now.Add(2 * time.Second)); u.Tokens != 4 {
		t.Errorf("2 s later: %v tokens, want 4", u.Tokens)
	}
	if !u.ResetsAt.Equal(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("resets at %v", u.ResetsAt)
	}
	l.Pause(now.Add(time.Minute))
	if u := l.Usage(now); u.PausedUntil.IsZero() {
		t.Error("pause not shown")
	}
	if u := l.Usage(now.Add(2 * time.Hour)); u.Used != 0 || !u.PausedUntil.IsZero() {
		t.Errorf("next day: %+v", u)
	}
}
