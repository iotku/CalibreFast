package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// fakeHardcoverAPI answers the GraphQL operations pkg/hardcover sends, for
// one series: "Other" (Hardcover id 77), books 1–4 and a novella (3.5).
func fakeHardcoverAPI(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		switch op {
		case "SearchSeries":
			if req.Variables["q"] == "Other" {
				io.WriteString(w, `{"data":{"search":{"results":{"hits":[
					{"document":{"id":"77","name":"Other","slug":"other","author_name":"Writer","books_count":5}},
					{"document":{"id":"78","name":"Other Things","slug":"other-things","author_name":"Someone","books_count":2}}]}}}}`)
				return
			}
			io.WriteString(w, `{"data":{"search":{"results":{"hits":[
				{"document":{"id":"90","name":"Wheel of Time","slug":"wot","author_name":"Robert Jordan","books_count":14}},
				{"document":{"id":"91","name":"Wheels","slug":"wheels","author_name":"Someone","books_count":2}}]}}}}`)
		case "SeriesByName":
			io.WriteString(w, `{"data":{"series":[{"id":77,"name":"Other","slug":"other","books_count":5,"author":{"name":"Writer"}}]}}`)
		case "EditionsByISBN":
			io.WriteString(w, `{"data":{"editions":[]}}`)
		case "SeriesBooks":
			io.WriteString(w, `{"data":{"series":[{"id":77,"name":"Other","slug":"other","books_count":5,"book_series":[
				{"position":1,"details":"1","book":{"id":1,"slug":"other-one","title":"Other One"}},
				{"position":2,"details":"2","book":{"id":2,"slug":"other-two","title":"Other Two"}},
				{"position":3,"details":"3","book":{"id":3,"slug":"other-three","title":"Other Three","release_date":"2010-03-04"}},
				{"position":3.5,"details":"3.5","book":{"id":35,"slug":"other-novella","title":"Other Novella"}},
				{"position":4,"details":"4","book":{"id":4,"slug":"other-four","title":"Other Four: A Novel"}}]}]}}`)
		case "BookDetails":
			io.WriteString(w, `{"data":{"books":[
				{"id":1,"contributions":[{"author":{"name":"Writer"}}],"editions":[]},
				{"id":2,"contributions":[{"author":{"name":"Writer"}}],"editions":[]},
				{"id":3,"contributions":[{"author":{"name":"Writer"}}],"editions":[{"isbn_13":"9780000000002"}]},
				{"id":35,"contributions":[{"author":{"name":"Writer"}}],"editions":[]},
				{"id":4,"contributions":[{"author":{"name":"Writer"}}],"editions":[]}]}}`)
		default:
			io.WriteString(w, `{"data":{"book_series":[]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type hcTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hcTestClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *hcTestClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func hardcoverMux() *http.ServeMux {
	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return requireEditor(requireLookups(requireHardcover(h))) }
	mux.HandleFunc("GET /missing/hardcover", wrap(hardcoverReviewPage))
	mux.HandleFunc("GET /missing/hardcover/author", wrap(hardcoverAuthorPage))
	mux.HandleFunc("POST /api/hardcover/candidates", wrap(hardcoverJobHandler(hcJobCandidates)))
	mux.HandleFunc("POST /api/hardcover/link", wrap(hardcoverJobHandler(hcJobLink)))
	mux.HandleFunc("POST /api/hardcover/refresh", wrap(hardcoverJobHandler(hcJobRefresh)))
	mux.HandleFunc("POST /api/hardcover/batch", wrap(hardcoverBatchHandler))
	mux.HandleFunc("GET /api/hardcover/jobs", wrap(hardcoverJobsHandler))
	mux.HandleFunc("DELETE /api/hardcover/jobs/{id}", wrap(hardcoverCancelJobHandler))
	mux.HandleFunc("POST /api/hardcover/apply", wrap(hardcoverApplyHandler))
	mux.HandleFunc("GET /missing", requireEditor(missingPageHandler))
	return mux
}

func TestHardcoverSeries(t *testing.T) {
	db, _, other := seriesFixture(t)
	four := addTestBook(t, db, "Other Four", []string{"Writer"}, nil, "", "epub")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	srv := fakeHardcoverAPI(t)
	clock := &hcTestClock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	origClient, origNow, origAwait, origWrites, origData := hcClient, hcNow, hcAwaitTime, allowWrites, dataDir
	hcNow, hcAwaitTime, allowWrites, dataDir = clock.now, 0, true, t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() {
		hcClient, hcNow, hcAwaitTime, allowWrites, dataDir = origClient, origNow, origAwait, origWrites, origData
		placeholdersChanged()
	})

	admin, _ := createUser("boss", "a long enough password", true)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := hardcoverMux()
	do := func(u *User, method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	// step runs the queue until it has nothing it may run now.
	step := func() {
		t.Helper()
		for range 20 {
			if ran, _ := hcStep(context.Background()); !ran {
				return
			}
		}
		t.Fatal("queue doesn't empty")
	}
	job := func(id int64) *hcJob {
		t.Helper()
		j, err := hcJobByID(id)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	answer := func(b []byte) hcJobAnswer {
		t.Helper()
		var a hcJobAnswer
		if err := json.Unmarshal(b, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}

	// Off without a token.
	hcClient = nil
	do(admin, "GET", "/missing/hardcover?series=Other", "", http.StatusForbidden)
	hcClient = hardcover.New(hardcover.Config{URL: srv.URL, Token: "test-token", UserAgent: "test", Now: clock.now,
		Budget: hardcover.NewBudget(60, 10, 8)})
	// Editors only; no GET for changes; series must be in the library.
	do(alice, "GET", "/missing/hardcover?series=Other", "", http.StatusNotFound)
	do(alice, "POST", "/api/hardcover/candidates", `{"series":"Other"}`, http.StatusNotFound)
	do(admin, "GET", "/api/hardcover/candidates", "", http.StatusMethodNotAllowed)
	do(admin, "POST", "/api/hardcover/candidates", `{"series":"Nope"}`, http.StatusNotFound)
	if !strings.Contains(string(do(admin, "GET", "/missing/hardcover?series=other", "", http.StatusOK)), "isn't linked to Hardcover yet") {
		t.Error("unlinked page")
	}

	// Candidates: queued, then found; "Other" by Writer is the sure one.
	a := answer(do(admin, "POST", "/api/hardcover/candidates", `{"series":"Other"}`, http.StatusOK))
	if a.Job.State != "queued" {
		t.Fatalf("candidates: %+v", a.Job)
	}
	// Asking again while it waits answers the same request.
	if b := answer(do(admin, "POST", "/api/hardcover/candidates", `{"series":"Other"}`, http.StatusOK)); b.Job.ID != a.Job.ID {
		t.Error("the same request queued twice")
	}
	step()
	var cands hcCandidates
	if j := job(a.Job.ID); j.State != "done" || json.Unmarshal(j.Result, &cands) != nil ||
		len(cands.Candidates) != 2 || cands.Candidates[0].ID != 77 || !cands.Candidates[0].Sure || cands.Candidates[1].Sure ||
		cands.Candidates[0].URL != "https://hardcover.app/series/other" {
		t.Fatalf("candidates job: %+v %s", j, j.Result)
	}

	// Link and fetch.
	do(admin, "POST", "/api/hardcover/link", `{"series":"Other"}`, http.StatusBadRequest)
	a = answer(do(admin, "POST", "/api/hardcover/link", `{"series":"Other","hcId":77}`, http.StatusOK))
	step()
	if j := job(a.Job.ID); j.State != "done" {
		t.Fatalf("link: %+v", j)
	}
	l, _ := hcLinkFor("other")
	if l == nil || l.HCID != 77 || l.Snapshot == nil || len(l.Snapshot.Entries) != 5 || l.CheckedAt != clock.now().Unix() {
		t.Fatalf("link %+v", l)
	}
	page := string(do(admin, "GET", "/missing/hardcover?series=Other", "", http.StatusOK))
	for _, want := range []string{"Other Three", "Other Novella", "Set series to Other #4", "https://hardcover.app/books/other-three",
		`href="/book/get/` + four + `"`, `href="/book/get/` + other[0] + `"`,
		`In the library: <a href="/book/get/` + four + `">Other Four</a> by Writer`} {
		if !strings.Contains(page, want) {
			t.Errorf("review page lacks %q", want)
		}
	}
	s, _ := hcSeries("Other")
	rev, err := hcReviewFor(s, l)
	if err != nil || len(rev.Present) != 2 || len(rev.Untagged) != 1 || rev.Untagged[0].Book.UUID != four || !rev.Untagged[0].Checked ||
		len(rev.Missing) != 2 || !rev.Missing[0].Checked || rev.Missing[1].Checked {
		t.Fatalf("review %+v %v (books %v)", rev, err, other)
	}

	// Apply: the placeholders come from the stored list, not the request;
	// unknown ids and books are ignored.
	body := `{"series":"Other","placeholders":[3,35,999],"tag":[{"uuid":"` + four + `","hcBookId":4},{"uuid":"` + other[0] + `","hcBookId":4}],
		"title":"Injected","dismiss":[]}`
	var res hcApplyResult
	if err := json.Unmarshal(do(admin, "POST", "/api/hardcover/apply", body, http.StatusOK), &res); err != nil {
		t.Fatal(err)
	}
	if res.Placeholders != 2 || res.Tagged != 1 || len(res.Errors) != 0 {
		t.Fatalf("apply: %+v", res)
	}
	waitBulkEdits()
	var three *placeholder
	for _, p := range allPlaceholders() {
		if p.Meta.Title == "Other Three" {
			three = p
		}
	}
	if three == nil || three.Source != "hardcover" || three.Meta.Series != "Other" || three.Meta.SeriesIndex != "3" ||
		three.Meta.Date != "2010-03-04" || placeholderISBN(three.Meta) != "9780000000002" {
		t.Fatalf("placeholder %+v", three)
	}
	// Again: nothing more to make.
	_ = json.Unmarshal(do(admin, "POST", "/api/hardcover/apply", body, http.StatusOK), &res)
	if res.Placeholders != 0 {
		t.Errorf("applied twice: %+v", res)
	}
	if !strings.Contains(string(do(admin, "GET", "/missing", "", http.StatusOK)), "from Hardcover") {
		t.Error("/missing doesn't mark placeholders from Hardcover")
	}

	// The daily limit (8; 4 used: a search and a name lookup, then a list and its books): refreshes are held, not failed, until
	// midnight UTC, and survive a restart.
	for range 2 {
		a = answer(do(admin, "POST", "/api/hardcover/refresh", `{"series":"Other"}`, http.StatusOK))
		step()
		if j := job(a.Job.ID); j.State != "done" {
			t.Fatalf("refresh: %+v", j)
		}
	}
	a = answer(do(admin, "POST", "/api/hardcover/refresh", `{"series":"Other"}`, http.StatusOK))
	step()
	held := job(a.Job.ID)
	midnight := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if held.State != "held" || held.NotBefore != midnight.Unix() {
		t.Fatalf("over the limit: %+v", held)
	}
	hcSaveUsage()
	hcClient = hardcover.New(hardcover.Config{URL: srv.URL, Token: "test-token", UserAgent: "test", Now: clock.now,
		Budget: hardcover.NewBudget(60, 10, 8)})
	hcRestoreUsage() // as after a restart
	step()
	if j := job(a.Job.ID); j.State != "held" {
		t.Fatalf("after a restart, before midnight: %+v", j)
	}
	clock.set(midnight.Add(time.Second))
	step()
	if j := job(a.Job.ID); j.State != "done" {
		t.Fatalf("after midnight: %+v", j)
	}

	// Cancelling a waiting request.
	a = answer(do(admin, "POST", "/api/hardcover/refresh", `{"series":"Other"}`, http.StatusOK))
	do(admin, "DELETE", "/api/hardcover/jobs/"+strconv.FormatInt(a.Job.ID, 10), "", http.StatusNoContent)
	do(admin, "DELETE", "/api/hardcover/jobs/"+strconv.FormatInt(a.Job.ID, 10), "", http.StatusConflict)
	if j := job(a.Job.ID); j.State != "failed" || j.Error != "cancelled" {
		t.Errorf("cancelled: %+v", j)
	}

	// A batch of the author's series: Other refreshes; Wheel has no sure
	// match, so it waits for a choice. Other series are refused.
	do(admin, "POST", "/api/hardcover/batch", `{"author":"Writer","series":["Not Theirs"]}`, http.StatusBadRequest)
	do(admin, "POST", "/api/hardcover/batch", `{"author":"Writer","series":["Other","Wheel"]}`, http.StatusOK)
	step()
	var jobs struct{ Jobs []*hcJob }
	if err := json.Unmarshal(do(admin, "GET", "/api/hardcover/jobs?ids=1,2,3,4,5,6,7,8,9,10,11,12", "", http.StatusOK), &jobs); err != nil {
		t.Fatal(err)
	}
	var wheel, oth *hcJob
	for _, j := range jobs.Jobs {
		if j.Kind == hcJobBatch && j.Series == "Wheel" {
			wheel = j
		}
		if j.Kind == hcJobBatch && j.Series == "Other" {
			oth = j
		}
	}
	var lr hcLinkResult
	if wheel == nil || wheel.State != "done" || json.Unmarshal(wheel.Result, &lr) != nil || !lr.NeedsChoice || lr.Linked {
		t.Fatalf("batch Wheel: %+v", wheel)
	}
	if oth == nil || oth.State != "done" || json.Unmarshal(oth.Result, &lr) != nil || !lr.Linked {
		t.Fatalf("batch Other: %+v", oth)
	}
	if !strings.Contains(string(do(admin, "GET", "/missing/hardcover/author?author=Writer", "", http.StatusOK)), "linked to Other") {
		t.Error("author page")
	}
}

// "Not a match, create placeholder": a library book the review matched
// by title alone isn't the Hardcover book. It's never matched to it
// again, and the Hardcover book gets a placeholder in the series.
func TestHardcoverNotAMatch(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	placeholdersChanged()
	mort := addTestBook(t, db, "Mort", []string{"Terry Pratchett"}, nil, "Discworld", "epub")
	aaron := addTestBook(t, db, "The Last Hero: A Life of Henry Aaron", []string{"Howard Bryant"}, nil, "", "epub")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	origClient, origWrites := hcClient, allowWrites
	hcClient, allowWrites = hardcover.New(hardcover.Config{URL: "https://127.0.0.1:1/", Token: "test-token"}), true
	t.Cleanup(func() { hcClient, allowWrites = origClient, origWrites; placeholdersChanged() })
	pratchett := []string{"Terry Pratchett"}
	if err := hcSaveLink("Discworld", &hardcover.SeriesDetail{Series: hardcover.Series{ID: 9, Name: "Discworld", Slug: "discworld"},
		Entries: []hardcover.Entry{
			{BookID: 1, Title: "Mort", Position: pos(1), Authors: pratchett},
			{BookID: 27, Title: "The Last Hero", Position: pos(27), Authors: pratchett, Released: "2001-01-01"},
		}}, nil); err != nil {
		t.Fatal(err)
	}
	admin, _ := createUser("boss", "a long enough password", true)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := http.NewServeMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return requireEditor(requireLookups(requireHardcover(h))) }
	mux.HandleFunc("POST /api/hardcover/notmatch", wrap(hardcoverNotMatchHandler))
	mux.HandleFunc("GET /missing/hardcover", wrap(hardcoverReviewPage))
	mux.HandleFunc("POST /api/hardcover/undismiss", wrap(hardcoverUndismissHandler))
	do := func(u *User, method, path, body string, want int) string {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.String()
	}
	review := func() hcReview {
		t.Helper()
		s, _ := hcSeries("Discworld")
		l, _ := hcLinkFor("Discworld")
		rev, err := hcReviewFor(s, l)
		if err != nil {
			t.Fatal(err)
		}
		return rev
	}
	if rev := review(); len(rev.Untagged) != 1 || rev.Untagged[0].Book.UUID != aaron || rev.Untagged[0].Strength != hcWeak {
		t.Fatalf("before: %+v", rev.Untagged)
	}
	if !strings.Contains(do(admin, "GET", "/missing/hardcover?series=Discworld", "", http.StatusOK), `data-notmatch="27"`) {
		t.Error("no Not a match button")
	}

	body := `{"series":"Discworld","uuid":"` + aaron + `","hcBookId":27}`
	do(alice, "POST", "/api/hardcover/notmatch", body, http.StatusNotFound)
	do(admin, "GET", "/api/hardcover/notmatch", "", http.StatusMethodNotAllowed)
	// Only a match the review suggests: not Mort, which is present.
	do(admin, "POST", "/api/hardcover/notmatch", `{"series":"Discworld","uuid":"`+mort+`","hcBookId":1}`, http.StatusConflict)
	var res struct{ Placeholder, Error string }
	if err := json.Unmarshal([]byte(do(admin, "POST", "/api/hardcover/notmatch", body, http.StatusOK)), &res); err != nil || res.Placeholder == "" || res.Error != "" {
		t.Fatalf("not a match: %+v %v", res, err)
	}
	p := placeholderByUUID(res.Placeholder)
	if p == nil || p.Meta.Title != "The Last Hero" || p.Meta.Series != "Discworld" || p.Meta.SeriesIndex != "27" || p.Source != "hardcover" {
		t.Fatalf("placeholder %+v", p)
	}
	rev := review()
	if len(rev.Untagged) != 0 || len(rev.Missing) != 1 || rev.Missing[0].Placeholder != res.Placeholder {
		t.Fatalf("after: untagged %+v missing %+v", rev.Untagged, rev.Missing)
	}
	// Again: the match isn't suggested any more.
	do(admin, "POST", "/api/hardcover/notmatch", body, http.StatusConflict)

	// It's listed with the dismissed, and Restore match undoes it: the book
	// is matched again (its placeholder stays).
	if len(rev.Dismissed) != 1 || !rev.Dismissed[0].Refused || rev.Dismissed[0].Book.UUID != aaron || rev.Dismissed[0].Entry.BookID != 27 {
		t.Fatalf("dismissed %+v", rev.Dismissed)
	}
	if page := do(admin, "GET", "/missing/hardcover?series=Discworld", "", http.StatusOK); !strings.Contains(page, `data-unrefuse="27"`) {
		t.Error("no Restore match")
	}
	undo := `{"series":"Discworld","ids":[],"notMatch":[{"uuid":"` + aaron + `","hcBookId":27}]}`
	do(alice, "POST", "/api/hardcover/undismiss", undo, http.StatusNotFound)
	do(admin, "POST", "/api/hardcover/undismiss", undo, http.StatusNoContent)
	if rev := review(); len(rev.Untagged) != 1 || rev.Untagged[0].Book.UUID != aaron || len(rev.Dismissed) != 0 {
		t.Fatalf("after restoring: untagged %+v dismissed %+v", rev.Untagged, rev.Dismissed)
	}
	if placeholderByUUID(res.Placeholder) == nil {
		t.Error("restoring removed the placeholder")
	}
	// Restore all undoes them too.
	do(admin, "POST", "/api/hardcover/notmatch", body, http.StatusOK)
	do(admin, "POST", "/api/hardcover/undismiss", `{"series":"Discworld","ids":[]}`, http.StatusNoContent)
	if rev := review(); len(rev.Untagged) != 1 || len(rev.Dismissed) != 0 {
		t.Fatalf("after Restore all: untagged %+v dismissed %+v", rev.Untagged, rev.Dismissed)
	}
}
