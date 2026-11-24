package hardcoverauthors

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/hardcover"
)

const testSchema = `CREATE TABLE hardcover_authors (name TEXT PRIMARY KEY COLLATE NOCASE, hc_id INTEGER NOT NULL DEFAULT 0,
	hc_slug TEXT NOT NULL DEFAULT '', hc_name TEXT NOT NULL DEFAULT '', bio TEXT NOT NULL DEFAULT '',
	image TEXT NOT NULL DEFAULT '', born_year INTEGER NOT NULL DEFAULT 0, death_year INTEGER NOT NULL DEFAULT 0,
	checked_at INTEGER NOT NULL);`

type fakeHost struct{}

func (fakeHost) Members(name string) []string       { return []string{name} }
func (fakeHost) TitleKeys([]string) map[string]bool { return nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// fixture is a Service whose Hardcover answers with fail (an HTTP status,
// 0 for none) or else one "Ann Author", with a picture.
type fixture struct {
	s       *Service
	clock   *clock
	client  *hardcover.Client
	fail    atomic.Int32
	asked   atomic.Int32
	stored  []string
	removed []string
}

func newFixture(t *testing.T, daily int) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatal(err)
	}
	f := &fixture{clock: &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}}
	var pic bytes.Buffer
	_ = png.Encode(&pic, image.NewRGBA(image.Rect(0, 0, 20, 30)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "assets.hardcover.app" {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pic.Bytes())
			return
		}
		f.asked.Add(1)
		if code := f.fail.Load(); code != 0 {
			http.Error(w, "down", int(code))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"authors":[{"id":7,"name":"Ann Author","slug":"ann-author","bio":"Bio.","born_year":1900,
			"death_year":1980,"books_count":1,"users_count":5,"contributions":[],
			"cached_image":{"url":"https://assets.hardcover.app/authors/7/x.jpg"}}]}}`)
	}))
	t.Cleanup(srv.Close)
	base := http.DefaultTransport.(*http.Transport).Clone()
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = r.URL.Host
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return base.RoundTrip(r)
	})}
	f.client = hardcover.New(hardcover.Config{URL: "https://api.hardcover.app/v1/graphql", Token: "t", UserAgent: "test",
		Now: f.clock.now, HTTPClient: hc, Budget: hardcover.NewBudget(600, 100, daily)})
	n := 0
	f.s = New(Config{DB: db, Host: fakeHost{}, Now: f.clock.now,
		Client: func() *hardcover.Client { return f.client },
		StoreImage: func([]byte) (string, error) {
			n++
			name := string(rune('a'+n-1)) + ".jpg"
			f.stored = append(f.stored, name)
			return name, nil
		},
		RemoveImage: func(name string) { f.removed = append(f.removed, name) },
	})
	return f
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLookupFoundAndImageReplaced(t *testing.T) {
	f := newFixture(t, 100)
	res, err := f.s.Lookup(context.Background(), "Ann Author")
	if err != nil || res != (Result{Found: true, HCName: "Ann Author"}) {
		t.Fatalf("lookup: %v %v", res, err)
	}
	a := f.s.For("ann author")
	if !a.Found() || a.HCID != 7 || a.Image != "a.jpg" || a.Years() != "1900–1980" || a.HardcoverURL() != "https://hardcover.app/authors/ann-author" {
		t.Fatalf("stored: %+v", a)
	}
	if a.NeedsLookup(f.clock.now().Add(365 * 24 * time.Hour)) {
		t.Error("a found author is looked up again")
	}
	// Looked up again: the new picture replaces the old, which goes.
	if _, err := f.s.Lookup(context.Background(), "Ann Author"); err != nil {
		t.Fatal(err)
	}
	if a := f.s.For("Ann Author"); a.Image != "b.jpg" || len(f.removed) != 1 || f.removed[0] != "a.jpg" {
		t.Errorf("replaced: %+v, removed %v", a, f.removed)
	}
	if err := f.s.Forget("Ann Author"); err != nil || f.s.For("Ann Author") != nil || len(f.removed) != 2 {
		t.Errorf("forget: %v, removed %v", err, f.removed)
	}
}

func TestLookupFailedIsRetriedLater(t *testing.T) {
	f := newFixture(t, 100)
	f.fail.Store(http.StatusBadRequest)
	if _, err := f.s.Lookup(context.Background(), "Ann Author"); err == nil {
		t.Fatal("no error")
	}
	a := f.s.For("Ann Author")
	if a == nil || a.HCID != -1 {
		t.Fatalf("failure not kept: %+v", a)
	}
	if a.NeedsLookup(f.clock.now().Add(time.Hour)) {
		t.Error("retried at once")
	}
	if !a.NeedsLookup(f.clock.now().Add(RetryAfter + time.Minute)) {
		t.Error("never retried")
	}
}

func TestLookupHeldNearTheLimit(t *testing.T) {
	f := newFixture(t, 10) // Reserve keeps 2 of 10 back
	f.client.Budget.Restore(f.clock.now(), hardcover.Usage{Day: "2026-11-19", Used: 6, Remote: -1})
	_, err := f.s.Lookup(context.Background(), "Ann Author")
	var held *hardcoverseries.HeldError
	if !errors.As(err, &held) || !held.Until.Equal(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("not held: %v", err)
	}
	if f.asked.Load() != 0 || f.s.For("Ann Author") != nil {
		t.Error("asked or saved while held")
	}
}

func TestNilService(t *testing.T) {
	var s *Service
	if s.For("x") != nil || s.Forget("x") != nil || !s.For("x").NeedsLookup(time.Now()) {
		t.Error("nil service")
	}
}
