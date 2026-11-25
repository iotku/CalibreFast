package main

import (
	"encoding/json"
	"testing"
)

// shelfShown: a shelf's count (getShelf, the shelves list, a Discover row)
// and the books its page lists, which must agree.
func shelfShown(t *testing.T, u *User, id int64) (count, listed int) {
	t.Helper()
	s, err := getShelf(id)
	if err != nil {
		t.Fatal(err)
	}
	mine, _, err := listShelves(u)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mine {
		if m.ID == id && m.Count != s.Count {
			t.Errorf("shelves list says %d, getShelf %d", m.Count, s.Count)
		}
	}
	row := []homeShelf{{ID: id}}
	if err := fillShelfBooks(row); err != nil {
		t.Fatal(err)
	}
	if row[0].Count != s.Count {
		t.Errorf("Discover row says %d, getShelf %d", row[0].Count, s.Count)
	}
	var shown []Book
	rec := call(t, shelfMux(), u, "GET", "/shelf/"+itoa(id)+"?api=1&page=1", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &shown); err != nil {
		t.Fatalf("shelf page: %v %s", err, rec.Body)
	}
	return s.Count, len(shown)
}

// Books that left the library (recycled, merged into another, deleted in
// Calibre) keep their shelf rows, for a restore, but aren't counted.
func TestShelfCountsOnlyBooksInLibrary(t *testing.T) {
	db := setupRecycleLibrary(t)
	a := addOPFTestBook(t, db, "Alpha", []string{"Ann"}, nil, nil)
	b := addOPFTestBook(t, db, "Beta", []string{"Ann"}, nil, nil)
	c := addOPFTestBook(t, db, "Gamma", []string{"Cy"}, nil, nil)
	u, err := createUser("reader", "longenough-pw", false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := appDB.Exec(`INSERT INTO shelves (owner_id, name, created_at, updated_at) VALUES (?, 'S', 0, 0)`, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, uuid := range []string{a.uuid, b.uuid, c.uuid, "deleted-in-calibre"} {
		if _, err := appDB.Exec(`INSERT INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, ?, ?)`, id, uuid, i); err != nil {
			t.Fatal(err)
		}
	}
	if n, listed := shelfShown(t, u, id); n != 3 || listed != 3 {
		t.Fatalf("before: count %d, listed %d; want 3", n, listed)
	}

	// Recycled: gone from the count, back when restored.
	batch, err := recycleBooks([]string{c.uuid}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if n, listed := shelfShown(t, u, id); n != 2 || listed != 2 {
		t.Fatalf("after recycling: count %d, listed %d; want 2", n, listed)
	}
	if _, err := restoreRecycled(batch.ID, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if n, listed := shelfShown(t, u, id); n != 3 || listed != 3 {
		t.Fatalf("after restoring: count %d, listed %d; want 3", n, listed)
	}

	// Merged: the kept book stays on the shelf, once.
	if _, err := mergeBooks(&mergeRequest{
		ApplyMetadataRequest: ApplyMetadataRequest{Title: "Alpha", Creators: "Ann"},
		Keep:                 a.uuid,
		UUIDs:                []string{a.uuid, b.uuid},
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	if n, listed := shelfShown(t, u, id); n != 2 || listed != 2 {
		t.Fatalf("after merging: count %d, listed %d; want 2", n, listed)
	}
	if !shelfHas(t, id, a.uuid) {
		t.Error("kept book left the shelf")
	}
}

func shelfHas(t *testing.T, id int64, uuid string) bool {
	t.Helper()
	m, err := liveShelfEntries([]int64{id})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m[id] {
		if b.UUID == uuid {
			return true
		}
	}
	return false
}

// A page is cut from the books shown, so rows for removed books don't
// leave it short.
func TestShelfPageSkipsRemovedBooks(t *testing.T) {
	u, books := setupLibraryAndUser(t, booksPerPage)
	res, err := appDB.Exec(`INSERT INTO shelves (owner_id, name, created_at, updated_at) VALUES (?, 'S', 0, 0)`, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, uuid := range books {
		// A removed book before each one in the shelf's order.
		for _, k := range []string{"gone-" + itoa(int64(i)), uuid} {
			if _, err := appDB.Exec(`INSERT INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, ?, ?)`, id, k, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, listed := shelfShown(t, u, id); n != booksPerPage || listed != booksPerPage {
		t.Fatalf("count %d, page 1 lists %d; want %d", n, listed, booksPerPage)
	}
}
