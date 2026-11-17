package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// seriesShelfFixture: a library with the series "Saga" (three books, given
// out of order) and one book on its own, in the catalog; and a user.
func seriesShelfFixture(t *testing.T) (u *User, saga []string, single string) {
	t.Helper()
	db := catalogFixture(t)
	setupTestAppDB(t)
	// Book 2, then 1, then 0.5: the card's cover is the 0.5's; two at 1 go by date.
	for _, b := range []struct {
		title, pubdate string
		index          float64
	}{{"Saga Two", "2002-01-01", 2}, {"Saga One B", "2001-06-01", 1}, {"Saga One A", "2001-01-01", 1}, {"Saga Prequel", "2005-01-01", 0.5}} {
		id := addTestBook(t, db, b.title, []string{"Writer"}, nil, "Saga")
		mustExec(t, db, `UPDATE books SET series_index = ?, pubdate = ? WHERE uuid = ?`, b.index, b.pubdate, id)
		saga = append(saga, id)
	}
	single = addTestBook(t, db, "Standalone", []string{"Writer"}, nil, "")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	u, err = createUser("reader", "longenough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	return u, saga, single
}

func shelfSeriesMux() *http.ServeMux {
	mux := listOrderMux()
	mux.HandleFunc("PUT /api/shelves/{id}/series/{name...}", requireUser(shelfAddSeriesHandler))
	mux.HandleFunc("DELETE /api/shelves/{id}/series/{name...}", requireUser(shelfRemoveSeriesHandler))
	return mux
}

func TestShelfSeries(t *testing.T) {
	alice, saga, single := seriesShelfFixture(t)
	bob, _ := createUser("bob", "longenough-pw", false)
	mux := shelfSeriesMux()

	var s Shelf
	_ = json.Unmarshal(call(t, mux, alice, "POST", "/api/shelves", `{"name":"Mix"}`).Body.Bytes(), &s)
	base := "/api/shelves/" + itoa(s.ID)
	if c := call(t, mux, alice, "PUT", base+"/books/"+single, "").Code; c != http.StatusNoContent {
		t.Fatalf("add book: %d", c)
	}
	// Any spelling of the series' name; stored as the library has it.
	if rec := call(t, mux, alice, "PUT", base+"/series/saga", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add series: %d %s", rec.Code, rec.Body)
	}
	if c := call(t, mux, alice, "PUT", base+"/series/No%20such%20series", "").Code; c != http.StatusNotFound {
		t.Fatalf("unknown series: %d", c)
	}
	if c := call(t, mux, bob, "PUT", base+"/series/Saga", "").Code; c != http.StatusNotFound {
		t.Fatalf("someone else's shelf: %d", c)
	}
	if c := call(t, mux, alice, "PUT", base+"/books/series:Saga", "").Code; c != http.StatusNotFound {
		t.Fatalf("a series key isn't a book: %d", c)
	}

	// The shelf: the book, then the series as one card.
	var shown []Book
	_ = json.Unmarshal(call(t, mux, alice, "GET", "/shelf/"+itoa(s.ID)+"?api=1&page=1", "").Body.Bytes(), &shown)
	if len(shown) != 2 || shown[0].UUID != single || shown[1].UUID != "series:Saga" {
		t.Fatalf("shelf entries: %+v", shown)
	}
	card := shown[1].SeriesCard
	if card == nil || card.Name != "Saga" || card.Count != 4 || card.Cover != saga[3] || card.Href != "/series/Saga" {
		t.Fatalf("series card: %+v", card)
	}
	if got, _ := getShelf(s.ID); got.Count != 2 || got.Series != 1 || got.BookCount() != 1 {
		t.Fatalf("counts: %+v", got)
	}

	// Arranged like a book.
	if rec := call(t, mux, alice, "PUT", base+"/order", orderBody("series:Saga", single)); rec.Code != http.StatusNoContent {
		t.Fatalf("arrange: %d %s", rec.Code, rec.Body)
	}
	if got := listUUIDs(t, mux, alice, "/shelf/"+itoa(s.ID)); !slices.Equal(got, []string{"series:Saga", single}) {
		t.Fatalf("arranged: %v", got)
	}

	// The home page's rows have it too.
	rows := []homeShelf{{Kind: "shelf", ID: s.ID}}
	if err := fillShelfBooks(rows); err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Books) != 2 || rows[0].Books[0].SeriesCard == nil {
		t.Fatalf("home row: %+v", rows[0].Books)
	}

	// The popup and the series page offer it, checked.
	if m := seriesShelfMarks(alice, "Saga"); len(m) != 1 || !m[0].Has {
		t.Fatalf("series marks: %+v", m)
	}
	if m := seriesShelfMarks(bob, "Saga"); len(m) != 0 {
		t.Fatalf("bob's marks: %+v", m)
	}

	// A series gone from the library is skipped, and can still be taken off.
	_, _ = appDB.Exec(`INSERT INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, 'series:Gone', 0)`, s.ID)
	if got := listUUIDs(t, mux, alice, "/shelf/"+itoa(s.ID)); len(got) != 2 {
		t.Fatalf("gone series shown: %v", got)
	}
	if c := call(t, mux, alice, "DELETE", base+"/series/Gone", "").Code; c != http.StatusNoContent {
		t.Fatalf("remove gone series: %d", c)
	}
	if c := call(t, mux, alice, "DELETE", base+"/series/saga", "").Code; c != http.StatusNoContent {
		t.Fatalf("remove series: %d", c)
	}
	if got := listUUIDs(t, mux, alice, "/shelf/"+itoa(s.ID)); !slices.Equal(got, []string{single}) {
		t.Fatalf("after removing: %v", got)
	}
}

// A series with a slash and other oddities in its name, through the URL.
func TestShelfSeriesNameInPath(t *testing.T) {
	alice, _, _ := seriesShelfFixture(t)
	_, _ = calibreDB.Exec(`UPDATE series SET name = 'A/B & C?' WHERE name = 'Saga'`)
	c, err := buildCatalog(calibreDB)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	mux := shelfSeriesMux()
	var s Shelf
	_ = json.Unmarshal(call(t, mux, alice, "POST", "/api/shelves", `{"name":"Odd"}`).Body.Bytes(), &s)
	path := "/api/shelves/" + itoa(s.ID) + "/series/" + url.PathEscape("A/B & C?")
	if rec := call(t, mux, alice, "PUT", path, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if got := listUUIDs(t, mux, alice, "/shelf/"+itoa(s.ID)); !slices.Equal(got, []string{"series:A/B & C?"}) {
		t.Fatalf("shelf: %v", got)
	}
}

// Signed-out visitors' featured shelves (featured.json) show the card too.
func TestFeaturedShelfSeries(t *testing.T) {
	seriesShelfFixture(t)
	res, err := appDB.Exec(`INSERT INTO shelves (owner_id, name, description, created_at, updated_at, featured_pos) VALUES (NULL, 'Picks', '', 0, 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	_, _ = appDB.Exec(`INSERT INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, 'series:Saga', 0)`, id)
	shelves, err := featuredShelves()
	if err != nil {
		t.Fatal(err)
	}
	if len(shelves) != 1 || len(shelves[0].Books) != 1 || shelves[0].Books[0].SeriesCard == nil {
		t.Fatalf("featured: %+v", shelves)
	}
	data, _ := json.Marshal(shelves)
	if !strings.Contains(string(data), `"series_card":{"name":"Saga"`) {
		t.Fatalf("featured JSON: %s", data)
	}
}

// The book popup offers the book's series; the series page its panel.
func TestSeriesShelfEntryPoints(t *testing.T) {
	alice, saga, single := seriesShelfFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /book/me/{uuid}", bookUserActionsHandler)
	mux.HandleFunc("GET /series/", filteredPageHandler(categoryByKey("series")))
	if body := call(t, mux, alice, "GET", "/book/me/"+saga[0], "").Body.String(); !strings.Contains(body, `data-series="Saga"`) || !strings.Contains(body, "Add series to shelf") {
		t.Fatalf("popup without the series: %s", body)
	}
	if body := call(t, mux, alice, "GET", "/book/me/"+single, "").Body.String(); strings.Contains(body, "Add series to shelf") {
		t.Fatal("popup offers a series for a book without one")
	}
	body := call(t, mux, alice, "GET", "/series/Saga", "").Body.String()
	if !strings.Contains(body, `id="series-shelf-panel"`) || !strings.Contains(body, "series-shelf-toggle") {
		t.Fatalf("series page without Add to shelf: %s", body)
	}
	if body := call(t, mux, nil, "GET", "/series/Saga", "").Body.String(); strings.Contains(body, "series-shelf-panel") {
		t.Fatal("signed-out series page offers shelves")
	}
}
