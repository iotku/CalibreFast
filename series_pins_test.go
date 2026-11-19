package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"testing"
)

// seriesFixture: a series whose books are all number 1 (so publication
// date orders them; one with an unknown date goes last), and a second
// series numbered as usual.
func seriesFixture(t *testing.T) (db *sql.DB, wheel, other []string) {
	t.Helper()
	db = catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	add := func(title, series string, index float64, pubdate string) string {
		uuid := addTestBook(t, db, title, []string{"Writer"}, []string{"Fantasy"}, series, "epub")
		mustExec(t, db, `UPDATE books SET series_index = ?, pubdate = ?, sort = ? WHERE uuid = ?`, index, pubdate, title, uuid)
		return uuid
	}
	// Added out of order, so neither Latest nor title order is the answer.
	undated := add("A Undated", "Wheel", 1, "0101-01-01 00:00:00+00:00")
	third := add("B Third", "Wheel", 1, "1992-05-01 00:00:00+00:00")
	first := add("C First", "Wheel", 1, "1990-01-15 00:00:00+00:00")
	second := add("D Second", "Wheel", 1, "1991-02-01T00:00:00+00:00")
	wheel = []string{first, second, third, undated}
	o2 := add("Other Two", "Other", 2, "2001-01-01 00:00:00+00:00")
	o1 := add("Other One", "Other", 1, "2005-01-01 00:00:00+00:00")
	other = []string{o1, o2}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	return db, wheel, other
}

func TestSeriesOrder(t *testing.T) {
	db, wheel, other := seriesFixture(t)
	uuids := func(slots []int32) []string {
		c := currentCatalog()
		var out []string
		for _, s := range slots {
			out = append(out, c.books[s].UUID)
		}
		return out
	}
	ix := currentRecIndex()
	if got := uuids(seriesBooks(ix, "Wheel")); !slices.Equal(got, wheel) {
		t.Errorf("Wheel in catalog order %q, want %q (by date, unknown last)", got, wheel)
	}
	if got := uuids(seriesBooks(ix, "other")); !slices.Equal(got, other) {
		t.Errorf("Other %q, want %q (by number first)", got, other)
	}
	// The series page (metadata.db) agrees.
	got, err := scanStrings(db.Query(`SELECT b.uuid FROM books b JOIN books_series_link l ON l.book = b.id
		JOIN series s ON s.id = l.series WHERE s.name = 'Wheel' ORDER BY ` + categoryByKey("series").bookOrder()))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, wheel) {
		t.Errorf("series page order %q, want %q", got, wheel)
	}
	// Ties on number and date: by title as sorted.
	a := &catalogBook{Book: Book{SeriesIndex: 1, PubDate: "2000-01-01"}, Sort: "Alpha"}
	b := &catalogBook{Book: Book{SeriesIndex: 1, PubDate: "2000-01-01 12:00:00+00:00"}, Sort: "Beta"}
	if !seriesLess(a, b) || seriesLess(b, a) {
		t.Error("same number and day: not by sort")
	}
}

func seriesMux() *http.ServeMux {
	mux := pinMux()
	mux.HandleFunc("PUT /api/me/tag-pins/{name}", requireUser(tagPinHandler))
	mux.HandleFunc("PUT /api/me/series-pins/{name}", requireUser(seriesPinHandler))
	mux.HandleFunc("DELETE /api/me/series-pins/{name}", requireUser(seriesUnpinHandler))
	mux.HandleFunc("PATCH /api/me/series-pins/{name}", requireUser(seriesPinPatchHandler))
	mux.HandleFunc("DELETE /api/me/on-deck/{uuid}", requireUser(onDeckDismissHandler))
	mux.HandleFunc("GET /api/me/home", requireUser(homeHandler))
	return mux
}

func TestSeriesPins(t *testing.T) {
	_, wheel, _ := seriesFixture(t)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := seriesMux()
	do := func(method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, alice, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s %s: %d %s (want %d)", method, path, body, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	rows := func() []homeShelf {
		t.Helper()
		got, err := pinnedShelves(alice, shelfRowsMax)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	names := func() []string {
		var out []string
		for _, r := range rows() {
			out = append(out, r.Kind+":"+r.Name)
		}
		return out
	}
	books := func(r homeShelf) []string {
		var out []string
		for _, b := range r.Books {
			out = append(out, b.UUID)
		}
		return out
	}

	do("PUT", "/api/me/tag-pins/Fantasy", "", http.StatusNoContent)
	do("PUT", "/api/me/series-pins/Wheel", "", http.StatusNoContent)
	do("PUT", "/api/me/series-pins/wheel", "", http.StatusNoContent) // already: no change
	do("PUT", "/api/me/series-pins/Nothing", "", http.StatusNotFound)
	do("PATCH", "/api/me/series-pins/Other", `{"show_finished":true}`, http.StatusNotFound)
	if got := names(); !slices.Equal(got, []string{"tag:Fantasy", "series:Wheel"}) {
		t.Fatalf("rows %q", got)
	}
	if r := rows()[1]; !slices.Equal(books(r), wheel) || r.Count != 4 || r.Href != "/series/Wheel" {
		t.Fatalf("series row %+v", r)
	}

	// Finished books are left out, until Show finished.
	if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, 'finished', 'manual', 1)`, alice.ID, wheel[0]); err != nil {
		t.Fatal(err)
	}
	if r := rows()[1]; !slices.Equal(books(r), wheel[1:]) || r.Finished != 1 || r.ShowFinished {
		t.Fatalf("finished hidden: %+v", r)
	}
	do("PATCH", "/api/me/series-pins/Wheel", `{"show_finished":true}`, http.StatusNoContent)
	if r := rows()[1]; !slices.Equal(books(r), wheel) || !r.ShowFinished {
		t.Fatalf("finished shown: %+v", r)
	}

	// One order with shelves and tags.
	do("PUT", "/api/me/pins", `{"rows":[{"series":"wheel"},{"tag":"Fantasy"}]}`, http.StatusNoContent)
	if got := names(); !slices.Equal(got, []string{"series:Wheel", "tag:Fantasy"}) {
		t.Fatalf("after reorder %q", got)
	}
	do("PUT", "/api/me/pins", `{"rows":[{"series":"Wheel","tag":"Fantasy"}]}`, http.StatusBadRequest)
	do("PUT", "/api/me/series-pins/Other", "", http.StatusNoContent) // at the end
	if got := names(); !slices.Equal(got, []string{"series:Wheel", "tag:Fantasy", "series:Other"}) {
		t.Fatalf("new pin %q", got)
	}
	do("DELETE", "/api/me/series-pins/WHEEL", "", http.StatusNoContent)
	if got := names(); !slices.Equal(got, []string{"tag:Fantasy", "series:Other"}) {
		t.Fatalf("after unpin %q", got)
	}
	// A home layout may name a series row.
	if !validLayout(homeLayout{Order: []string{"k:wanted", "r:Other"}, Hidden: []string{"wanted"}}) {
		t.Error("layout with On deck and a series refused")
	}
}

func TestOnDeck(t *testing.T) {
	_, wheel, other := seriesFixture(t)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := seriesMux()
	home := func() homeAnswer {
		t.Helper()
		rec := call(t, mux, alice, "GET", "/api/me/home", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("home: %d %s", rec.Code, rec.Body)
		}
		var h homeAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	deck := func(h homeAnswer) []string {
		var out []string
		for _, b := range h.OnDeck {
			out = append(out, b.UUID)
		}
		return out
	}
	status := func(uuid, s string, at int64) {
		t.Helper()
		if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)
			ON CONFLICT (user_id, book_uuid) DO UPDATE SET status = excluded.status, updated_at = excluded.updated_at`, alice.ID, uuid, s, at); err != nil {
			t.Fatal(err)
		}
	}
	if h := home(); len(h.OnDeck) != 0 {
		t.Fatalf("nothing finished: %v", deck(h))
	}

	// Book 1 finished: book 2 (by date: all are number 1) is on deck, and
	// not in Recommended's next in series.
	status(wheel[0], "finished", 100)
	h := home()
	if got := deck(h); !slices.Equal(got, []string{wheel[1]}) {
		t.Fatalf("on deck %q, want %q", got, wheel[1])
	}
	for _, r := range h.Recommended {
		if r.Book.UUID == wheel[1] {
			t.Fatalf("on deck and recommended: %+v", r)
		}
	}

	// Finishing a later one moves on past it; another series finished
	// since comes first.
	status(wheel[2], "finished", 200)
	status(other[0], "finished", 300)
	if got := deck(home()); !slices.Equal(got, []string{other[1], wheel[3]}) {
		t.Fatalf("on deck %q", got)
	}
	// Being read: in Continue reading instead.
	status(wheel[3], "reading", 400)
	if got := deck(home()); !slices.Equal(got, []string{other[1]}) {
		t.Fatalf("on deck %q (the one being read left out)", got)
	}

	// ✕: gone for good.
	if rec := call(t, mux, alice, "DELETE", "/api/me/on-deck/"+other[1], ""); rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if got := deck(home()); len(got) != 0 {
		t.Fatalf("after dismiss %q", got)
	}
	if rec := call(t, mux, alice, "DELETE", "/api/me/on-deck/no-such-book", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("dismiss unknown: %d", rec.Code)
	}
	if rec := call(t, mux, nil, "DELETE", "/api/me/on-deck/"+other[1], ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", rec.Code)
	}
}


// Downloads ahead of time: On Deck first, then Saved, N in all.
func TestSavedBooksOnDeck(t *testing.T) {
	_, wheel, other := seriesFixture(t)
	alice, _ := createUser("alice", "a long enough password", false)
	mux := seriesMux()
	mux.HandleFunc("GET /api/me/saved-books", requireUser(savedBooksHandler))
	status := func(uuid, s string, at int64) {
		t.Helper()
		if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)`, alice.ID, uuid, s, at); err != nil {
			t.Fatal(err)
		}
	}
	list := func(n int) []savedBook {
		t.Helper()
		rec := call(t, mux, alice, "GET", "/api/me/saved-books?limit="+strconv.Itoa(n), "")
		var got []savedBook
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		return got
	}
	status(wheel[0], "finished", 1) // On Deck: wheel[1]
	status(other[0], "finished", 2) // On Deck: other[1] (finished more recently: first)
	status(wheel[3], "wanted", 10)
	status(wheel[1], "wanted", 5) // On Deck and Saved: listed once, as On Deck
	status(wheel[2], "wanted", 20)
	got := list(4)
	var ids []string
	for _, b := range got {
		ids = append(ids, b.UUID)
	}
	if want := []string{other[1], wheel[1], wheel[3], wheel[2]}; !slices.Equal(ids, want) {
		t.Fatalf("got %q, want %q", ids, want)
	}
	if !got[0].OnDeck || !got[1].OnDeck || got[2].OnDeck || got[2].SavedAt != 10*1000 {
		t.Errorf("flags: %+v", got)
	}
	if got := list(1); len(got) != 1 || got[0].UUID != other[1] {
		t.Errorf("limit 1: %+v", got)
	}
}

func TestAuthorSeries(t *testing.T) {
	db, wheel, _ := seriesFixture(t)
	solo := addTestBook(t, db, "Standalone", []string{"Writer"}, nil, "", "epub")
	elsewhere := addTestBook(t, db, "Elsewhere", []string{"Someone Else"}, nil, "Third", "epub")
	catalogChanged(solo, elsewhere)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	want := []AuthorSeries{
		{Name: "Wheel", Href: "/series/Wheel", Count: 4},
		{Name: "Other", Href: "/series/Other", Count: 2},
	}
	if got := authorSeries([]string{"writer"}); !slices.Equal(got, want) {
		t.Errorf("authorSeries = %+v, want %+v (most books first, other authors' series left out)", got, want)
	}
	if got := authorSeries([]string{"Nobody"}); len(got) != 0 {
		t.Errorf("unknown author: %+v", got)
	}

	// Cached per catalog: the same index until a book changes.
	ix := currentAuthorSeriesIndex()
	if currentAuthorSeriesIndex() != ix {
		t.Error("index rebuilt without a change")
	}
	// Moving a book out of Wheel, as an edit does (catalogChanged), shows
	// on the next page.
	mustExec(t, db, `DELETE FROM books_series_link WHERE book = (SELECT id FROM books WHERE uuid = ?)`, wheel[0])
	catalogChanged(wheel[0])
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	if currentAuthorSeriesIndex() == ix {
		t.Error("index not rebuilt after a change")
	}
	want[0].Count = 3
	if got := authorSeries([]string{"Writer"}); !slices.Equal(got, want) {
		t.Errorf("after the edit: %+v, want %+v", got, want)
	}
}
