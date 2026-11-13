package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

func TestTagStrip(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	origGroups := groups
	groups = loadGroups(filepath.Join(t.TempDir(), groupsFileName))
	t.Cleanup(func() { groups = origGroups })

	const total = 30
	var uuids []string
	for i := range total {
		uuids = append(uuids, addTestBook(t, db, "Book "+strconv.Itoa(i), []string{"A"}, []string{"Space Opera"}, "", "epub"))
	}
	addTestBook(t, db, "Other", []string{"A"}, []string{"Cozy"}, "", "epub")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	reader, _ := createUser("reader", "a long enough password", false)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tag-strip", tagStripHandler)
	strip := func(u *User, query string, want int) tagStripAnswer {
		t.Helper()
		rec := call(t, mux, u, "GET", "/api/tag-strip?"+query, "")
		if rec.Code != want {
			t.Fatalf("%s: %d %s (want %d)", query, rec.Code, rec.Body, want)
		}
		var a tagStripAnswer
		_ = json.Unmarshal(rec.Body.Bytes(), &a)
		return a
	}

	// Walking a strip at a time from anywhere shows every book once before
	// any comes back.
	first := strip(nil, "tag=space%20opera&n=12", http.StatusOK)
	if first.Count != total || len(first.Books) != 12 || first.Next != (first.Start+12)%total {
		t.Fatalf("first strip: start %d next %d count %d, %d books", first.Start, first.Next, first.Count, len(first.Books))
	}
	seen := map[string]int{}
	at := first.Start
	for range 3 { // 36 books: the 30, then 6 again
		a := strip(nil, "tag=Space%20Opera&n=12&start="+strconv.Itoa(at), http.StatusOK)
		for _, b := range a.Books {
			seen[b.UUID]++
		}
		at = a.Next
	}
	if len(seen) != total {
		t.Fatalf("%d of %d books seen", len(seen), total)
	}
	for _, id := range uuids {
		if seen[id] == 0 {
			t.Fatalf("%s never came", id)
		}
	}

	// Starts differ between loads.
	starts := map[int]bool{}
	for range 20 {
		starts[strip(nil, "tag=Space%20Opera", http.StatusOK).Start] = true
	}
	if len(starts) < 2 {
		t.Fatal("every load started at the same place")
	}

	// Signed in, finished books are left out.
	for _, id := range uuids[:25] {
		if _, err := appDB.Exec(`INSERT INTO reading_status (user_id, book_uuid, status, source, updated_at) VALUES (?, ?, 'finished', 'manual', 1)`, reader.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	mine := strip(reader, "tag=Space%20Opera&start=0", http.StatusOK)
	if len(mine.Books) != 5 || mine.Count != total {
		t.Fatalf("signed in: %d books of %d", len(mine.Books), mine.Count)
	}
	for _, b := range mine.Books {
		for _, id := range uuids[:25] {
			if b.UUID == id {
				t.Fatalf("finished %s shown", b.Title)
			}
		}
	}

	strip(nil, "tag=Nothing", http.StatusNotFound)
	strip(nil, "tag=", http.StatusBadRequest)
}
