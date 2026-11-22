package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/opf"
)

func dbRating(t *testing.T, uuid string) int {
	t.Helper()
	var r int
	_ = calibreDB.QueryRow(`SELECT COALESCE((SELECT r.rating FROM books_ratings_link l JOIN ratings r ON r.id = l.rating
		WHERE l.book = b.id), 0) FROM books b WHERE b.uuid = ?`, uuid).Scan(&r)
	return r
}

func opfRating(t *testing.T, path string) string {
	t.Helper()
	p, err := opf.Load(filepath.Join(baseDir, path, "metadata.opf"))
	if err != nil {
		t.Fatal(err)
	}
	return p.Metadata.GetMeta("calibre:rating")
}

// The editor sets, changes and clears a rating; metadata.db, metadata.opf
// and the catalog follow, and the admin's Undo puts each back.
func TestRatingEditAndUndo(t *testing.T) {
	db := setupRecycleLibrary(t)
	orig := allowWrites
	allowWrites = true
	t.Cleanup(func() { allowWrites = orig })
	mux := http.NewServeMux()
	registerAllEditRoutes(mux)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	admin, _ := createUser("admin1", "correct horse battery", true)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, asUser(httptest.NewRequest(method, path, strings.NewReader(body)), admin))
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		return rec
	}
	apply := func(body string) {
		t.Helper()
		do("POST", "/metadata/"+a.uuid+"/apply", body)
		if err := catalogFlush(); err != nil {
			t.Fatal(err)
		}
	}

	apply(`{"title":"Alpha","creators":"Ann","rating":"8"}`)
	if dbRating(t, a.uuid) != 8 || opfRating(t, a.path) != "8" {
		t.Fatalf("set: db %d, opf %q", dbRating(t, a.uuid), opfRating(t, a.path))
	}
	// Not sent: left alone.
	apply(`{"title":"Alpha 2","creators":"Ann"}`)
	if dbRating(t, a.uuid) != 8 {
		t.Errorf("a save without a rating changed it: %d", dbRating(t, a.uuid))
	}
	// Out of range is clamped; junk clears.
	apply(`{"title":"Alpha 2","creators":"Ann","rating":"42"}`)
	if dbRating(t, a.uuid) != 10 {
		t.Errorf("clamped: %d", dbRating(t, a.uuid))
	}
	apply(`{"title":"Alpha 2","creators":"Ann","rating":""}`)
	if dbRating(t, a.uuid) != 0 || opfRating(t, a.path) != "" {
		t.Errorf("cleared: db %d, opf %q", dbRating(t, a.uuid), opfRating(t, a.path))
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM books_ratings_link`).Scan(&n)
	if n != 0 {
		t.Errorf("%d rating links left", n)
	}

	// The editor's current metadata carries the rating.
	apply(`{"title":"Alpha 2","creators":"Ann","rating":"7"}`)
	var metas []UploadedBookMeta
	_ = json.Unmarshal(do("GET", "/metadata/"+a.uuid, "").Body.Bytes(), &metas)
	if len(metas) == 0 || metas[0].Format != "current" || metas[0].Rating != 7 {
		t.Errorf("editor's current: %+v", metas)
	}

	// Undo the last edit (7 → back to none).
	var list struct{ Entries []editLogEntry }
	_ = json.Unmarshal(do("GET", "/api/admin/activity", "").Body.Bytes(), &list)
	body, _ := json.Marshal(map[string]any{"ids": []int64{list.Entries[0].ID}})
	var res struct{ Results []revertResult }
	_ = json.Unmarshal(do("POST", "/api/admin/activity/revert", string(body)).Body.Bytes(), &res)
	if len(res.Results) != 1 || !res.Results[0].Done {
		t.Fatalf("undo: %+v", res.Results)
	}
	if dbRating(t, a.uuid) != 0 {
		t.Errorf("after undo: %d", dbRating(t, a.uuid))
	}

	// An entry logged before ratings were kept (no "rating" in it) still
	// undoes, and leaves the rating as it is.
	apply(`{"title":"Alpha 3","creators":"Ann","rating":"6"}`)
	_ = json.Unmarshal(do("GET", "/api/admin/activity", "").Body.Bytes(), &list)
	e := list.Entries[0]
	strip := func(raw json.RawMessage) string {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		delete(m, "rating")
		b, _ := json.Marshal(m)
		return string(b)
	}
	// Stored as the log stores it: re-marshal through bookSnap so key order matches.
	var before, after bookSnap
	_ = json.Unmarshal([]byte(strip(e.Before)), &before)
	_ = json.Unmarshal([]byte(strip(e.After)), &after)
	mustExec(t, appDB, `UPDATE edit_log SET before = ?, after = ? WHERE id = ?`, string(mustJSONBytes(before)), string(mustJSONBytes(after)), e.ID)
	body, _ = json.Marshal(map[string]any{"ids": []int64{e.ID}})
	_ = json.Unmarshal(do("POST", "/api/admin/activity/revert", string(body)).Body.Bytes(), &res)
	if len(res.Results) != 1 || !res.Results[0].Done {
		t.Fatalf("undo of an old entry: %+v", res.Results)
	}
	if got := snapBook(a.uuid); got.Title != "Alpha 2" || dbRating(t, a.uuid) != 6 {
		t.Errorf("old entry undone: title %q, rating %d", got.Title, dbRating(t, a.uuid))
	}
}

func TestRatingBulkEdit(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bob"}, nil, nil)
	five := "10"
	res := bulkEditBooks([]string{a.uuid, b.uuid}, &bulkEdit{Rating: &five})
	if len(res.Failed) != 0 || dbRating(t, a.uuid) != 10 || dbRating(t, b.uuid) != 10 {
		t.Fatalf("bulk: %+v, ratings %d %d", res, dbRating(t, a.uuid), dbRating(t, b.uuid))
	}
	none := ""
	bulkEditBooks([]string{a.uuid}, &bulkEdit{Rating: &none})
	if dbRating(t, a.uuid) != 0 || dbRating(t, b.uuid) != 10 {
		t.Errorf("cleared one: %d %d", dbRating(t, a.uuid), dbRating(t, b.uuid))
	}
	// A bulk edit of something else leaves ratings alone.
	pub := "New Pub"
	bulkEditBooks([]string{b.uuid}, &bulkEdit{Publisher: &pub})
	if dbRating(t, b.uuid) != 10 {
		t.Errorf("publisher edit changed the rating: %d", dbRating(t, b.uuid))
	}
}

// A book added with calibre:rating in its OPF keeps it.
func TestRatingOnInsert(t *testing.T) {
	db := catalogFixture(t)
	meta := &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Rated", Creators: []string{"Ann"},
		Meta: []opf.Meta{{Name: "calibre:rating", Content: "9.0"}}}}
	_, uuid, _, err := insertBookIntoCalibreDB(db, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dbRating(t, uuid) != 9 {
		t.Errorf("rating %d", dbRating(t, uuid))
	}
}

func setRating(t *testing.T, uuid string, r int) {
	t.Helper()
	tx, err := calibreDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM books WHERE uuid = ?`, uuid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := setBookRating(tx, id, r); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRatingSortSearchAndPopup(t *testing.T) {
	ids := sortFixture(t)
	setRating(t, ids["apple"], 10)
	setRating(t, ids["mango"], 7)
	setRating(t, ids["zebra"], 8)
	c, err := buildCatalog(calibreDB)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
	checkCatalogMatchesDB(t, calibreDB, c)

	order := latestOrder(c, sortByRating)
	var uuids []string
	for _, s := range order {
		uuids = append(uuids, c.books[s].UUID)
	}
	if got := names(ids, uuids); !slices.Equal(got, []string{"apple", "zebra", "mango", "nodate", "noauth"}) {
		t.Errorf("catalog rating sort: %v", got)
	}

	// The same order from SQL, on a tag page.
	mustExec(t, calibreDB, `INSERT INTO tags (name) VALUES ('Fruit')`)
	mustExec(t, calibreDB, `INSERT INTO books_tags_link (book, tag) SELECT id, (SELECT id FROM tags WHERE name = 'Fruit') FROM books`)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("/tag/", filteredPageHandler(categoryByKey("tag")))
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/tag/Fruit?api=1&sort=rating", nil))
	var books []Book
	_ = json.Unmarshal(rec.Body.Bytes(), &books)
	uuids = nil
	for _, b := range books {
		uuids = append(uuids, b.UUID)
	}
	if got := names(ids, uuids); !slices.Equal(got, []string{"apple", "zebra", "mango", "nodate", "noauth"}) {
		t.Errorf("SQL rating sort: %v", got)
	}

	for q, want := range map[string][]string{
		"rating:4":       {"apple", "zebra"},
		"rating:3.5":     {"apple", "zebra", "mango"},
		"rating:=5":      {"apple"},
		"rating:=3.5":    {"mango"},
		"rating:0":       {"nodate", "noauth"},
		"stars:5":        {"apple"},
		"rating:six":     nil,
		"rating:4 zebra": {"zebra"},
	} {
		res, _ := runSearchNames(t, ids, q)
		slices.Sort(res)
		w := slices.Clone(want)
		slices.Sort(w)
		if !slices.Equal(res, w) {
			t.Errorf("search %q: %v, want %v", q, res, w)
		}
	}

	for r, want := range map[int]string{0: "", 1: "Rated 0.5 out of 5", 7: "Rated 3.5 out of 5", 10: "Rated 5 out of 5"} {
		s := starsFor(r)
		if (s == nil) != (want == "") || (s != nil && (s.Text != want || len(s.Stars) != 5)) {
			t.Errorf("starsFor(%d) = %+v", r, s)
		}
	}
	if s := starsFor(7); strings.Join(s.Stars, ",") != "full,full,full,half,empty" {
		t.Errorf("3.5 stars: %v", s.Stars)
	}
}

func runSearchNames(t *testing.T, ids map[string]string, q string) ([]string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	searchHandler(rec, httptest.NewRequest("GET", "/api/search?q="+strings.ReplaceAll(q, " ", "+"), nil))
	var books []Book
	if err := json.Unmarshal(rec.Body.Bytes(), &books); err != nil {
		return nil, err
	}
	var u []string
	for _, b := range books {
		u = append(u, b.UUID)
	}
	return names(ids, u), nil
}
