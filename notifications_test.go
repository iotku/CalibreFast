package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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
	resolvePlaceholders(placeholdersForBook(&OPFMetadata{Title: "Other Three", Creators: []string{"Writer"}, Series: "Other", SeriesIndex: "3"}), book, nil)
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
	if len(an) != 1 || an[0].BookUUID != book || an[0].Title != "Other Three" || an[0].Authors != "Writer" || !an[0].OnDeck || an[0].Href != "/book/get/"+book {
		t.Errorf("alice's notifications %+v", an)
	}
	if len(bn) != 1 || bn[0].OnDeck {
		t.Errorf("bob's notifications %+v", bn)
	}
	// One event for both savers; On deck is each one's own.
	if len(an) == 1 && len(bn) == 1 && an[0].ID != bn[0].ID {
		t.Errorf("two events for one placeholder: %d, %d", an[0].ID, bn[0].ID)
	}
	var events int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE kind = ? AND book_uuid = ?`, notifyPlaceholderAdded, book).Scan(&events); err != nil || events != 1 {
		t.Errorf("events stored: %d (%v), want 1", events, err)
	}
	if len(notes(carol)) != 0 {
		t.Error("carol was told about a book she didn't save")
	}
	page := string(do(alice, "GET", "/me/notifications", "", http.StatusOK))
	for _, want := range []string{
		`<img class="notification-cover" src="/cover-thumb/` + book + `"`,
		`<span class="notification-head">Saved book added:</span>`,
		`<span class="notification-sub"><a href="/book/get/` + book + `" class="meta-link">Other Three</a> by Writer · On deck</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the notifications page doesn't have %s:\n%s", want, page)
		}
	}
	if page := string(do(bob, "GET", "/me/notifications", "", http.StatusOK)); !strings.Contains(page, "Other Three</a> by Writer</span>") {
		t.Error("bob's page: not On deck, or worded otherwise")
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
	do(alice, "DELETE", "/api/me/notifications/"+itoa(an[0].ID), "", http.StatusNoContent) // again: nothing to do
	// Dismissed: kept, under Earlier, with its link.
	if earlier, err := dismissedNotifications(alice.ID); err != nil || len(earlier) != 1 || earlier[0].ID != an[0].ID {
		t.Errorf("alice's history %+v (%v)", earlier, err)
	}
	page = string(do(alice, "GET", "/me/notifications", "", http.StatusOK))
	if i := strings.Index(page, "Earlier (dismissed)"); i < 0 || !strings.Contains(page[i:], `<a href="/book/get/`+book+`" class="meta-link">Other Three</a>`) {
		t.Error("the page doesn't show the dismissed one under Earlier, linked")
	}
	if n := unseen(alice); n != 0 {
		t.Errorf("a dismissed one counted: %d", n)
	}
	do(bob, "GET", "/api/me/notifications/1", "", http.StatusMethodNotAllowed)
	do(bob, "DELETE", "/api/me/notifications", "", http.StatusNoContent)
	if len(notes(bob)) != 0 {
		t.Error("not all dismissed")
	}
	if earlier, _ := dismissedNotifications(bob.ID); len(earlier) != 1 {
		t.Errorf("bob's history %+v", earlier)
	}
	// Still one event, shared by both histories.
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM notification_events`).Scan(&events); err != nil || events != 1 {
		t.Errorf("events after dismissing: %d (%v)", events, err)
	}
}

// Each user keeps at most notificationsMax, active and dismissed together,
// the oldest dismissed going first; dismissed ones go after
// notificationsKeepDismissed; an event nobody has goes too.
func TestNotificationRetention(t *testing.T) {
	setupTestAppDB(t)
	u, _ := createUser("erin", "a long enough password", false)
	v, _ := createUser("finn", "a long enough password", false)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := appDB.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	add := func(title string, users ...*User) int64 {
		t.Helper()
		id, err := addEvent(appDB, notificationEvent{Kind: notifyFavoriteAuthor, BookUUID: title, Title: title, Subject: "Someone"})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range users {
			if err := deliver(appDB, x.ID, id, false); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	// Ten shared with finn, dismissed by erin, oldest first.
	var dismissed []int64
	for i := range 10 {
		id := add("old"+itoa(int64(i)), u, v)
		mustExecApp(t, `UPDATE user_notifications SET created_at = ? WHERE event_id = ?`, 1000+i, id)
		if err := dismissNotifications(u.ID, id); err != nil {
			t.Fatal(err)
		}
		dismissed = append(dismissed, id)
	}
	// Then the rest of the 200, active.
	for i := range notificationsMax - 10 {
		id := add("new"+itoa(int64(i)), u)
		mustExecApp(t, `UPDATE user_notifications SET created_at = ? WHERE event_id = ?`, 5000+i, id)
	}
	if n := count(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ?`, u.ID); n != notificationsMax {
		t.Fatalf("rows %d, want %d", n, notificationsMax)
	}
	// Three more: the three oldest dismissed go, not active ones.
	for i := range 3 {
		add("newer"+itoa(int64(i)), u)
	}
	if n := count(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ?`, u.ID); n != notificationsMax {
		t.Errorf("rows %d after the cap, want %d", n, notificationsMax)
	}
	if n := count(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND dismissed_at IS NULL`, u.ID); n != notificationsMax-7 {
		t.Errorf("active %d, want %d", n, notificationsMax-7)
	}
	for i, id := range dismissed {
		has := count(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND event_id = ?`, u.ID, id) == 1
		if has != (i >= 3) {
			t.Errorf("dismissed #%d kept: %v", i, has)
		}
	}
	// finn still has the events erin's cap dropped.
	if n := count(`SELECT COUNT(*) FROM notification_events WHERE id = ?`, dismissed[0]); n != 1 {
		t.Error("an event finn has was collected")
	}

	// One only finn had, dismissed long ago.
	solo := add("solo", v)
	if err := dismissNotifications(v.ID, solo); err != nil {
		t.Fatal(err)
	}

	// 30 days on: the dismissed ones go; the events nobody has any more
	// are collected; the rest stay.
	mustExecApp(t, `UPDATE user_notifications SET dismissed_at = ? WHERE dismissed_at IS NOT NULL`, time.Now().Add(-notificationsKeepDismissed-time.Hour).Unix())
	mustExecApp(t, `UPDATE notification_events SET created_at = 0`)
	if err := dismissNotifications(v.ID, dismissed[0]); err != nil { // finn's, dismissed now: stays
		t.Fatal(err)
	}
	if err := sweepNotifications(time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND dismissed_at IS NOT NULL`, u.ID); n != 0 {
		t.Errorf("erin's dismissed after 30 days: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ?`, v.ID); n != 10 {
		t.Errorf("finn's %d, want 10", n)
	}
	if n := count(`SELECT COUNT(*) FROM notification_events WHERE NOT EXISTS (SELECT 1 FROM user_notifications WHERE event_id = notification_events.id)`); n != 0 {
		t.Errorf("%d events nobody has were kept", n)
	}
	if n := count(`SELECT COUNT(*) FROM notification_events WHERE id = ?`, solo); n != 0 {
		t.Error("an event nobody has was kept")
	}
	if n := count(`SELECT COUNT(*) FROM notification_events`); n != notificationsMax-7+10 {
		t.Errorf("events %d, want %d", n, notificationsMax-7+10)
	}
}

func mustExecApp(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := appDB.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

// Migration 49 moves the old notifications table's rows to events and
// users' rows, keeping their ids.
func TestNotificationsMigration(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "app.db")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i := range 48 {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, is_admin, created_at) VALUES (7, 'gail', '', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notifications (id, user_id, kind, book_uuid, title, on_deck, created_at, seen_at, subject, count)
		VALUES (42, 7, 'placeholder_added', 'u-1', 'Old Title', 1, 100, 200, '', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var kind, title string
	var onDeck bool
	var seen int64
	if err := db.QueryRow(`SELECT e.kind, e.title, un.on_deck, un.seen_at FROM user_notifications un JOIN notification_events e ON e.id = un.event_id
		WHERE un.user_id = 7 AND e.id = 42 AND un.dismissed_at IS NULL`).Scan(&kind, &title, &onDeck, &seen); err != nil {
		t.Fatal(err)
	}
	if kind != notifyPlaceholderAdded || title != "Old Title" || !onDeck || seen != 200 {
		t.Errorf("migrated: %s %q %v %d", kind, title, onDeck, seen)
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
