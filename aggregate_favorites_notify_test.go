package main

import "testing"

func TestFavoriteNotifications(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	addTestBook(t, db, "Seed", []string{"Liked"}, nil, "Saga", "epub")
	old := addTestBook(t, db, "Old", []string{"Other"}, nil, "", "epub")
	c, err := buildCatalog(db)
	if err != nil {
		t.Fatal(err)
	}
	libraryCatalog.Store(c)

	alice, _ := createUser("alice", "a long enough password", false)
	bob, _ := createUser("bob", "a long enough password", false)
	for _, f := range [][2]string{{"author", "Liked"}, {"series", "Saga"}} {
		if _, err := appDB.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, created_at) VALUES (?, ?, ?, 1)`, alice.ID, f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	flush := func(uuids ...string) {
		t.Helper()
		catalogChanged(uuids...)
		if err := catalogFlush(); err != nil {
			t.Fatal(err)
		}
		favNotifyWG.Wait()
	}
	seen := 0
	news := func(u *User) []notification { // the ones since last asked, newest first
		t.Helper()
		ns, err := userNotifications(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if u != alice {
			return ns
		}
		fresh := ns[:len(ns)-seen]
		seen = len(ns)
		return fresh
	}

	// An upload by the starred author: one notification, to the book.
	one := addTestBook(t, db, "New One", []string{"Liked"}, nil, "", "epub")
	flush(one)
	if n := news(alice); len(n) != 1 || n[0].Kind != notifyFavoriteAuthor || n[0].Subject != "Liked" || n[0].Count != 1 ||
		n[0].Title != "New One" || n[0].Href != "/book/get/"+one {
		t.Fatalf("one upload: %+v", n)
	}

	// Three in one batch: one notification, to the author's page.
	var three []string
	for _, title := range []string{"A", "B", "C"} {
		three = append(three, addTestBook(t, db, title, []string{"Liked"}, nil, "", "epub"))
	}
	flush(three...)
	if n := news(alice); len(n) != 1 || n[0].Count != 3 || n[0].Href != "/author/Liked" {
		t.Fatalf("three uploads: %+v", n)
	}

	// By the starred author, in the starred series: told once, about the series.
	saga := addTestBook(t, db, "Saga Two", []string{"Liked"}, nil, "Saga", "epub")
	flush(saga)
	if n := news(alice); len(n) != 1 || n[0].Kind != notifyFavoriteSeries || n[0].Subject != "Saga" {
		t.Fatalf("series: %+v", n)
	}

	// An existing book edited to have the author: told; a later title edit isn't news.
	mustExec(t, db, `INSERT INTO books_authors_link (book, author) SELECT b.id, a.id FROM books b, authors a WHERE b.uuid = ? AND a.name = 'Liked'`, old)
	flush(old)
	if n := news(alice); len(n) != 1 || n[0].Title != "Old" || n[0].Kind != notifyFavoriteAuthor {
		t.Fatalf("author added: %+v", n)
	}
	mustExec(t, db, `UPDATE books SET title = 'Old, Renamed' WHERE uuid = ?`, old)
	flush(old)
	if n := news(alice); len(n) != 0 {
		t.Fatalf("title edit: %+v", n)
	}

	// Removed, then restored: not news.
	catalogRemoved(one)
	if err := catalogFlush(); err != nil {
		t.Fatal(err)
	}
	flush(one)
	if n := news(alice); len(n) != 0 {
		t.Fatalf("restored: %+v", n)
	}

	// A book the user was already told about (a saved placeholder it
	// replaced), even if they dismissed that: not again.
	placed := addTestBook(t, db, "Was Saved", []string{"Liked"}, nil, "", "epub")
	ev, err := addEvent(appDB, notificationEvent{Kind: notifyPlaceholderAdded, BookUUID: placed, Title: "Was Saved"})
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(appDB, alice.ID, ev, false); err != nil {
		t.Fatal(err)
	}
	if err := dismissNotifications(alice.ID, ev); err != nil {
		t.Fatal(err)
	}
	flush(placed)
	if n := news(alice); len(n) != 0 {
		t.Fatalf("already told: %+v", n)
	}

	// Two fans of the author told of the same book share one event.
	if _, err := appDB.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, created_at) VALUES (?, 'author', 'Liked', 1)`, bob.ID); err != nil {
		t.Fatal(err)
	}
	both := addTestBook(t, db, "For Both", []string{"Liked"}, nil, "", "epub")
	flush(both)
	an, bn := news(alice), news(bob)
	if len(an) != 1 || len(bn) != 1 || an[0].ID != bn[0].ID {
		t.Fatalf("shared: alice %+v, bob %+v", an, bn)
	}
	var events int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE book_uuid = ?`, both).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events for one book: %d (%v)", events, err)
	}
	if _, err := appDB.Exec(`DELETE FROM aggregate_favorites WHERE user_id = ?`, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := dismissNotifications(bob.ID, 0); err != nil {
		t.Fatal(err)
	}

	if n := news(bob); len(n) != 0 {
		t.Fatalf("bob starred nothing: %+v", n)
	}
}
