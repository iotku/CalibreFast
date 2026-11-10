package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func annotationMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me/annotations/{uuid}", requireUser(annotationsGetHandler))
	mux.HandleFunc("PUT /api/me/annotations/{uuid}/{id}", requireUser(annotationPutHandler))
	return mux
}

func TestAnnotationsSync(t *testing.T) {
	u, books := setupLibraryAndUser(t, 2)
	other, _ := createUser("other", "longenough-pw", false)
	mux := annotationMux()
	b := books[0]
	now := time.Now().UnixMilli()

	list := func(who *User, book string) []annotationRec {
		t.Helper()
		var out struct{ Annotations []annotationRec }
		rec := call(t, mux, who, "GET", "/api/me/annotations/"+book, "")
		if rec.Code != 200 {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Annotations
	}
	put := func(book, id string, a annotationRec) (annotationRec, int) {
		t.Helper()
		body, _ := json.Marshal(a)
		rec := call(t, mux, u, "PUT", "/api/me/annotations/"+book+"/"+id, string(body))
		var out annotationRec
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out, rec.Code
	}
	epub := annotationRec{
		Format: "epub", Anchor: "epubcfi(/6/8!/4/2,/1:0,/1:42)", Text: "a highlighted sentence",
		Color: "yellow", Chapter: "One", Page: 12, CreatedAt: now - 5000, UpdatedAt: now - 5000,
	}

	if rec := call(t, mux, nil, "GET", "/api/me/annotations/"+b, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", rec.Code)
	}
	if rec := call(t, mux, nil, "PUT", "/api/me/annotations/"+b+"/a1", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous put: %d", rec.Code)
	}

	if got, code := put(b, "a1", epub); code != 200 || got.ID != "a1" || got.Text != epub.Text || got.CreatedAt != epub.CreatedAt {
		t.Fatalf("add: %d %+v", code, got)
	}
	pdf := annotationRec{Format: "pdf", Anchor: `{"page":3,"rects":[[72,100,300,112]]}`, Text: "pdf text", Color: "green", Page: 3, CreatedAt: now - 4000, UpdatedAt: now - 4000}
	if _, code := put(b, "a2", pdf); code != 200 {
		t.Fatalf("add pdf: %d", code)
	}
	if got := list(u, b); len(got) != 2 || got[0].ID != "a1" || got[1].Anchor != pdf.Anchor {
		t.Fatalf("list: %+v", got)
	}
	if got := list(other, b); len(got) != 0 {
		t.Fatal("annotations leaked across users")
	}

	// A note added later wins; where the annotation is doesn't change.
	edit := epub
	edit.Note, edit.Color, edit.UpdatedAt = "my note", "blue", now-3000
	edit.Anchor, edit.CreatedAt = "epubcfi(/6/99!/4/2,/1:0,/1:1)", now-1
	if got, _ := put(b, "a1", edit); got.Note != "my note" || got.Color != "blue" || got.Anchor != epub.Anchor || got.CreatedAt != epub.CreatedAt {
		t.Fatalf("edit: %+v", got)
	}
	// An older change (a device that was offline) doesn't.
	stale := epub
	stale.Note, stale.UpdatedAt = "stale", now-4500
	if got, _ := put(b, "a1", stale); got.Note != "my note" {
		t.Fatalf("an older change won: %+v", got)
	}

	// A delete is kept as a tombstone, without the text, and beats an older edit…
	del := edit
	del.Deleted, del.UpdatedAt = true, now-2000
	if got, _ := put(b, "a1", del); !got.Deleted || got.Text != "" || got.Note != "" {
		t.Fatalf("delete: %+v", got)
	}
	late := edit
	late.Note, late.UpdatedAt = "edited before the delete", now-2500
	if got, _ := put(b, "a1", late); !got.Deleted {
		t.Fatalf("an edit older than the delete brought it back: %+v", got)
	}
	if got := list(u, b); len(got) != 2 || !got[0].Deleted {
		t.Fatalf("tombstone not listed: %+v", got)
	}
	// …and an edit made after it brings the annotation back.
	back := edit
	back.Note, back.UpdatedAt = "restored", now-1000
	if got, _ := put(b, "a1", back); got.Deleted || got.Note != "restored" || got.Text != epub.Text {
		t.Fatalf("a newer edit should restore: %+v", got)
	}

	// A clock far in the future is clamped, so it can't win forever.
	future := edit
	future.UpdatedAt = now + 365*24*3600*1000
	if got, _ := put(b, "a1", future); got.UpdatedAt > time.Now().UnixMilli()+annotationClockSkew {
		t.Fatalf("future timestamp not clamped: %d", got.UpdatedAt)
	}

	// An id can't be moved to another book.
	if _, code := put(books[1], "a2", pdf); code != http.StatusConflict {
		t.Fatalf("id reused on another book: %d", code)
	}
	if got := list(u, books[1]); len(got) != 0 {
		t.Fatalf("id moved across books: %+v", got)
	}

	for name, bad := range map[string]annotationRec{
		"format":  {Format: "mobi", Anchor: "x"},
		"anchor":  {Format: "epub"},
		"colour":  {Format: "epub", Anchor: "x", Color: "chartreuse"},
		"text":    {Format: "epub", Anchor: "x", Text: strings.Repeat("x", maxAnnotationText+1)},
		"note":    {Format: "epub", Anchor: "x", Note: strings.Repeat("x", maxAnnotationText+1)},
		"page":    {Format: "pdf", Anchor: "x", Page: -1},
		"too far": {Format: "pdf", Anchor: strings.Repeat("x", maxAnchorLen+1)},
	} {
		if _, code := put(b, "bad", bad); code != http.StatusBadRequest {
			t.Errorf("bad %s: %d", name, code)
		}
	}
	if _, code := put("no-such-book", "a9", epub); code != http.StatusNotFound {
		t.Fatalf("unknown book: %d", code)
	}

	// Deleting the user takes their annotations along.
	if _, err := appDB.Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM annotations`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d annotations left after their user was deleted", n)
	}
}

func TestAnnotationsLimit(t *testing.T) {
	u, books := setupLibraryAndUser(t, 1)
	mux := annotationMux()
	for i := 0; i < maxAnnotationsBook; i++ {
		if _, err := appDB.Exec(`INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, created_at, updated_at)
			VALUES (?, ?, ?, 'pdf', 'x', 1, 1)`, u.ID, books[0], fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"format":"pdf","anchor":"x"}`
	if rec := call(t, mux, u, "PUT", "/api/me/annotations/"+books[0]+"/one-more", body); rec.Code != http.StatusConflict {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	// Existing ones can still be changed and deleted.
	if rec := call(t, mux, u, "PUT", "/api/me/annotations/"+books[0]+"/n0", `{"format":"pdf","anchor":"x","deleted":true}`); rec.Code != 200 {
		t.Fatalf("delete at the limit: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, mux, u, "PUT", "/api/me/annotations/"+books[0]+"/one-more", body); rec.Code != 200 {
		t.Fatalf("after freeing one: %d %s", rec.Code, rec.Body)
	}
}

// A database from before annotations (schema 5) gains the table and keeps its data.
func TestAnnotationsMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, appDBFile)+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:5] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
		_, _ = db.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1)
	}
	if _, err := db.Exec(`INSERT INTO users (id, username, created_at) VALUES (7, 'reader', 1);
		INSERT INTO bookmarks (user_id, book_uuid, client_id, format, position, created_at) VALUES (7, 'b', 'bm', 'pdf', '3', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = openAppDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, bookmarks int
	_ = db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version)
	_ = db.QueryRow(`SELECT COUNT(*) FROM bookmarks`).Scan(&bookmarks)
	if version != len(migrations) || bookmarks != 1 {
		t.Fatalf("version %d (want %d), %d bookmarks (want 1)", version, len(migrations), bookmarks)
	}
	if _, err := db.Exec(`INSERT INTO annotations (user_id, book_uuid, client_id, format, anchor, created_at, updated_at)
		VALUES (7, 'b', 'a', 'pdf', 'x', 1, 1)`); err != nil {
		t.Fatalf("annotations table: %v", err)
	}
}
