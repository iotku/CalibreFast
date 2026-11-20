package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A series an editor gave a count of books is complete while the library
// has 1…count, with no Hardcover token at all; the rest show as gaps.
func TestSeriesVerified(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	placeholdersChanged()
	t.Cleanup(placeholdersChanged)
	t.Chdir(t.TempDir())
	origClient, origWrites := hcClient, allowWrites
	hcClient, allowWrites = nil, true // no Hardcover
	t.Cleanup(func() { hcClient, allowWrites = origClient, origWrites })
	for _, n := range []float64{1, 2, 3, 2.5} {
		uuid := addTestBook(t, db, "Book", []string{"W"}, nil, "Saga", "epub")
		mustExec(t, db, `UPDATE books SET series_index = ? WHERE uuid = ?`, n, uuid)
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	refreshAllCaches() // other tests' series lists
	t.Cleanup(refreshAllCaches)
	admin, _ := createUser("boss", "a long enough password", true)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/series/verified", requireEditor(seriesVerifyHandler))
	mux.HandleFunc("DELETE /api/series/verified", requireEditor(seriesUnverifyHandler))
	mux.HandleFunc("GET /series/", filteredPageHandler(categoryByKey("series")))
	mux.HandleFunc("GET /series", aggregatePageHandler(categoryByKey("series")))
	do := func(u *User, method, path, body string, want int) string {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.String()
	}

	if _, ok := seriesStatusOf("Saga"); ok {
		t.Fatal("status before anyone said anything")
	}
	if !strings.Contains(do(admin, "GET", "/series/Saga", "", http.StatusOK), `id="series-verify"`) {
		t.Error("no Mark complete for an editor")
	}
	if strings.Contains(do(alice, "GET", "/series/Saga", "", http.StatusOK), `id="series-verify"`) {
		t.Error("Mark complete for a reader")
	}
	do(alice, "POST", "/api/series/verified", `{"series":"Saga","total":3}`, http.StatusNotFound)
	do(admin, "GET", "/api/series/verified", "", http.StatusMethodNotAllowed)
	do(admin, "POST", "/api/series/verified", `{"series":"Saga","total":0}`, http.StatusBadRequest)
	do(admin, "POST", "/api/series/verified", `{"series":"Nowhere","total":3}`, http.StatusNotFound)

	// Three books, 1–3 here (2.5 doesn't count): complete.
	var st seriesStatus
	if err := json.Unmarshal([]byte(do(admin, "POST", "/api/series/verified", `{"series":"saga","total":3}`, http.StatusOK)), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Complete || st.Have != 3 || st.Source != statusVerified {
		t.Fatalf("status %+v", st)
	}
	if page := do(alice, "GET", "/series/Saga", "", http.StatusOK); !strings.Contains(page, "Every book is in the library") {
		t.Error("series page doesn't say it's complete")
	}
	if page := do(alice, "GET", "/series?complete=1", "", http.StatusOK); !strings.Contains(page, `data-complete`) || !strings.Contains(page, "Complete (1)") {
		t.Error("/series?complete=1 doesn't list it")
	}

	// Five: 4 and 5 are missing, and listed as gaps.
	do(admin, "POST", "/api/series/verified", `{"series":"Saga","total":5}`, http.StatusOK)
	if st, _ := seriesStatusOf("Saga"); st.Complete || st.Have != 3 || st.Numbered != 5 {
		t.Errorf("status %+v", st)
	}
	if g := seriesGaps(); len(g) != 1 || !slices.Equal(g[0].Numbers, []int{4, 5}) {
		t.Errorf("gaps %+v", g)
	}
	// …and a ring three-fifths full, not a ✓, on the series list.
	if page := do(alice, "GET", "/series", "", http.StatusOK); !strings.Contains(page, `aria-label="3 of 5 books"`) || !strings.Contains(page, `stroke-dasharray="60 100"`) || strings.Contains(page, `data-complete`) {
		t.Error("/series doesn't show Saga's ring")
	}

	// Forgotten: no status, no gaps.
	do(admin, "DELETE", "/api/series/verified?series=Saga", "", http.StatusNoContent)
	if _, ok := seriesStatusOf("Saga"); ok {
		t.Error("status after forgetting")
	}
	if g := seriesGaps(); len(g) != 0 {
		t.Errorf("gaps %+v", g)
	}
}

// A book numbered past the count: the series grew, and it's complete
// only with every book up to the new one.
func TestVerifiedStatuses(t *testing.T) {
	cat := &catalog{seriesNames: map[int32]string{1: "Saga"}}
	for _, n := range []float64{1, 2, 4} {
		b := catalogBook{Series: 1}
		b.SeriesIndex = n
		cat.books = append(cat.books, b)
	}
	st := verifiedStatuses(cat, map[string]seriesCount{"saga": {Total: 2}})["saga"]
	if st.Complete || st.Numbered != 4 || st.Have != 3 || st.Total != 2 {
		t.Errorf("status %+v", st)
	}
}

// A series whose books aren't numbered (all #1 here) is counted by its
// books. Placeholders stand in for the missing ones on /missing, but
// aren't books the library has: 10 books, 8 here and 2 placeholders is
// 8 of 10, with nothing more missing.
func TestSeriesBookCount(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	placeholdersChanged()
	t.Cleanup(placeholdersChanged)
	t.Chdir(t.TempDir())
	origClient, origWrites := hcClient, allowWrites
	hcClient, allowWrites = nil, true
	t.Cleanup(func() { hcClient, allowWrites = origClient, origWrites })
	for range 8 {
		addTestBook(t, db, "Tale", []string{"W"}, nil, "Tales", "epub") // all #1
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	refreshAllCaches()
	t.Cleanup(refreshAllCaches)
	admin, _ := createUser("boss", "a long enough password", true)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/series/verified", requireEditor(seriesVerifyHandler))
	mux.HandleFunc("GET /series/", filteredPageHandler(categoryByKey("series")))
	do := func(method, path, body string, want int) string {
		t.Helper()
		rec := call(t, mux, admin, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.String()
	}
	// Unset: the dialog offers the number of books, not a highest number.
	page := do("GET", "/series/Tales", "", http.StatusOK)
	if !strings.Contains(page, `id="series-verify-books" min="0" max="10000" step="1" value="8"`) || !strings.Contains(page, `id="series-verify-total" min="0" max="10000" step="1" value=""`) {
		t.Error("dialog doesn't offer the book count")
	}
	do("POST", "/api/series/verified", `{"series":"Tales","total":0,"books":0}`, http.StatusBadRequest)
	do("POST", "/api/series/verified", `{"series":"Tales","books":-1}`, http.StatusBadRequest)

	do("POST", "/api/series/verified", `{"series":"Tales","books":10}`, http.StatusOK)
	st, _ := seriesStatusOf("Tales")
	if st.Complete || st.Books != 10 || st.BooksHave != 8 || st.Numbered != 0 {
		t.Fatalf("status %+v", st)
	}
	if g := seriesGaps(); len(g) != 1 || g[0].BooksMissing != 2 || len(g[0].Numbers) != 0 {
		t.Fatalf("gaps %+v", g)
	}
	for _, title := range []string{"Lost One", "Lost Two"} {
		if err := savePlaceholder(&placeholder{Meta: OPFMetadata{Title: title, Series: "Tales"}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := seriesStatusOf("Tales"); st.Complete || st.BooksHave != 8 {
		t.Errorf("placeholders counted as books: %+v", st)
	}
	if g := seriesGaps(); len(g) != 0 {
		t.Errorf("placeholders don't stand in for missing books: %+v", g)
	}
	if page := do("GET", "/series/Tales", "", http.StatusOK); !strings.Contains(page, "8 of 10 books") {
		t.Error("series page doesn't say 8 of 10 books")
	}
	do("POST", "/api/series/verified", `{"series":"Tales","books":8}`, http.StatusOK)
	if st, _ := seriesStatusOf("Tales"); !st.Complete {
		t.Errorf("8 of 8: %+v", st)
	}
	// Both: the count and the numbers must hold (they're all #1: 2 is missing).
	do("POST", "/api/series/verified", `{"series":"Tales","total":2,"books":8}`, http.StatusOK)
	if st, _ := seriesStatusOf("Tales"); st.Complete || st.Numbered != 2 || st.Have != 1 || st.BooksHave != 8 {
		t.Errorf("both: %+v", st)
	}
}

// The ring's share: by number, by count, or the lower of the two; full
// only when complete.
func TestSeriesCoverage(t *testing.T) {
	for _, c := range []struct {
		st            seriesStatus
		have, of, pct int
		text          string
	}{
		{seriesStatus{Numbered: 7, Have: 3}, 3, 7, 42, "3 of 7 books"},
		{seriesStatus{Books: 4, BooksHave: 1}, 1, 4, 25, "1 of 4 books"},
		{seriesStatus{Numbered: 5, Have: 4, Books: 10, BooksHave: 2}, 2, 10, 20, "2 of 10 books"},
		{seriesStatus{Numbered: 5, Have: 1, Books: 10, BooksHave: 9}, 1, 5, 20, "1 of 5 books"},
		{seriesStatus{Numbered: 3}, 0, 3, 0, "0 of 3 books"},
		{seriesStatus{Numbered: 1}, 0, 1, 0, "0 of 1 book"},
		{seriesStatus{Numbered: 200, Have: 199}, 199, 200, 99, "199 of 200 books"},
		{seriesStatus{Numbered: 2, Have: 2, Complete: true}, 2, 2, 100, "2 of 2 books"},
		{seriesStatus{}, 0, 0, 0, "0 of 0 books"},
	} {
		have, of := c.st.Coverage()
		if have != c.have || of != c.of || c.st.Percent() != c.pct || c.st.CoverageText() != c.text {
			t.Errorf("%+v: %d/%d %d%% %q", c.st, have, of, c.st.Percent(), c.st.CoverageText())
		}
	}
	if statusPtr(map[string]seriesStatus{"a": {Source: statusHardcover}}, "A") != nil {
		t.Error("a ring for a series counted by nothing")
	}
	if p := statusPtr(map[string]seriesStatus{"a": {Numbered: 2}}, "A"); p == nil || p.Numbered != 2 {
		t.Error("no status for a counted series")
	}
}
