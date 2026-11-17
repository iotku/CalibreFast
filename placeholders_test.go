package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func placeholderMux() *http.ServeMux {
	mux := seriesMux()
	mux.HandleFunc("POST /api/placeholders", requireWrites(placeholderCreateHandler))
	mux.HandleFunc("PUT /api/placeholders/{uuid}", requireWrites(placeholderUpdateHandler))
	mux.HandleFunc("DELETE /api/placeholders/{uuid}", requireWrites(placeholderDeleteHandler))
	mux.HandleFunc("GET /missing", requireWrites(missingPageHandler))
	mux.HandleFunc("GET /missing/{uuid}", placeholderPageHandler)
	for _, k := range []string{"series", "author", "tag"} {
		c := categoryByKey(k)
		mux.HandleFunc("/"+c.URLPrefix+"/", filteredPageHandler(c))
	}
	return mux
}

func TestPlaceholders(t *testing.T) {
	_, _, other := seriesFixture(t)
	origData, origWrites := dataDir, allowWrites
	dataDir, allowWrites = t.TempDir(), true
	placeholdersChanged()
	t.Cleanup(func() { dataDir, allowWrites = origData, origWrites; placeholdersChanged() })

	admin, _ := createUser("boss", "a long enough password", true)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := placeholderMux()
	do := func(u *User, method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	create := func(body string) string {
		t.Helper()
		var res struct{ UUID, URL string }
		if err := json.Unmarshal(do(admin, "POST", "/api/placeholders", body, http.StatusOK), &res); err != nil {
			t.Fatal(err)
		}
		if res.URL != "/missing/"+res.UUID {
			t.Errorf("url %q", res.URL)
		}
		return res.UUID
	}
	uuids := func(path string) []string {
		t.Helper()
		var books []Book
		if err := json.Unmarshal(do(alice, "GET", path, "", http.StatusOK), &books); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range books {
			if b.Placeholder != nil {
				out = append(out, "ph:"+b.Title)
			} else {
				out = append(out, b.UUID)
			}
		}
		return out
	}

	// Only admins make them, and they need a title.
	do(alice, "POST", "/api/placeholders", `{"title":"Nope"}`, http.StatusNotFound)
	do(admin, "POST", "/api/placeholders", `{"title":"  "}`, http.StatusBadRequest)
	do(admin, "POST", "/api/placeholders", `{"title":"X","series":"Other","series_index":"two"}`, http.StatusBadRequest)
	third := create(`{"title":"Other Three","creators":"Writer","series":"Other","series_index":"3","date":"2010-03-04"}`)
	p := placeholderByUUID(third)
	if p == nil || p.Meta.Series != "Other" || p.index() != 3 || p.Meta.Date != "2010-03-04" || !slices.Equal(p.Meta.Creators, []string{"Writer"}) {
		t.Fatalf("saved %+v", p)
	}

	// Among the series' books in order, and the author's by title; not on
	// a tag's page.
	if got, want := uuids("/series/Other?api=1&page=1"), []string{other[0], other[1], "ph:Other Three"}; !slices.Equal(got, want) {
		t.Errorf("series page %q, want %q", got, want)
	}
	if got := uuids("/author/Writer?api=1&page=1"); len(got) != 7 || got[5] != "ph:Other Three" {
		t.Errorf("author page %q: want it between Other One and Other Two, by title", got)
	}
	if got := uuids("/tag/Fantasy?api=1&page=1"); slices.Contains(got, "ph:Other Three") {
		t.Errorf("tag page shows a placeholder: %q", got)
	}
	if body := string(do(alice, "GET", "/series/Other", "", http.StatusOK)); !strings.Contains(body, `data-total-books="3"`) {
		t.Error("series page total doesn't count the placeholder")
	}
	if body := string(do(alice, "GET", "/missing/"+third, "", http.StatusOK)); !strings.Contains(body, "Other Three") || strings.Contains(body, "ph-form") {
		t.Error("a reader's view of the placeholder page")
	}
	do(alice, "GET", "/missing", "", http.StatusNotFound)

	// On deck: after book 2, the missing book 3; after book 1, a missing
	// 1.5 comes before the library's book 2.
	status := func(uuid, s string, at int64) {
		t.Helper()
		if err := setStatus(alice.ID, uuid, s, "manual"); err != nil {
			t.Fatal(err)
		}
		mustExec(t, appDB, `UPDATE reading_status SET updated_at = ? WHERE user_id = ? AND book_uuid = ?`, at, alice.ID, uuid)
	}
	deck := func() []string {
		t.Helper()
		var h homeAnswer
		if err := json.Unmarshal(do(alice, "GET", "/api/me/home", "", http.StatusOK), &h); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range h.OnDeck {
			out = append(out, b.UUID)
		}
		return out
	}
	status(other[0], "finished", 100)
	if got := deck(); !slices.Equal(got, []string{other[1]}) {
		t.Fatalf("on deck %q, want book 2", got)
	}
	half := create(`{"title":"Other One and a Half","series":"Other","series_index":"1.5"}`)
	if got := deck(); !slices.Equal(got, []string{half}) {
		t.Fatalf("on deck %q, want the missing 1.5", got)
	}
	do(alice, "DELETE", "/api/me/on-deck/"+half, "", http.StatusNoContent)
	if got := deck(); len(got) != 0 {
		t.Fatalf("dismissed placeholder still on deck: %q", got)
	}
	status(other[1], "finished", 200)
	if got := deck(); !slices.Equal(got, []string{third}) {
		t.Fatalf("on deck %q, want the missing book 3", got)
	}

	// No gaps (Other has 1 and 2); editing keeps it; /missing lists them.
	if gaps := seriesGaps(currentCatalog(), allPlaceholders()); len(gaps) != 0 {
		t.Errorf("gaps %+v", gaps)
	}
	create(`{"title":"Other Six","series":"other","series_index":"6"}`)
	do(admin, "PUT", "/api/placeholders/"+half, `{"title":"Other One and a Half","series":"Other","series_index":"1.5","creators":"Writer"}`, http.StatusOK)
	if p := placeholderByUUID(half); p == nil || p.Meta.Creators[0] != "Writer" || p.CreatedAt == 0 {
		t.Errorf("edited %+v", p)
	}
	if body := string(do(admin, "GET", "/missing", "", http.StatusOK)); !strings.Contains(body, "Other Three") || !strings.Contains(body, "Other Six") {
		t.Error("/missing doesn't list the placeholders")
	}

	// A book added for it (same series and number, or same title and an
	// author in common) resolves it.
	resolvePlaceholders(&OPFMetadata{Title: "Something Else", Series: "OTHER", SeriesIndex: "3"})
	if placeholderByUUID(third) != nil {
		t.Error("book 3 added, placeholder kept")
	}
	resolvePlaceholders(&OPFMetadata{Title: "other one and a half", Creators: []string{"Someone"}})
	if placeholderByUUID(half) == nil {
		t.Error("resolved by title alone")
	}
	resolvePlaceholders(&OPFMetadata{Title: "other one and a half", Creators: []string{"writer"}})
	if placeholderByUUID(half) != nil {
		t.Error("same title and author, placeholder kept")
	}
	do(admin, "DELETE", "/api/placeholders/"+half, "", http.StatusNotFound)
	do(alice, "GET", "/missing/"+half, "", http.StatusNotFound)
}

func TestSeriesGaps(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	for _, n := range []float64{1, 2, 5, 2.5} {
		uuid := addTestBook(t, db, "Book", []string{"W"}, nil, "Saga", "epub")
		mustExec(t, db, `UPDATE books SET series_index = ? WHERE uuid = ?`, n, uuid)
	}
	uuid := addTestBook(t, db, "Year", []string{"W"}, nil, "Annual", "epub")
	mustExec(t, db, `UPDATE books SET series_index = 2004 WHERE uuid = ?`, uuid)
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	phs := []*placeholder{
		{Meta: OPFMetadata{Title: "Four", Series: "saga", SeriesIndex: "4"}},
		{Meta: OPFMetadata{Title: "Eight", Series: "Saga", SeriesIndex: "8"}},
		{Meta: OPFMetadata{Title: "Alone", Series: "Nowhere", SeriesIndex: "3"}}, // no library books: not listed
	}
	gaps := seriesGaps(currentCatalog(), phs)
	if len(gaps) != 1 || gaps[0].Series != "Saga" || !slices.Equal(gaps[0].Numbers, []int{3, 6, 7}) || gaps[0].Authors != "W" {
		t.Fatalf("gaps %+v, want Saga #3, #6, #7 only (years left out)", gaps)
	}
}

func TestWithPlaceholder(t *testing.T) {
	file := OPFMetadata{Title: "file title", Creators: []string{"F"}, Publisher: "FP", Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "1"}, {Scheme: "uuid", Value: "u"}}}
	p := &placeholder{Meta: OPFMetadata{Title: "Real", Series: "S", SeriesIndex: "2", Identifiers: []opfIdentifier{{Scheme: "ISBN", Value: "9"}}}}
	m := withPlaceholder(file, p)
	if m.Title != "Real" || m.Creators[0] != "F" || m.Publisher != "FP" || m.Series != "S" || m.SeriesIndex != "2" {
		t.Errorf("merged %+v", m)
	}
	if len(m.Identifiers) != 2 || m.Identifiers[0].Scheme != "uuid" || m.Identifiers[1].Value != "9" {
		t.Errorf("identifiers %+v", m.Identifiers)
	}
	var series bool
	for _, x := range m.Meta {
		series = series || x.Name == "calibre:series" && x.Content == "S"
	}
	if !series {
		t.Errorf("series not in the OPF meta: %+v", m.Meta)
	}
}
