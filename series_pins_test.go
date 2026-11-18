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
	if !validLayout(homeLayout{Order: []string{"k:ondeck", "r:Other"}, Hidden: []string{"ondeck"}}) {
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

func TestOnDeckSaved(t *testing.T) {
	db, wheel, _ := seriesFixture(t)
	var saved []string
	for i := range 5 {
		saved = append(saved, addTestBook(t, db, "Saved "+strconv.Itoa(i), []string{"Writer"}, nil, "", "epub"))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	alice, _ := createUser("alice", "a long enough password", false)
	mux := seriesMux()
	status := func(uuid, s string, at int64) {
		t.Helper()
		if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, ?, 'manual', ?)`, alice.ID, uuid, s, at); err != nil {
			t.Fatal(err)
		}
	}
	deck := func() []string {
		t.Helper()
		rec := call(t, mux, alice, "GET", "/api/me/home", "")
		var h homeAnswer
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range h.OnDeck {
			out = append(out, b.UUID)
		}
		return out
	}
	status(wheel[0], "finished", 1)
	// The series' next book is saved too (first, saved longest ago): it stays
	// where the series puts it and doesn't count among the Saved ones.
	status(wheel[1], "wanted", 5)
	for i, u := range saved {
		status(u, "wanted", int64(10+i))
	}
	if got, want := deck(), []string{wheel[1], saved[0], saved[1], saved[2]}; !slices.Equal(got, want) {
		t.Fatalf("on deck %q, want %q (series first, then the oldest %d Saved)", got, want, onDeckSavedMax)
	}
	if got, _ := wantedInOrder(alice); !slices.Equal(got, append([]string{wheel[1]}, saved...)) {
		t.Fatalf("Saved %q, want oldest first", got)
	}
	// ✕ on a Saved card: the next Saved book takes its place; still Saved.
	if rec := call(t, mux, alice, "DELETE", "/api/me/on-deck/"+saved[0], ""); rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Body)
	}
	if got, want := deck(), []string{wheel[1], saved[1], saved[2], saved[3]}; !slices.Equal(got, want) {
		t.Fatalf("after dismiss %q, want %q", got, want)
	}
	if got, _ := wantedInOrder(alice); !slices.Contains(got, saved[0]) {
		t.Fatal("dismissed from On Deck dropped it from Saved")
	}
}
