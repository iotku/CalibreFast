package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

func TestAuthorNameVariants(t *testing.T) {
	for name, want := range map[string][]string{
		"J. R. R. Tolkien":   {"J. R. R. Tolkien", "J.R.R. Tolkien"},
		"J.R.R. Tolkien":     {"J.R.R. Tolkien", "J. R. R. Tolkien"},
		"Le Guin, Ursula K.": {"Le Guin, Ursula K.", "Ursula K. Le Guin"},
		"Terry Pratchett":    {"Terry Pratchett"},
	} {
		got := hcAuthorNameVariants(name)
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("%q: %q lacks %q", name, got, w)
			}
		}
	}
}

func TestPickAuthor(t *testing.T) {
	a := hardcover.Author{ID: 1, Name: "John Smith", UsersCount: 900, Titles: []string{"Famous Book"}}
	b := hardcover.Author{ID: 2, Name: "John Smith", UsersCount: 10, Titles: []string{"The Quiet Garden: A Novel"}}
	c := hardcover.Author{ID: 3, Name: "John Smith", UsersCount: 400, Titles: []string{"Other"}}
	// The one whose books the library has, however little read.
	if p := hcPickAuthor([]hardcover.Author{a, b}, map[string]bool{textkey.Title("Quiet Garden"): true}); p == nil || p.ID != 2 {
		t.Errorf("by books: %+v", p)
	}
	// None of theirs here: only one far more read than the next.
	if p := hcPickAuthor([]hardcover.Author{a, b}, nil); p == nil || p.ID != 1 {
		t.Errorf("far more read: %+v", p)
	}
	if p := hcPickAuthor([]hardcover.Author{a, c}, nil); p != nil {
		t.Errorf("ambiguous taken: %+v", p)
	}
	if p := hcPickAuthor([]hardcover.Author{c}, nil); p == nil || p.ID != 3 {
		t.Errorf("only one: %+v", p)
	}
	if hcPickAuthor(nil, nil) != nil {
		t.Error("none")
	}
}

func TestSplitDescription(t *testing.T) {
	if l, m := splitDescription("short"); l != "short" || m != "" {
		t.Errorf("short: %q %q", l, m)
	}
	p1, p2, p3 := strings.Repeat("a", 350), strings.Repeat("b", 200), strings.Repeat("c", 200)
	if l, m := splitDescription(p1 + "\n\n" + p2 + "\n\n" + p3); l != p1 || m != p2+"\n\n"+p3 {
		t.Errorf("paragraphs: %d %d", len(l), len(m))
	}
	long := strings.Repeat("Word word word. ", 100)
	l, m := splitDescription(long)
	if len(l) > descLeadMax || !strings.HasSuffix(l, ".") || strings.TrimSpace(l+" "+m) != strings.TrimSpace(long) {
		t.Errorf("one long paragraph: %d %q…", len(l), m[:20])
	}
	for desc, want := range map[string]bool{
		"short":                  false,
		strings.Repeat("a", 250): true,
		"one\ntwo\nthree\nfour":  true,
	} {
		if got := (FilteredPageData{Description: desc}).DescClamps(); got != want {
			t.Errorf("DescClamps(%.20q) = %v", desc, got)
		}
	}
}

// fakeHardcoverAuthors answers author queries: two "John Smith"s (only
// one wrote the library's book), J.R.R. Tolkien under that spelling only
// (and by search), and nobody else. Pictures come from
// assets.hardcover.app, served here.
func fakeHardcoverAuthors(t *testing.T, requests *atomic.Int32) *http.Client {
	var pic bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 60))
	img.Set(1, 1, color.RGBA{200, 0, 0, 255})
	_ = png.Encode(&pic, img)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "assets.hardcover.app" {
			if r.Header.Get("Authorization") != "" {
				t.Error("token sent with a picture")
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write(pic.Bytes())
			return
		}
		requests.Add(1)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, `{"error":"invalid_token"}`, http.StatusUnauthorized)
			return
		}
		op, _, _ := strings.Cut(strings.Fields(req.Query)[1], "(")
		w.Header().Set("Content-Type", "application/json")
		smith := `{"id":11,"name":"John Smith","slug":"john-smith","bio":"The popular one.","books_count":30,"users_count":500,
			"cached_image":null,"contributions":[{"book":{"title":"Famous"}}]},
			{"id":12,"name":"John Smith","slug":"john-smith-2","bio":"Wrote <b>gardens</b> & <i>hedges</i>.<sup>[1]</sup>\r\n\r\nSecond paragraph.","born_year":1950,"death_year":null,"books_count":2,"users_count":200,
			"cached_image":{"url":"https://assets.hardcover.app/authors/12/x.jpg"},"contributions":[{"book":{"title":"The Quiet Garden"}},{"book":null}]}`
		tolkien := `{"id":21,"name":"J.R.R. Tolkien","slug":"jrr-tolkien","bio":"Philologist.","born_year":1892,"death_year":1973,
			"cached_image":{"url":"https://evil.example/x.jpg"},"books_count":300,"users_count":9000,"contributions":[]}`
		switch op {
		case "AuthorsByName":
			names, _ := req.Variables["names"].([]any)
			var out []string
			for _, n := range names {
				switch n {
				case "John Smith":
					out = append(out, smith)
				}
			}
			io.WriteString(w, `{"data":{"authors":[`+strings.Join(out, ",")+`]}}`)
		case "SearchAuthors":
			io.WriteString(w, `{"data":{"search":{"results":{"hits":[
				{"document":{"id":"99","name":"J. R. R. Tolkien & Someone","alternate_names":[]}},
				{"document":{"id":"21","name":"J.R.R. Tolkien","alternate_names":["Tolkien, John Ronald Reuel"]}}]}}}}`)
		case "AuthorsByID":
			ids, _ := req.Variables["ids"].([]any)
			if len(ids) != 1 || ids[0] != float64(21) {
				t.Errorf("AuthorsByID %v", ids)
			}
			io.WriteString(w, `{"data":{"authors":[`+tolkien+`]}}`)
		default:
			io.WriteString(w, `{"errors":[{"message":"unknown op `+op+`"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	// Every request, whatever its host, goes to srv.
	base := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Host = r.URL.Host
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(srv.URL, "http://")
		return base.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHardcoverAuthorProfiles(t *testing.T) {
	db, _, _ := seriesFixture(t)
	addTestBook(t, db, "The Quiet Garden", []string{"John Smith"}, nil, "", "epub")
	addTestBook(t, db, "The Hobbit", []string{"J. R. R. Tolkien"}, nil, "", "epub")
	addTestBook(t, db, "Unknown Work", []string{"Nobody Atall"}, nil, "", "epub")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	httpClient := fakeHardcoverAuthors(t, &requests)
	clock := &hcTestClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	origClient, origNow, origAwait, origWrites, origData := hcClient, hcNow, hcAwaitTime, allowWrites, dataDir
	hcNow, hcAwaitTime, allowWrites, dataDir = clock.now, 0, true, t.TempDir()
	newClient := func(daily int) {
		hcClient = hardcover.New(hardcover.Config{URL: "https://api.hardcover.app/v1/graphql", Token: "test-token", UserAgent: "test",
			Now: clock.now, HTTPClient: httpClient, Budget: hardcover.NewBudget(600, 100, daily)})
	}
	newClient(100)
	t.Cleanup(func() {
		hcClient, hcNow, hcAwaitTime, allowWrites, dataDir = origClient, origNow, origAwait, origWrites, origData
	})

	admin, _ := createUser("boss", "a long enough password", true)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/authors/profile", requireUser(requireLookups(requireHardcover(authorProfileHandler(false)))))
	mux.HandleFunc("POST /api/authors/profile/refresh", requireEditor(requireLookups(requireHardcover(authorProfileHandler(true)))))
	mux.HandleFunc("GET /author-images/{file}", authorImageHandler)
	mux.HandleFunc("POST /api/hardcover/refresh", requireEditor(requireLookups(requireHardcover(hardcoverJobHandler(hardcoverseries.JobRefresh)))))
	do := func(u *User, method, path string, want int) string {
		t.Helper()
		rec := call(t, mux, u, method, path, "")
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.String()
	}
	step := func() bool {
		t.Helper()
		ran, _ := hcLinks.Step(context.Background())
		return ran
	}
	jobsOf := func(state string) int {
		var n int
		_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_jobs WHERE kind = 'author' AND state = ?`, state).Scan(&n)
		return n
	}

	// Signed-in users only, POST only, authors with books here only.
	do(nil, "POST", "/api/authors/profile?name=John+Smith", http.StatusUnauthorized)
	do(alice, "GET", "/api/authors/profile?name=John+Smith", http.StatusMethodNotAllowed)
	do(alice, "POST", "/api/authors/profile?name=Not+Here", http.StatusNotFound)
	if requests.Load() != 0 {
		t.Fatal("asked Hardcover before the queue ran")
	}

	// Queued: answered 202 while it waits; asking again doesn't queue twice.
	do(alice, "POST", "/api/authors/profile?name=John+Smith", http.StatusAccepted)
	do(alice, "POST", "/api/authors/profile?name=john+smith", http.StatusAccepted)
	if jobsOf("queued") != 1 {
		t.Fatalf("queued %d", jobsOf("queued"))
	}
	if !step() {
		t.Fatal("didn't run")
	}
	a := hcAuthorFor("John Smith")
	if a == nil || a.HCID != 12 || a.Slug != "john-smith-2" || a.Born != 1950 || a.Died != 0 || a.Image == "" ||
		a.Bio != "Wrote gardens & hedges.\n\nSecond paragraph." {
		t.Fatalf("John Smith: %+v", a)
	}
	if requests.Load() != 1 {
		t.Errorf("requests %d, want 1", requests.Load())
	}
	if _, err := os.Stat(filepath.Join(dataDir, hcAuthorImagesDir, a.Image)); err != nil {
		t.Errorf("picture not kept: %v", err)
	}
	if rec := call(t, mux, nil, "GET", "/author-images/"+a.Image, ""); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Errorf("picture served: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	// Now it's kept: answered at once, escaped, with nothing asked of Hardcover.
	page := do(alice, "POST", "/api/authors/profile?name=John+Smith", http.StatusOK)
	for _, want := range []string{"Wrote gardens &amp; hedges.", "born 1950", `src="/author-images/` + a.Image + `"`,
		`href="https://hardcover.app/authors/john-smith-2"`, "From"} {
		if !strings.Contains(page, want) {
			t.Errorf("header lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "data-profile-refresh") {
		t.Error("refresh offered to a reader")
	}
	if requests.Load() != 1 || hcAuthorFor("John Smith").CheckedAt != clock.now().Unix() {
		t.Error("asked again")
	}
	// An admin's own description wins; Hardcover's picture still shows.
	desc := "Our own words."
	if err := setAggregateMeta("author", "John Smith", metaEdit{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	page = do(admin, "POST", "/api/authors/profile?name=John+Smith", http.StatusOK)
	if !strings.Contains(page, "Our own words.") || strings.Contains(page, "gardens") || !strings.Contains(page, "/author-images/") ||
		!strings.Contains(page, "data-profile-refresh") {
		t.Errorf("own description:\n%s", page)
	}
	// The age goes under the picture, worked out from the years.
	if !strings.Contains(page, `<span class="agg-header-age">Age 76</span>`) {
		t.Errorf("no age:\n%s", page)
	}
	// An editor's years win over Hardcover's; set back, Hardcover's show again.
	if err := setAggregateMeta("author", "John Smith", metaEdit{Years: &metaYears{Set: true, Born: 1950, Died: 2001}}); err != nil {
		t.Fatal(err)
	}
	page = do(admin, "POST", "/api/authors/profile?name=John+Smith", http.StatusOK)
	if !strings.Contains(page, "Died at 51") || !strings.Contains(page, "1950–2001") || !strings.Contains(page, "Our own words.") {
		t.Errorf("own years:\n%s", page)
	}
	if err := setAggregateMeta("author", "John Smith", metaEdit{Years: &metaYears{}}); err != nil {
		t.Fatal(err)
	}
	if page = do(admin, "POST", "/api/authors/profile?name=John+Smith", http.StatusOK); !strings.Contains(page, "born 1950") {
		t.Errorf("Hardcover's years again:\n%s", page)
	}
	if err := setAggregateMeta("author", "John Smith", metaEdit{Years: &metaYears{Set: true, Born: 2000, Died: 1990}}); err == nil {
		t.Error("died before born accepted")
	}

	// Author lookups are spread out; editors' requests don't wait for them.
	do(alice, "POST", "/api/authors/profile?name=J.+R.+R.+Tolkien", http.StatusAccepted)
	do(alice, "POST", "/api/authors/profile?name=Nobody+Atall", http.StatusAccepted)
	if _, err := hcLinks.Enqueue(hardcoverseries.JobRefresh, "Other", "", hardcoverseries.Payload{}, hcUserID(admin)); err != nil {
		t.Fatal(err)
	}
	clock.set(clock.now().Add(5 * time.Second))
	step() // the series' refresh (it isn't linked, so it fails: it ran)
	var series string
	_ = appDB.QueryRow(`SELECT state FROM hardcover_jobs WHERE kind = 'refresh'`).Scan(&series)
	if series != "failed" {
		t.Errorf("series request waited behind authors: %s", series)
	}
	if ran, next := hcLinks.Step(context.Background()); ran || !next.Equal(time.Date(2026, 11, 19, 12, 0, 20, 0, time.UTC)) {
		t.Fatalf("author lookup not spaced: ran %v, next %v", ran, next)
	}
	clock.set(clock.now().Add(15 * time.Second))
	if !step() {
		t.Fatal("Tolkien didn't run")
	}
	if ran, _ := hcLinks.Step(context.Background()); ran {
		t.Fatal("two author lookups together")
	}
	// Tolkien: found by search under "J.R.R. Tolkien"; a picture not on
	// hardcover.app isn't fetched.
	if a := hcAuthorFor("J. R. R. Tolkien"); a == nil || a.HCID != 21 || a.Image != "" || a.years() != "1892–1973" {
		t.Fatalf("Tolkien: %+v", a)
	}

	// Near the day's limit, author lookups are held till it resets; series
	// requests still run.
	newClient(10) // 20% kept back: 2 of 10
	hcClient.Budget.Restore(clock.now(), hardcover.Usage{Day: "2026-11-19", Used: 6, Remote: -1})
	clock.set(clock.now().Add(time.Minute))
	step()
	if jobsOf("held") != 1 {
		t.Fatalf("not held near the limit: held %d", jobsOf("held"))
	}
	hcLinks.Enqueue(hardcoverseries.JobRefresh, "Wheel", "", hardcoverseries.Payload{}, hcUserID(admin))
	step()
	_ = appDB.QueryRow(`SELECT state FROM hardcover_jobs WHERE kind = 'refresh' AND series = 'Wheel'`).Scan(&series)
	if series != "failed" {
		t.Errorf("series request held with the authors: %s", series)
	}
	clock.set(time.Date(2026, 11, 20, 0, 0, 1, 0, time.UTC))
	step()
	if a := hcAuthorFor("Nobody Atall"); a == nil || a.HCID != 0 {
		t.Fatalf("after the reset: %+v (held %d)", a, jobsOf("held"))
	}
	// Not found is kept too.
	n := requests.Load()
	if page := do(alice, "POST", "/api/authors/profile?name=Nobody+Atall", http.StatusOK); strings.Contains(page, "Hardcover") {
		t.Errorf("not found shows: %s", page)
	}
	if requests.Load() != n || jobsOf("queued") != 0 {
		t.Error("not-found asked again")
	}

	// Editors can have it looked up again.
	do(alice, "POST", "/api/authors/profile/refresh?name=John+Smith", http.StatusNotFound)
	do(admin, "POST", "/api/authors/profile/refresh?name=John+Smith", http.StatusAccepted)
	if hcAuthorFor("John Smith") != nil || jobsOf("queued") != 1 {
		t.Error("refresh didn't forget and queue")
	}
	if _, err := os.Stat(filepath.Join(dataDir, hcAuthorImagesDir, a.Image)); !os.IsNotExist(err) {
		t.Errorf("unused picture kept: %v", err)
	}

	// Author lookups aren't listed among the series requests.
	for _, j := range hcOpenJobs("") {
		if j.Kind == hcJobAuthor {
			t.Errorf("author job listed: %+v", j)
		}
	}
}
