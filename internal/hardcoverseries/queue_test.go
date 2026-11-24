package hardcoverseries

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// testSchema is the Service's tables, as the app's migrations leave them.
const testSchema = `
CREATE TABLE hardcover_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, series TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '{}', state TEXT NOT NULL DEFAULT 'queued', batch TEXT NOT NULL DEFAULT '',
	created_by INTEGER, created_at INTEGER NOT NULL, not_before INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0, result TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '');
CREATE TABLE hardcover_usage (day TEXT PRIMARY KEY, used INTEGER NOT NULL, remote INTEGER NOT NULL DEFAULT -1,
	paused_until INTEGER NOT NULL DEFAULT 0);
CREATE TABLE hardcover_series (name TEXT PRIMARY KEY COLLATE NOCASE, hc_id INTEGER NOT NULL, hc_slug TEXT NOT NULL DEFAULT '',
	hc_name TEXT NOT NULL DEFAULT '', identified_by INTEGER, identified_at INTEGER NOT NULL,
	checked_at INTEGER NOT NULL DEFAULT 0, snapshot TEXT NOT NULL DEFAULT '');
CREATE TABLE hardcover_series_parts (name TEXT NOT NULL COLLATE NOCASE, hc_id INTEGER NOT NULL, hc_slug TEXT NOT NULL DEFAULT '',
	hc_name TEXT NOT NULL DEFAULT '', author TEXT NOT NULL DEFAULT '', books INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (name, hc_id));
CREATE TABLE hardcover_dismissed (name TEXT NOT NULL COLLATE NOCASE, hc_book_id INTEGER NOT NULL, PRIMARY KEY (name, hc_book_id));
CREATE TABLE hardcover_not_match (name TEXT NOT NULL COLLATE NOCASE, hc_book_id INTEGER NOT NULL, uuid TEXT NOT NULL,
	PRIMARY KEY (name, hc_book_id, uuid));
`

// fakeHost is a library with one series, Wheel.
type fakeHost struct{ changed int }

func (*fakeHost) Series(name string) (SeriesInfo, bool) {
	if !strings.EqualFold(name, "Wheel") {
		return SeriesInfo{}, false
	}
	return SeriesInfo{Name: "Wheel", Members: []string{"Wheel"}, Write: "Wheel"}, true
}
func (*fakeHost) Library(si SeriesInfo, _ *hardcover.SeriesDetail) (Library, error) {
	return Library{Members: si.Members}, nil
}
func (*fakeHost) SeriesISBNs(SeriesInfo) ([]string, error) { return nil, nil }
func (*fakeHost) SeriesTitles(SeriesInfo) []string         { return nil }
func (*fakeHost) SeriesAuthors(SeriesInfo) []string        { return nil }
func (*fakeHost) Placeholders() []Placeholder              { return nil }
func (h *fakeHost) StatusChanged()                         { h.changed++ }
func (*fakeHost) ReviewURL(series string) string           { return "/review/" + series }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestService(t *testing.T, bg *Background) (*Service, *fakeHost, *clock) {
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
	h, c := &fakeHost{}, &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	// Never asked: no job here sends a request.
	client := hardcover.New(hardcover.Config{URL: "http://127.0.0.1:1/graphql", Token: "t", Budget: hardcover.NewBudget(60, 10, 100), Now: c.now})
	s := New(Config{DB: db, Host: h, Client: func() *hardcover.Client { return client }, Now: c.now, Background: bg})
	return s, h, c
}

func TestQueueOrderAndBackground(t *testing.T) {
	var ran []string
	hold := time.Time{}
	bg := &Background{Kind: "author", Spacing: 20 * time.Second, Run: func(_ context.Context, name string) (any, error) {
		if !hold.IsZero() {
			return nil, &HeldError{Until: hold, Reason: "held for the test"}
		}
		ran = append(ran, name)
		return map[string]bool{"found": true}, nil
	}}
	s, _, c := newTestService(t, bg)
	ctx := context.Background()

	a1, _ := s.Enqueue("author", "A1", "", Payload{}, 0)
	a2, _ := s.Enqueue("author", "A2", "", Payload{}, 0)
	if again, _ := s.Enqueue("author", "A1", "", Payload{}, 0); again != a1 {
		t.Errorf("queued twice: %d, %d", a1, again)
	}
	// An editor's request, queued last, runs first. (Wheel isn't linked:
	// a refresh fails without asking Hardcover.)
	ed, _ := s.Enqueue(JobRefresh, "Wheel", "", Payload{}, 7)
	if ran, _ := s.Step(ctx); !ran {
		t.Fatal("nothing ran")
	}
	if j, _ := s.JobByID(ed); j.State != "failed" || !strings.Contains(j.Error, "isn't linked") {
		t.Fatalf("editor's job: %+v", j)
	}
	if len(ran) != 0 {
		t.Fatalf("background ran first: %v", ran)
	}
	// Then the background jobs, one per Spacing.
	s.Step(ctx)
	if ran, next := s.Step(ctx); ran || !next.Equal(c.t.Add(20*time.Second)) {
		t.Errorf("spacing: ran %v, next %v", ran, next)
	}
	c.t = c.t.Add(20 * time.Second)
	s.Step(ctx)
	if strings.Join(ran, ",") != "A1,A2" {
		t.Errorf("ran %v", ran)
	}
	if j, _ := s.JobByID(a2); j.State != "done" || string(j.Result) != `{"found":true}` {
		t.Errorf("result: %+v", j)
	}

	// A HeldError holds every background job; an editor's still runs.
	hold = c.t.Add(time.Hour)
	c.t = c.t.Add(time.Minute)
	b1, _ := s.Enqueue("author", "B1", "", Payload{}, 0)
	b2, _ := s.Enqueue("author", "B2", "", Payload{}, 0)
	s.Step(ctx)
	for _, id := range []int64{b1, b2} {
		if j, _ := s.JobByID(id); j.State != "held" || j.NotBefore != hold.Unix() || j.Error != "held for the test" {
			t.Errorf("not held: %+v", j)
		}
	}
	ed2, _ := s.Enqueue(JobRefresh, "Wheel", "", Payload{}, 7)
	if ran, _ := s.Step(ctx); !ran {
		t.Error("an editor's job waited behind held background ones")
	}
	if j, _ := s.JobByID(ed2); j.State != "failed" {
		t.Errorf("editor's job: %+v", j)
	}

	// Cancelling a waiting job, once.
	if ok, err := s.Cancel(b1); !ok || err != nil {
		t.Errorf("cancel: %v %v", ok, err)
	}
	if ok, _ := s.Cancel(b1); ok {
		t.Error("cancelled twice")
	}
	if open, _ := s.OpenJobs(""); len(open) != 0 {
		t.Errorf("background jobs listed as editors': %+v", open)
	}
}

func TestLinksAndDecisions(t *testing.T) {
	s, h, c := newTestService(t, nil)
	pos := func(f float64) *float64 { return &f }
	d := &hardcover.SeriesDetail{Series: hardcover.Series{ID: 9, Name: "The Wheel of Time", Slug: "wheel-of-time"},
		Entries: []hardcover.Entry{{BookID: 1, Title: "The Eye of the World", Position: pos(1)}, {BookID: 2, Title: "The Great Hunt", Position: pos(2)}}}
	if err := s.SaveLink("Wheel", d, 7); err != nil {
		t.Fatal(err)
	}
	l, err := s.LinkFor("wheel")
	if err != nil || l == nil || l.HCName != "The Wheel of Time" || l.URL() != "https://hardcover.app/series/wheel-of-time" || len(l.Parts) != 1 || l.Stale(c.t) {
		t.Fatalf("link: %+v %v", l, err)
	}
	if l.Stale(c.t.Add(31*24*time.Hour)) != true {
		t.Error("never stale")
	}
	si, _ := h.Series("Wheel")
	rev, err := s.ReviewFor(si, l)
	if err != nil || len(rev.Missing) != 2 || rev.Numbered != 2 || rev.Complete() || rev.Pending() != 2 {
		t.Fatalf("review: %+v %v", rev, err)
	}
	if n, err := s.Dismiss("Wheel", []int64{2, 2}); n != 1 || err != nil {
		t.Errorf("dismiss: %d %v", n, err)
	}
	if err := s.NotMatch("Wheel", 1, "uuid-1"); err != nil {
		t.Fatal(err)
	}
	if lib, _ := s.library(si, d); !lib.Refused[1]["uuid-1"] {
		t.Errorf("refused: %v", lib.Refused)
	}
	if rev, _ = s.ReviewFor(si, l); rev.Numbered != 1 || len(rev.Dismissed) != 1 {
		t.Errorf("after dismissing: %+v", rev)
	}
	if err := s.Restore("Wheel", nil); err != nil {
		t.Fatal(err)
	}
	if rev, _ = s.ReviewFor(si, l); rev.Numbered != 2 {
		t.Errorf("after restoring: %+v", rev)
	}
	if revs := s.LinkedReviews(); revs["wheel"].Numbered != 2 {
		t.Errorf("linked reviews: %+v", revs)
	}
	if err := s.Unlink("Wheel"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.LinkFor("Wheel"); l != nil {
		t.Error("still linked")
	}
	if h.changed < 4 {
		t.Errorf("status changes told: %d", h.changed)
	}
	var nilService *Service
	if l, err := nilService.LinkFor("Wheel"); l != nil || err != nil {
		t.Error("a nil Service has links")
	}
}
