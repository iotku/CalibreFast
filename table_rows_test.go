package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"testing"
)

// useCatalog builds the catalog of the test library and makes it current.
func useCatalog(t *testing.T) {
	t.Helper()
	c, err := buildCatalog(calibreDB)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)
}

func getTable(t *testing.T, query string, gz bool) (tableResponse, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/library/table"+query, nil)
	if gz {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	libraryTableHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
	}
	var body io.Reader = rec.Body
	if rec.Header().Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = zr
	}
	var res tableResponse
	if err := json.NewDecoder(body).Decode(&res); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return res, rec
}

func tableUUIDs(books []tableBook) []string {
	var out []string
	for _, b := range books {
		out = append(out, b.UUID)
	}
	return out
}

// The catalog's rows are metadata.db's (tableBooks), but for the ISBN's
// form (the catalog keeps it normalized).
func TestCatalogTableRowsMatchDB(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann", "Bo"}, []string{"t1", "t2"}, []string{"eng", "fra"})
	addOPFTestBook(t, db, "Beta", []string{"Ashton, Hugh"}, nil, nil)
	c := addOPFTestBook(t, db, "Gamma", []string{"Cy"}, []string{"t2"}, nil)
	series, index := "Saga", "2.5"
	if res := bulkEditBooks([]string{a.uuid}, &bulkEdit{Series: &series, SeriesIndex: &index}); res.Changed != 1 {
		t.Fatalf("series: %+v", res)
	}
	if _, err := db.Exec(`DELETE FROM data WHERE book = ?`, c.id); err != nil {
		t.Fatal(err)
	}
	formatCache.Delete(c.uuid)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	useCatalog(t)

	want, err := tableBooks(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := getTable(t, "", false)
	if got.Total != 3 || len(got.Books) != 3 || got.Version == "" {
		t.Fatalf("table: total %d, %d books, version %q", got.Total, len(got.Books), got.Version)
	}
	byUUID := map[string]tableBook{}
	for _, b := range got.Books {
		byUUID[b.UUID] = b
	}
	for _, w := range want {
		g := byUUID[w.UUID]
		g.ISBN, w.ISBN = "", ""
		w.path, w.modified = "", ""
		slices.Sort(g.Tags)
		slices.Sort(w.Tags)
		gj, _ := json.Marshal(g)
		wj, _ := json.Marshal(w)
		if string(gj) != string(wj) {
			t.Errorf("row of %s:\n got %s\nwant %s", w.Title, gj, wj)
		}
	}
	if r := byUUID[c.uuid]; !r.FormatsFromFolder || !slices.Equal(r.Formats, []string{"epub"}) {
		t.Errorf("unlisted book: %+v", r)
	}
	if r := byUUID[a.uuid]; r.Series != "Saga" || r.SeriesIndex != 2.5 || !slices.Equal(r.Languages, []string{"eng", "fra"}) {
		t.Errorf("Alpha: %+v", r)
	}
}

// The first rows come in the sort asked for; the pages after them cover
// every book once, however the library changes in between.
func TestTablePages(t *testing.T) {
	db := setupRecycleLibrary(t)
	var all []string
	for _, title := range []string{"Delta", "alpha", "Charlie", "Écho", "Bravo"} {
		all = append(all, addOPFTestBook(t, db, title, []string{"Ann"}, nil, nil).uuid)
	}
	useCatalog(t)

	first, _ := getTable(t, "?sort=title&limit=3", false)
	var titles []string
	for _, b := range first.Books {
		titles = append(titles, b.Title)
	}
	if !slices.Equal(titles, []string{"alpha", "Bravo", "Charlie"}) || first.Total != 5 || first.NotDuplicates == nil {
		t.Errorf("sort=title: %v (total %d)", titles, first.Total)
	}
	first, _ = getTable(t, "?sort=-title&limit=2", false)
	if first.Books[0].Title != "Écho" || first.Books[1].Title != "Delta" {
		t.Errorf("sort=-title: %v", tableUUIDs(first.Books))
	}
	if _, rec := getTable(t, "?sort=nope&limit=1", false); rec.Code != 200 {
		t.Errorf("unknown sort: %d", rec.Code)
	}

	var seen []string
	from := "0"
	for range 10 {
		p, _ := getTable(t, "?limit=2&from="+from, false)
		seen = append(seen, tableUUIDs(p.Books)...)
		if p.Next == 0 {
			break
		}
		from = itoa(int64(p.Next))
		if len(seen) == 2 {
			// A book renamed between pages moves in no sort that matters here.
			title := "Zulu"
			bulkEditBooks([]string{all[3]}, &bulkEdit{Title: &title})
			if err := catalogFlush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, want := slices.Sorted(slices.Values(seen)), slices.Sorted(slices.Values(all))
	if !slices.Equal(got, want) {
		t.Errorf("pages: %v, want each of %v once", seen, all)
	}

	for _, q := range []string{"?limit=0", "?from=-1&limit=2", "?from=99999999999&limit=2"} {
		rec := httptest.NewRecorder()
		libraryTableHandler(rec, httptest.NewRequest("GET", "/api/library/table"+q, nil))
		if rec.Code != 400 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
	if p, _ := getTable(t, "?from=0&limit=99999999999", false); len(p.Books) != 5 || p.Next != 0 {
		t.Errorf("huge limit: %d books, next %d", len(p.Books), p.Next)
	}
}

// ?have= answers "unchanged" until the rows would differ.
func TestTableVersion(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Bo"}, nil, nil)
	useCatalog(t)

	v := func() string { t.Helper(); r, _ := getTable(t, "?limit=1", false); return r.Version }
	unchanged := func(version string) bool {
		t.Helper()
		r, _ := getTable(t, "?limit=1&have="+version, false)
		if r.Unchanged && len(r.Books) != 0 {
			t.Errorf("unchanged, with %d rows", len(r.Books))
		}
		return r.Unchanged
	}
	v0 := v()
	if !unchanged(v0) {
		t.Fatal("same version: not unchanged")
	}

	// A queued edit (its row shows at once), then the catalog after it's made.
	title := "Alpha 2"
	if _, err := queueBulkEdit([]string{a.uuid}, &bulkEdit{Title: &title}, &User{Username: "admin", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if unchanged(v0) {
		t.Error("after a queued edit: unchanged")
	}
	waitBulkEdits()
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	v1 := v()
	if v1 == v0 || !unchanged(v1) {
		t.Errorf("after the edit was made: %q (was %q)", v1, v0)
	}

	// Books marked not duplicates.
	if _, err := markNotDuplicates([]string{a.uuid, b.uuid}, "admin"); err != nil {
		t.Fatal(err)
	}
	if unchanged(v1) {
		t.Error("after marking not duplicates: unchanged")
	}
}

func TestTableGzip(t *testing.T) {
	db := setupRecycleLibrary(t)
	addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	useCatalog(t)
	res, rec := getTable(t, "", true)
	if rec.Header().Get("Content-Encoding") != "gzip" || len(res.Books) != 1 {
		t.Errorf("gzip: %q, %d books", rec.Header().Get("Content-Encoding"), len(res.Books))
	}
	if _, rec := getTable(t, "", false); rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzipped for a browser that didn't ask")
	}
}
