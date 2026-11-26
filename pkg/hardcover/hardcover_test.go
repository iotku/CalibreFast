package hardcover

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// selectionDepth is a query's deepest nesting of selection sets, with
// arguments (and their braces) left out: series { book_series { book { id } } } is 3.
func selectionDepth(q string) (depth, topLevel int) {
	_, body, _ := strings.Cut(q, "{")
	body = body[:strings.LastIndex(body, "}")]
	level, parens := 1, 0
	var word strings.Builder
	flush := func() {
		if level == 1 && parens == 0 && word.Len() > 0 {
			topLevel++
		}
		word.Reset()
	}
	for _, r := range body {
		switch {
		case r == '(':
			if parens == 0 {
				flush()
			}
			parens++
		case r == ')':
			parens--
		case parens > 0:
		case r == '{':
			flush()
			level++
			depth = max(depth, level-1)
		case r == '}':
			level--
		case r == ' ' || r == '\n' || r == '\t' || r == ',':
			flush()
		default:
			if level == 1 && word.Len() == 0 && strings.ContainsRune(":", r) {
				continue
			}
			word.WriteRune(r)
		}
	}
	flush()
	return depth, topLevel
}

func TestQueriesDepthAndCost(t *testing.T) {
	for name, q := range allQueries {
		depth, top := selectionDepth(q.text)
		if depth > 3 {
			t.Errorf("%s: depth %d, want at most 3", name, depth)
		}
		if top != q.cost {
			t.Errorf("%s: %d top-level fields, cost says %d", name, top, q.cost)
		}
		if strings.Contains(q.text, "_like") || strings.Contains(q.text, "_regex") || strings.Contains(q.text, "_similar") {
			t.Errorf("%s uses an operator Hardcover disables", name)
		}
	}
	if d, n := selectionDepth("query { a { b { c { d } } } x }"); d != 3 || n != 2 {
		t.Errorf("selectionDepth self-check: %d %d", d, n)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestBudgetBurstAndRefill(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	b := NewBudget(60, 10, 100)
	for i := range 10 {
		if w := b.Reserve(now, 1); !w.IsZero() {
			t.Fatalf("request %d held until %v", i, w)
		}
	}
	w := b.Reserve(now, 1)
	if w.IsZero() || w.Sub(now) != time.Second {
		t.Fatalf("11th request: wait %v, want 1s", w.Sub(now))
	}
	if w := b.Reserve(now.Add(time.Second), 1); !w.IsZero() {
		t.Fatalf("after a second: held until %v", w)
	}
	if u := b.Usage(now.Add(time.Second)); u.Used != 11 {
		t.Errorf("used %d, want 11", u.Used)
	}
}

func TestBudgetDailyResetsAtMidnightUTC(t *testing.T) {
	now := time.Date(2026, 11, 19, 22, 0, 0, 0, time.UTC)
	b := NewBudget(60, 10, 3)
	for range 3 {
		if w := b.Reserve(now, 1); !w.IsZero() {
			t.Fatal("held too soon")
		}
		now = now.Add(time.Minute)
	}
	w := b.Reserve(now, 1)
	if want := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC); !w.Equal(want) {
		t.Fatalf("held until %v, want %v", w, want)
	}
	if w := b.Reserve(w, 1); !w.IsZero() {
		t.Fatalf("after the reset: held until %v", w)
	}
}

func TestBudgetObserve(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	b := NewBudget(60, 10, 4500)
	b.Observe(now, `"Free";r=2;t=0, "daily";r=1;t=43200`)
	if w := b.Reserve(now, 1); !w.IsZero() {
		t.Fatal("first request held")
	}
	// Hardcover says the day has 1 left, now used: held until midnight UTC.
	w := b.Reserve(now, 1)
	if want := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC); !w.Equal(want) {
		t.Fatalf("held until %v, want %v", w, want)
	}
	b2 := NewBudget(60, 10, 4500)
	b2.Observe(now, `"Free";r=0;t=7`)
	if w := b2.Reserve(now, 1); w.Sub(now) != 7*time.Second {
		t.Fatalf("per-minute bucket empty: wait %v, want 7s", w.Sub(now))
	}
	// Garbage is ignored.
	b3 := NewBudget(60, 10, 4500)
	b3.Observe(now, `nonsense;;r=x, "daily";r=-4`)
	if w := b3.Reserve(now, 1); !w.IsZero() {
		t.Fatal("garbage header held a request")
	}
}

func TestBudgetRestore(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	b := NewBudget(60, 10, 10)
	b.Restore(now, Usage{Day: "2026-11-19", Used: 10, Remote: -1})
	if w := b.Reserve(now, 1); w.IsZero() {
		t.Fatal("restored day's use not counted")
	}
	b2 := NewBudget(60, 10, 10)
	b2.Restore(now, Usage{Day: "2026-11-18", Used: 10, Remote: -1, PausedUntil: now.Add(time.Hour)})
	if w := b2.Reserve(now, 1); !w.Equal(now.Add(time.Hour)) {
		t.Fatalf("pause not restored: %v", w)
	}
	if u := b2.Usage(now); u.Used != 0 {
		t.Errorf("yesterday's use counted: %d", u.Used)
	}
}

// fakeHardcover answers the queries this package sends.
type fakeHardcover struct {
	srv      *httptest.Server
	requests atomic.Int32
	bad      atomic.Int32 // requests with a wrong header or a cookie
	status   atomic.Int32 // answer this status when set
	lastVars sync.Map     // operation -> variables
}

const testToken = "s3cret-token"

func newFakeHardcover(t *testing.T) *fakeHardcover {
	f := &fakeHardcover{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testToken ||
			r.Header.Get("User-Agent") != "CalibreFast/test" || r.Header.Get("Cookie") != "" ||
			r.Header.Get("Content-Type") != "application/json" {
			f.bad.Add(1)
		}
		w.Header().Set("RateLimit", `"Free";r=9;t=0, "daily";r=4000;t=1000`)
		if s := f.status.Load(); s != 0 {
			if s == 429 {
				w.Header().Set("Retry-After", "30")
			}
			w.WriteHeader(int(s))
			io.WriteString(w, `{"error":"x"}`)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad", 400)
			return
		}
		op := strings.Fields(req.Query)[1]
		op, _, _ = strings.Cut(op, "(")
		f.lastVars.Store(op, req.Variables)
		w.Header().Set("Content-Type", "application/json")
		switch op {
		case "SearchSeries":
			io.WriteString(w, `{"data":{"search":{"results":{"found":2,"hits":[
				{"document":{"id":"42","name":"The Expanse","slug":"the-expanse","author_name":"James S. A. Corey","books_count":12,"primary_books_count":9}},
				{"document":{"id":7,"name":"Bad <slug>","slug":"../evil","books_count":1}}]}}}}`)
		case "EditionsByISBN":
			io.WriteString(w, `{"data":{"editions":[{"book_id":1},{"book_id":2},{"book_id":1}]}}`)
		case "SeriesOfBooks":
			io.WriteString(w, `{"data":{"book_series":[
				{"book_id":1,"position":1,"series":{"id":42,"name":"The Expanse","slug":"the-expanse","books_count":12}},
				{"book_id":2,"position":2,"series":{"id":42,"name":"The Expanse","slug":"the-expanse","books_count":12}},
				{"book_id":2,"position":1,"series":{"id":99,"name":"Other","slug":"other","books_count":3}}]}}`)
		case "SeriesBooks":
			io.WriteString(w, `{"data":{"series":[{"id":42,"name":"The Expanse","slug":"the-expanse","books_count":12,"description":"A <b>space</b> opera.<br>Nine books.","book_series":[
				{"position":1,"details":"1","book":{"id":1,"slug":"leviathan-wakes","title":"Leviathan Wakes","subtitle":null,"release_date":"2011-06-02","cached_image":{"url":"https://assets.hardcover.app/books/1/cover.jpg"}}},
				{"position":1.5,"details":"1.5","book":{"id":3,"slug":"the-butcher","title":"The Butcher of Anderson Station","release_date":"2011-10-01","cached_image":{"url":"javascript:alert(1)"}}},
				{"position":2,"details":"2","book":{"id":2,"slug":"calibans-war","title":"Caliban's War","release_date":null}}]}]}}`)
		case "BookDetails":
			io.WriteString(w, `{"data":{"books":[
				{"id":1,"contributions":[{"author":{"name":"James S. A. Corey"}}],"editions":[{"isbn_13":"9780316129084"}]},
				{"id":2,"contributions":[{"author":{"name":"James S. A. Corey"}},{"author":null}],"editions":[]}]}}`)
		default:
			io.WriteString(w, `{"errors":[{"message":"unknown op"}]}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestClient(f *fakeHardcover, clock *fakeClock) *Client {
	return New(Config{URL: f.srv.URL, Token: testToken, UserAgent: "CalibreFast/test", Now: clock.now,
		Budget: NewBudget(60, 10, 100)})
}

func TestClientQueries(t *testing.T) {
	f := newFakeHardcover(t)
	clock := &fakeClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	c := newTestClient(f, clock)
	ctx := context.Background()

	got, err := c.SearchSeries(ctx, "  The   Expanse ")
	if err != nil || len(got) != 2 || got[0].ID != 42 || got[0].Slug != "the-expanse" || got[1].Slug != "" {
		t.Fatalf("SearchSeries = %+v, %v", got, err)
	}
	if v, _ := f.lastVars.Load("SearchSeries"); v.(map[string]any)["q"] != "The Expanse" {
		t.Errorf("search variables: %v", v)
	}
	n := f.requests.Load()
	if _, err := c.SearchSeries(ctx, "the expanse"); err != nil || f.requests.Load() != n {
		t.Errorf("search not cached")
	}

	cands, err := c.SeriesByISBNs(ctx, []string{"9780316129084", "9780316129091", "9780316129084"})
	if err != nil || len(cands) != 2 || cands[0].ID != 42 || cands[0].Hits != 2 || cands[1].Hits != 1 {
		t.Fatalf("SeriesByISBNs = %+v, %v", cands, err)
	}

	d, err := c.SeriesBooks(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Entries) != 3 || d.Entries[0].Authors[0] != "James S. A. Corey" || d.Entries[0].ISBNs[0] != "9780316129084" ||
		*d.Entries[1].Position != 1.5 || len(d.Entries[2].Authors) != 1 || d.Entries[2].Released != "" {
		t.Fatalf("SeriesBooks = %+v", d.Entries)
	}
	if d.Entries[0].Image != "https://assets.hardcover.app/books/1/cover.jpg" || d.Entries[1].Image != "" || d.Entries[2].Image != "" {
		t.Errorf("cover images: %q %q %q", d.Entries[0].Image, d.Entries[1].Image, d.Entries[2].Image)
	}
	if d.Description != "A space opera.\nNine books." {
		t.Errorf("Description = %q", d.Description)
	}
	if f.bad.Load() != 0 {
		t.Errorf("%d requests with wrong headers or cookies", f.bad.Load())
	}
	if u := c.Budget.Usage(clock.now()); u.Used != 5 || u.Remote != 4000-0 && u.Remote > 4000 {
		t.Errorf("usage %+v", u)
	}
}

func TestClientErrorsAndHolds(t *testing.T) {
	f := newFakeHardcover(t)
	clock := &fakeClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	c := newTestClient(f, clock)
	ctx := context.Background()

	f.status.Store(401)
	_, err := c.SeriesBooks(ctx, 42)
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("401: %v", err)
	}
	f.status.Store(429)
	_, err = c.SeriesBooks(ctx, 42)
	var we *WaitError
	if !errors.As(err, &we) || we.Until.Sub(clock.now()) != 30*time.Second {
		t.Fatalf("429: %v", err)
	}
	n := f.requests.Load()
	if _, err := c.SeriesBooks(ctx, 42); !errors.As(err, &we) || f.requests.Load() != n {
		t.Errorf("paused budget still sent a request")
	}
	clock.add(31 * time.Second)
	f.status.Store(0)
	if _, err := c.SeriesBooks(ctx, 42); err != nil {
		t.Errorf("after the pause: %v", err)
	}
	for _, s := range []int32{401, 403, 500, 503} {
		f.status.Store(s)
		clock.add(2 * time.Minute)
		if _, err := c.SeriesBooks(ctx, 42); err == nil || strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), f.srv.URL) {
			t.Errorf("HTTP %d: %v", s, err)
		}
	}
	// Unreachable: the URL isn't in the error.
	f.srv.Close()
	clock.add(2 * time.Minute)
	if _, err := c.SeriesBooks(ctx, 42); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("unreachable: %v", err)
	}
	if New(Config{}).auth != "" {
		t.Error("no token, yet an Authorization header")
	}
	if _, err := New(Config{URL: "http://127.0.0.1:1"}).SeriesBooks(ctx, 1); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no token: %v", err)
	}
}

func TestAuthHeaderAndSlugs(t *testing.T) {
	for in, want := range map[string]string{"abc": "Bearer abc", "Bearer abc": "Bearer abc", "bearer  abc ": "Bearer abc", " ": ""} {
		if got := authHeader(in); got != want {
			t.Errorf("authHeader(%q) = %q, want %q", in, got, want)
		}
	}
	for s, ok := range map[string]bool{"the-expanse": true, "a1": true, "": false, "../x": false, "a/b": false, "A": false, "javascript:alert(1)": false} {
		if ValidSlug(s) != ok {
			t.Errorf("ValidSlug(%q) != %v", s, ok)
		}
	}
}

func TestSeriesBooksParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		op, _, _ := strings.Cut(strings.Fields(req.Query)[1], "(")
		switch op {
		case "SeriesByName":
			io.WriteString(w, `{"data":{"series":[
				{"id":1,"name":"Target","slug":"target-a","books_count":2,"author":{"name":"A"}},
				{"id":2,"name":"Target","slug":"target-b","books_count":3,"author":null}]}}`)
		case "SeriesBooks":
			if ids, _ := req.Variables["ids"].([]any); len(ids) != 2 {
				t.Errorf("ids %v", req.Variables["ids"])
			}
			// Part 1 is smaller but lists position 2 with more readers.
			io.WriteString(w, `{"data":{"series":[
				{"id":1,"name":"Target","slug":"target-a","books_count":2,"author":{"name":"A"},"book_series":[
					{"position":2,"book":{"id":20,"title":"The Daleks","users_count":50}},
					{"position":5,"book":{"id":50,"title":"Five","users_count":1}}]},
				{"id":2,"name":"Target","slug":"target-b","books_count":3,"author":{"name":"B"},"book_series":[
					{"position":1,"book":{"id":10,"title":"One","users_count":3}},
					{"position":2,"book":{"id":21,"slug":"dr-who-the-daleks","title":"Dr Who - The Daleks","users_count":2}},
					{"position":null,"book":{"id":99,"title":"Unnumbered","users_count":0}}]}]}}`)
		case "BookDetails":
			io.WriteString(w, `{"data":{"books":[
				{"id":21,"contributions":[{"author":{"name":"Someone"}}],"editions":[{"isbn_13":"9780000000021"}]},
				{"id":20,"contributions":[{"author":{"name":"David Whitaker"}}],"editions":[{"isbn_13":"9780000000020"}]}]}}`)
		}
	}))
	defer srv.Close()
	clock := &fakeClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	c := New(Config{URL: srv.URL, Token: "t", Now: clock.now, Budget: NewBudget(60, 10, 100)})
	ctx := context.Background()

	same, err := c.SeriesByName(ctx, []string{"Target", "Target"})
	if err != nil || len(same) != 2 || same[0].Author != "A" || same[1].Author != "" {
		t.Fatalf("SeriesByName = %+v, %v", same, err)
	}
	d, err := c.SeriesBooks(ctx, 2, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != 2 || len(d.Parts) != 2 || len(d.AllParts()) != 2 {
		t.Errorf("series %+v parts %+v", d.Series, d.Parts)
	}
	var titles []string
	for _, e := range d.Entries {
		titles = append(titles, e.Title)
	}
	if strings.Join(titles, "|") != "One|The Daleks|Five|Unnumbered" {
		t.Fatalf("entries %q", titles)
	}
	daleks := d.Entries[1]
	if daleks.BookID != 20 || len(daleks.AltTitles) != 1 || daleks.AltTitles[0] != "Dr Who - The Daleks" ||
		len(daleks.ISBNs) != 2 || len(daleks.Authors) != 1 || daleks.Authors[0] != "David Whitaker" {
		t.Errorf("merged position 2: %+v", daleks)
	}
	if ids := daleks.IDs(); len(ids) != 2 || ids[0] != 20 {
		t.Errorf("merged position 2 ids %v", ids)
	}
	if slugs := daleks.Slugs(); len(slugs) != 1 || slugs[0] != "dr-who-the-daleks" {
		t.Errorf("merged position 2 slugs %v", slugs)
	}
}

func TestBudgetReadyIn(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	b := NewBudget(60, 10, 20)
	if w, ok := b.ReadyIn(now, 2); w != 0 || !ok {
		t.Errorf("full: %v %v", w, ok)
	}
	if w := b.Reserve(now, 10); !w.IsZero() {
		t.Fatal(w)
	}
	if w, ok := b.ReadyIn(now, 2); w != 2*time.Second || !ok {
		t.Errorf("empty, 2 wanted: %v %v", w, ok)
	}
	if _, ok := b.ReadyIn(now, 11); ok {
		t.Error("over the day's requests should be not today")
	}
	b.Pause(now.Add(time.Minute))
	if w, _ := b.ReadyIn(now, 1); w != time.Minute {
		t.Errorf("paused: %v", w)
	}
	if u := b.Usage(now); u.Used != 10 {
		t.Errorf("ReadyIn reserved: used %d", u.Used)
	}
}
