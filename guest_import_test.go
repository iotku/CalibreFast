package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type importFixture struct {
	t     *testing.T
	u     *User
	books []string
}

func newImportFixture(t *testing.T, n int) *importFixture {
	t.Helper()
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	f := &importFixture{t: t}
	for i := range n {
		f.books = append(f.books, addTestBook(t, db, "Book "+string(rune('A'+i)), []string{"A"}, nil, "", "epub"))
	}
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.u, err = createUser("reader", "a long enough password", false); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *importFixture) post(h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	data, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h(rec, asUser(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)), f.u))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	return rec
}

func (f *importFixture) preview(books ...guestBook) map[string]importPreview {
	f.t.Helper()
	var out struct{ Books []importPreview }
	if err := json.Unmarshal(f.post(guestImportPreviewHandler, "/api/me/import/preview", map[string]any{"books": books}).Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	m := map[string]importPreview{}
	for _, p := range out.Books {
		m[p.UUID] = p
	}
	return m
}

type applyBook struct {
	guestBook
	UsePosition bool `json:"use_position"`
}

func (f *importFixture) apply(books ...applyBook) {
	f.t.Helper()
	var out struct{ Results []importResult }
	if err := json.Unmarshal(f.post(guestImportHandler, "/api/me/import", map[string]any{"books": books}).Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	for _, r := range out.Results {
		if r.Error != "" {
			f.t.Errorf("import %s: %s", r.UUID, r.Error)
		}
	}
}

func (f *importFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := appDB.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *importFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := appDB.QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestGuestImportPreview(t *testing.T) {
	f := newImportFixture(t, 4)
	now := time.Now().UnixMilli()
	fresh, same, differs, kobo := f.books[0], f.books[1], f.books[2], f.books[3]

	// same: the account has this position and highlight already (an older
	// copy of its own); differs: the account read elsewhere in it;
	// kobo: the account's newest is from a KOReader device.
	f.exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'epub', 'cfi-same', 0.3, ?)`, f.u.ID, same, now-1000)
	f.exec(`INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, text, note, color, chapter, page, created_at, updated_at, deleted) VALUES (?, ?, 'h1', 'epub', 'cfi-h', 't', '', 'yellow', '', 0, 1, 1, 0)`, f.u.ID, same)
	f.exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'epub', 'cfi-account', 0.4, ?)`, f.u.ID, differs, now-1000)
	f.exec(`INSERT INTO book_digests (book_uuid, format, digest, size, mtime) VALUES (?, 'epub', 'doc1', 1, 1)`, kobo)
	f.exec(`INSERT INTO device_progress (user_id, document, progress, percentage, device, updated_at) VALUES (?, 'doc1', '/body/DocFragment[3]', 0.8, 'Kobo', ?)`, f.u.ID, now)

	pos := func(p string, pct float64) []progressRec {
		return []progressRec{{Format: "epub", Position: p, Percent: pct, UpdatedAt: now - 5000}}
	}
	got := f.preview(
		guestBook{UUID: fresh, Positions: pos("cfi-1", 0.1), Annotations: []annotationRec{{ID: "g1", Format: "epub", Anchor: "a"}, {ID: "gone", Format: "epub", Anchor: "b", Deleted: true}}},
		guestBook{UUID: same, Positions: pos("cfi-same", 0.3), Annotations: []annotationRec{{ID: "h1", Format: "epub", Anchor: "cfi-h"}}},
		guestBook{UUID: differs, Positions: pos("cfi-device", 0.6)},
		guestBook{UUID: kobo, Positions: pos("cfi-x", 0.2)},
		guestBook{UUID: "not-in-library", Positions: pos("x", 0.1)},
	)
	if p := got[fresh]; p.State != "new" || p.NewAnnotations != 1 || p.Device == nil || p.Book == nil || p.Book.Title != "Book A" {
		t.Errorf("fresh: %+v", p)
	}
	if p := got[same]; p.State != "same" {
		t.Errorf("same: %+v", p)
	}
	if p := got[differs]; p.State != "conflict" || p.Account == nil || p.Account.Percent != 0.4 || p.Device.Percent != 0.6 {
		t.Errorf("differs: %+v", p)
	}
	if p := got[kobo]; p.State != "conflict" || p.Account == nil || p.Account.Device != "Kobo" {
		t.Errorf("kobo (a KOReader position counts): %+v", p)
	}
	if p := got["not-in-library"]; p.State != "missing" || p.Book != nil {
		t.Errorf("missing: %+v", p)
	}
	if n := f.count(`SELECT COUNT(*) FROM annotations`); n != 1 {
		t.Errorf("a preview wrote: %d annotations", n)
	}
}

func TestGuestImportApply(t *testing.T) {
	f := newImportFixture(t, 3)
	now := time.Now().UnixMilli()
	older, fresh, ended := f.books[0], f.books[1], f.books[2]

	// The account's place in older is newer than the device's; the user
	// keeps the device's anyway.
	f.exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'epub', 'cfi-account', 0.5, ?)`, f.u.ID, older, now)
	// The same passage highlighted in the account, with a note.
	f.exec(`INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, text, note, color, chapter, page, created_at, updated_at, deleted) VALUES (?, ?, 'acc', 'epub', 'cfi-p', 't', 'Account note', 'yellow', '', 0, 1, 1, 0)`, f.u.ID, older)

	books := []applyBook{
		{guestBook{UUID: older,
			Positions:   []progressRec{{Format: "epub", Position: "cfi-device", Percent: 0.3, UpdatedAt: now - 86400000}},
			Annotations: []annotationRec{{ID: "dev", Format: "epub", Anchor: "cfi-p", Note: "Device note", UpdatedAt: now - 1}},
			Bookmarks:   []bookmarkRec{{ID: "bm1", Format: "epub", Position: "cfi-b", Label: "here"}},
		}, true},
		{guestBook{UUID: fresh, Positions: []progressRec{{Format: "epub", Position: "cfi-f", Percent: 0.2, UpdatedAt: now - 10}}}, false},
		{guestBook{UUID: ended, Positions: []progressRec{{Format: "pdf", Position: "300", Percent: 0.99, UpdatedAt: now - 10}}}, false},
	}
	f.apply(books...)

	// The chosen older place is now the newest, everywhere.
	if p := latestProgress(f.u.ID, older)[older]; p.Position != "cfi-device" || p.UpdatedAt <= now {
		t.Errorf("older: kept %q at %d (account's was at %d)", p.Position, p.UpdatedAt, now)
	}
	// Both notes kept on the one highlight; no second highlight of it.
	var note string
	if err := appDB.QueryRow(`SELECT note FROM annotations WHERE user_id = ? AND client_id = 'acc'`, f.u.ID).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if note != "Account note\n\nDevice note" {
		t.Errorf("merged note %q", note)
	}
	if n := f.count(`SELECT COUNT(*) FROM annotations WHERE user_id = ?`, f.u.ID); n != 1 {
		t.Errorf("%d annotations, want the account's one", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM bookmarks WHERE user_id = ?`, f.u.ID); n != 1 {
		t.Errorf("%d bookmarks", n)
	}
	// A book new to the account: its place is taken without asking, and it's
	// being read; one read to the end is finished.
	if p := latestProgress(f.u.ID, fresh)[fresh]; p.Position != "cfi-f" {
		t.Errorf("fresh place %q", p.Position)
	}
	if st, _ := readStatus(f.u.ID, fresh); st != "reading" {
		t.Errorf("fresh status %q", st)
	}
	if st, _ := readStatus(f.u.ID, ended); st != "finished" {
		t.Errorf("ended status %q", st)
	}

	// Again: nothing changes.
	var at int64
	_ = appDB.QueryRow(`SELECT updated_at FROM reading_progress WHERE user_id = ? AND book_uuid = ?`, f.u.ID, older).Scan(&at)
	f.apply(books...)
	if n := f.count(`SELECT COUNT(*) FROM annotations WHERE user_id = ?`, f.u.ID) + f.count(`SELECT COUNT(*) FROM bookmarks WHERE user_id = ?`, f.u.ID); n != 2 {
		t.Errorf("re-applying added rows: %d", n)
	}
	var note2 string
	_ = appDB.QueryRow(`SELECT note FROM annotations WHERE client_id = 'acc'`).Scan(&note2)
	if note2 != note {
		t.Errorf("re-applying changed the note: %q", note2)
	}
	if p := f.preview(books[0].guestBook)[older]; p.State != "same" {
		t.Errorf("after importing, the device's records should count as the same: %+v", p)
	}

	// Keeping the account's place: the device's isn't written.
	f.exec(`INSERT INTO reading_progress (user_id, book_uuid, format, position, percent, updated_at) VALUES (?, ?, 'pdf', '7', 0.1, ?) ON CONFLICT DO NOTHING`, f.u.ID, fresh, now+60000)
	f.apply(applyBook{guestBook{UUID: fresh, Positions: []progressRec{{Format: "epub", Position: "cfi-other", Percent: 0.6, UpdatedAt: now - 5}}}, false})
	if p := latestProgress(f.u.ID, fresh)[fresh]; p.Position != "7" {
		t.Errorf("kept the account's place, but it's now %q", p.Position)
	}
}

func TestGuestImportNeedsSignIn(t *testing.T) {
	setupTestAppDB(t)
	for _, h := range []http.HandlerFunc{requireUser(guestImportPreviewHandler), requireUser(guestImportHandler)} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/api/me/import", strings.NewReader(`{"books":[]}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("signed out: %d", rec.Code)
		}
	}
}
