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
	if page := do(alice, "GET", "/series/Saga", "", http.StatusOK); !strings.Contains(page, "Every numbered book is in the library") {
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
	st := verifiedStatuses(cat, map[string]int{"saga": 2})["saga"]
	if st.Complete || st.Numbered != 4 || st.Have != 3 || st.Total != 2 {
		t.Errorf("status %+v", st)
	}
}
