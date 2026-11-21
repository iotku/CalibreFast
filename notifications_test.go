package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A placeholder can be Saved; when a library book replaces it, those who
// saved it are told and have the book Saved, and On Deck when they had
// finished the book before it.
func TestSavedPlaceholderArrives(t *testing.T) {
	db, _, other := seriesFixture(t) // Other #1 and #2 in the library
	origData, origWrites := dataDir, allowWrites
	dataDir, allowWrites = t.TempDir(), true
	placeholdersChanged()
	t.Cleanup(func() { dataDir, allowWrites = origData, origWrites; placeholdersChanged() })

	admin, _ := createUser("boss", "a long enough password", true)
	alice, _ := createUser("alice", "a long enough password", false) // finished book 2
	bob, _ := createUser("bob", "a long enough password", false)     // hasn't
	carol, _ := createUser("carol", "a long enough password", false) // didn't save it
	mux := placeholderMux()
	mux.HandleFunc("PUT /api/me/books/{uuid}/status", requireUser(setStatusHandler))
	mux.HandleFunc("GET /me/{list}", requireUser(meListHandler))
	mux.HandleFunc("GET /api/me", meHandler)
	mux.HandleFunc("GET /api/me/notifications", requireUser(notificationsHandler))
	mux.HandleFunc("GET /me/notifications", requireUser(notificationsPage))
	mux.HandleFunc("POST /api/me/notifications/seen", requireUser(notificationsSeenHandler))
	mux.HandleFunc("DELETE /api/me/notifications/{id}", requireUser(notificationDeleteHandler))
	mux.HandleFunc("DELETE /api/me/notifications", requireUser(notificationsClearHandler))
	do := func(u *User, method, path, body string, want int) []byte {
		t.Helper()
		rec := call(t, mux, u, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.Bytes()
	}
	var res struct{ UUID string }
	if err := json.Unmarshal(do(admin, "POST", "/api/placeholders", `{"title":"Other Three","creators":"Writer","series":"Other","series_index":"3"}`, http.StatusOK), &res); err != nil {
		t.Fatal(err)
	}
	ph := res.UUID

	// Saving it: Saved, nothing else; it's in Saved as its card, and its
	// page says so.
	do(alice, "PUT", "/api/me/books/"+ph+"/status", `{"status":"reading"}`, http.StatusBadRequest)
	do(alice, "PUT", "/api/me/books/"+ph+"/status", `{"status":"wanted"}`, http.StatusOK)
	do(bob, "PUT", "/api/me/books/"+ph+"/status", `{"status":"wanted"}`, http.StatusOK)
	do(alice, "PUT", "/api/me/books/not-a-book/status", `{"status":"wanted"}`, http.StatusNotFound)
	var saved []Book
	if err := json.Unmarshal(do(alice, "GET", "/me/saved?api=1&page=1", "", http.StatusOK), &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].UUID != ph || saved[0].Placeholder == nil {
		t.Fatalf("Saved: %+v", saved)
	}
	if body := string(do(alice, "GET", "/missing/"+ph, "", http.StatusOK)); !strings.Contains(body, `data-placeholder-save="`+ph+`" aria-pressed="true"`) {
		t.Error("the placeholder's page doesn't show it saved")
	}
	if body := string(do(carol, "GET", "/missing/"+ph, "", http.StatusOK)); !strings.Contains(body, `aria-pressed="false"`) {
		t.Error("the placeholder's page shows it saved for someone who didn't")
	}

	if err := setStatus(alice.ID, other[0], "finished", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := setStatus(alice.ID, other[1], "finished", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := setStatus(bob.ID, other[0], "finished", "manual"); err != nil { // book 1, not 2
		t.Fatal(err)
	}
	// Alice dismissed the placeholder from On Deck; the book still comes.
	do(alice, "DELETE", "/api/me/on-deck/"+ph, "", http.StatusNoContent)

	// The book arrives.
	book := addTestBook(t, db, "Other Three", []string{"Writer"}, nil, "Other", "epub")
	mustExec(t, db, `UPDATE books SET series_index = 3 WHERE uuid = ?`, book)
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	resolvePlaceholders(&OPFMetadata{Title: "Other Three", Creators: []string{"Writer"}, Series: "Other", SeriesIndex: "3"}, book)
	if placeholderByUUID(ph) != nil {
		t.Fatal("placeholder not replaced")
	}

	for _, u := range []*User{alice, bob} {
		if s, _ := readStatus(u.ID, book); s != "wanted" {
			t.Errorf("%s: the book's status %q, want wanted", u.Username, s)
		}
		if s, _ := readStatus(u.ID, ph); s != "" {
			t.Errorf("%s: the placeholder still has status %q", u.Username, s)
		}
	}
	deck := func(u *User) []string {
		t.Helper()
		var h homeAnswer
		if err := json.Unmarshal(do(u, "GET", "/api/me/home", "", http.StatusOK), &h); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range h.OnDeck {
			out = append(out, b.UUID)
		}
		return out
	}
	if got := deck(alice); !slices.Contains(got, book) {
		t.Errorf("alice's On Deck %q lacks the book", got)
	}
	if got := deck(bob); slices.Contains(got, book) {
		t.Errorf("bob's On Deck %q has the book, though he didn't finish book 2", got)
	}

	notes := func(u *User) []notification {
		t.Helper()
		var ns []notification
		if err := json.Unmarshal(do(u, "GET", "/api/me/notifications", "", http.StatusOK), &ns); err != nil {
			t.Fatal(err)
		}
		return ns
	}
	unseen := func(u *User) int {
		t.Helper()
		var me struct{ Notifications int }
		if err := json.Unmarshal(do(u, "GET", "/api/me", "", http.StatusOK), &me); err != nil {
			t.Fatal(err)
		}
		return me.Notifications
	}
	an, bn := notes(alice), notes(bob)
	if len(an) != 1 || an[0].BookUUID != book || an[0].Title != "Other Three" || !an[0].OnDeck || an[0].Href != "/book/get/"+book {
		t.Errorf("alice's notifications %+v", an)
	}
	if len(bn) != 1 || bn[0].OnDeck {
		t.Errorf("bob's notifications %+v", bn)
	}
	if len(notes(carol)) != 0 {
		t.Error("carol was told about a book she didn't save")
	}
	if page := string(do(alice, "GET", "/me/notifications", "", http.StatusOK)); !strings.Contains(page, "Other Three") || !strings.Contains(page, "On deck") {
		t.Error("the notifications page doesn't say it")
	}

	// Seen: the bell's count goes; they're still listed until dismissed.
	if n := unseen(alice); n != 1 {
		t.Errorf("unseen %d, want 1", n)
	}
	do(alice, "GET", "/api/me/notifications/seen", "", http.StatusMethodNotAllowed)
	do(alice, "POST", "/api/me/notifications/seen", "", http.StatusNoContent)
	if n := unseen(alice); n != 0 {
		t.Errorf("unseen after seen %d", n)
	}
	if ns := notes(alice); len(ns) != 1 || !ns[0].Seen {
		t.Errorf("after seen %+v", ns)
	}
	// Only one's own can be dismissed.
	do(bob, "DELETE", "/api/me/notifications/"+itoa(an[0].ID), "", http.StatusNoContent)
	if len(notes(alice)) != 1 {
		t.Error("bob dismissed alice's notification")
	}
	do(alice, "DELETE", "/api/me/notifications/"+itoa(an[0].ID), "", http.StatusNoContent)
	if len(notes(alice)) != 0 {
		t.Error("not dismissed")
	}
	do(bob, "DELETE", "/api/me/notifications", "", http.StatusNoContent)
	if len(notes(bob)) != 0 {
		t.Error("not all dismissed")
	}
}

// Someone who gave the book a status of their own before the placeholder
// was replaced keeps it; Saved isn't put over it.
func TestSavedPlaceholderKeepsOwnStatus(t *testing.T) {
	db, _, _ := seriesFixture(t)
	origData := dataDir
	dataDir = t.TempDir()
	placeholdersChanged()
	t.Cleanup(func() { dataDir = origData; placeholdersChanged() })
	u, _ := createUser("dana", "a long enough password", false)
	p := &placeholder{Meta: OPFMetadata{Title: "Lone Book", Creators: []string{"Writer"}}}
	if err := savePlaceholder(p, nil); err != nil {
		t.Fatal(err)
	}
	if err := setStatus(u.ID, p.UUID, "wanted", "manual"); err != nil {
		t.Fatal(err)
	}
	book := addTestBook(t, db, "Lone Book", []string{"Writer"}, nil, "", "epub")
	if err := setStatus(u.ID, book, "reading", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := replacePlaceholder(p.UUID, book); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStatus(u.ID, book); s != "reading" {
		t.Errorf("status %q, want reading", s)
	}
	if s, _ := readStatus(u.ID, p.UUID); s != "" {
		t.Errorf("placeholder status %q left behind", s)
	}
	if n := unseenNotifications(u.ID); n != 1 {
		t.Errorf("notifications %d, want 1", n)
	}
	// Removed by hand (no book): nobody is told.
	q := &placeholder{Meta: OPFMetadata{Title: "Never Comes"}}
	_ = savePlaceholder(q, nil)
	_ = setStatus(u.ID, q.UUID, "wanted", "manual")
	if err := deletePlaceholder(q.UUID); err != nil {
		t.Fatal(err)
	}
	if n := unseenNotifications(u.ID); n != 1 {
		t.Errorf("notifications %d after a deletion, want 1", n)
	}
}

// Finding a placeholder's savers uses the index on reading_status's
// book_uuid (migration 40), not a scan of everyone's statuses.
func TestPlaceholderSaversIndexed(t *testing.T) {
	setupTestAppDB(t)
	var id, parent, unused int
	var detail string
	if err := appDB.QueryRow(`EXPLAIN QUERY PLAN SELECT user_id FROM reading_status WHERE book_uuid = ? AND status = 'wanted'`, "x").
		Scan(&id, &parent, &unused, &detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "reading_status_book") {
		t.Errorf("plan %q doesn't use reading_status_book", detail)
	}
}
