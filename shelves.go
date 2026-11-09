package main

// Bookshelves: named lists of books.
//   - Personal shelves (owner_id = user): visible to and editable by their
//     owner only.
//   - Global shelves (owner_id NULL): visible to everyone who can browse,
//     created and edited by admins (e.g. to showcase a collection).
// Stored in the app database; the library itself is never modified.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	maxShelfName      = 100
	maxShelfDesc      = 1000
	maxShelvesPerUser = 200
	maxBooksPerShelf  = 10000
)

type Shelf struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Global      bool   `json:"global"`
	Count       int    `json:"count"`
	CanEdit     bool   `json:"can_edit"`
	ownerID     sql.NullInt64
}

// shelfMark: a shelf and whether a given book is on it (for the book popup).
type shelfMark struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Global  bool   `json:"global"`
	Has     bool   `json:"has"`
	CanEdit bool   `json:"can_edit"`
}

var errShelfNotFound = errors.New("no such shelf")

func getShelf(id int64) (*Shelf, error) {
	var s Shelf
	err := appDB.QueryRow(`SELECT s.id, s.name, s.description, s.owner_id,
			(SELECT COUNT(*) FROM shelf_books b WHERE b.shelf_id = s.id)
		FROM shelves s WHERE s.id = ?`, id).Scan(&s.ID, &s.Name, &s.Description, &s.ownerID, &s.Count)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errShelfNotFound
	}
	if err != nil {
		return nil, err
	}
	s.Global = !s.ownerID.Valid
	return &s, nil
}

// canView: global shelves are public (to whoever can browse); personal ones
// only to their owner.
func (s *Shelf) canView(u *User) bool {
	return s.Global || (u != nil && s.ownerID.Int64 == u.ID)
}

// canEdit: owner for personal shelves, admins for global ones.
func (s *Shelf) canEdit(u *User) bool {
	if u == nil {
		return false
	}
	if s.Global {
		return u.IsAdmin
	}
	return s.ownerID.Int64 == u.ID
}

// listShelves returns the user's own shelves and all global shelves.
func listShelves(u *User) (mine, global []Shelf, err error) {
	q := `SELECT s.id, s.name, s.description, s.owner_id,
			(SELECT COUNT(*) FROM shelf_books b WHERE b.shelf_id = s.id)
		FROM shelves s WHERE s.owner_id IS NULL`
	args := []any{}
	if u != nil {
		q += ` OR s.owner_id = ?`
		args = append(args, u.ID)
	}
	rows, err := appDB.Query(q+` ORDER BY s.name COLLATE NOCASE`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	mine, global = []Shelf{}, []Shelf{}
	for rows.Next() {
		var s Shelf
		if err := rows.Scan(&s.ID, &s.Name, &s.Description, &s.ownerID, &s.Count); err != nil {
			return nil, nil, err
		}
		s.Global = !s.ownerID.Valid
		s.CanEdit = s.canEdit(u)
		if s.Global {
			global = append(global, s)
		} else {
			mine = append(mine, s)
		}
	}
	return mine, global, rows.Err()
}

// shelfMarks lists the shelves a user can add this book to, and whether it's
// on each.
func shelfMarks(u *User, uuid string) []shelfMark {
	out := []shelfMark{}
	rows, err := appDB.Query(`SELECT s.id, s.name, s.owner_id,
			EXISTS(SELECT 1 FROM shelf_books b WHERE b.shelf_id = s.id AND b.book_uuid = ?)
		FROM shelves s WHERE s.owner_id = ? OR s.owner_id IS NULL
		ORDER BY s.owner_id IS NULL, s.name COLLATE NOCASE`, uuid, u.ID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var m shelfMark
		var owner sql.NullInt64
		if rows.Scan(&m.ID, &m.Name, &owner, &m.Has) != nil {
			continue
		}
		m.Global = !owner.Valid
		m.CanEdit = !m.Global || u.IsAdmin
		// Regular users only see global shelves this book is on (read-only).
		if m.Global && !m.CanEdit && !m.Has {
			continue
		}
		out = append(out, m)
	}
	return out
}

func cleanShelfFields(name, desc string) (string, string, error) {
	name, desc = strings.TrimSpace(name), strings.TrimSpace(desc)
	if name == "" || utf8.RuneCountInString(name) > maxShelfName {
		return "", "", errors.New("shelf name must be 1–100 characters")
	}
	if utf8.RuneCountInString(desc) > maxShelfDesc {
		return "", "", errors.New("description must be at most 1000 characters")
	}
	return name, desc, nil
}

// ---------------------------------------------------------------------------
// HTTP: JSON API
// ---------------------------------------------------------------------------

func shelvesListHandler(w http.ResponseWriter, r *http.Request) {
	mine, global, err := listShelves(currentUser(r))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mine": mine, "global": global})
}

func shelfCreateHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Global      bool   `json:"global"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name, desc, err := cleanShelfFields(req.Name, req.Description)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var owner any = u.ID
	if req.Global {
		if !u.IsAdmin {
			http.Error(w, "only admins can create global shelves", http.StatusForbidden)
			return
		}
		owner = nil
	} else {
		var n int
		_ = appDB.QueryRow(`SELECT COUNT(*) FROM shelves WHERE owner_id = ?`, u.ID).Scan(&n)
		if n >= maxShelvesPerUser {
			http.Error(w, "too many shelves", http.StatusConflict)
			return
		}
	}
	now := nowUnix()
	res, err := appDB.Exec(`INSERT INTO shelves (owner_id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		owner, name, desc, now, now)
	if err != nil {
		http.Error(w, "could not create shelf", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	s, _ := getShelf(id)
	s.CanEdit = true
	writeJSON(w, http.StatusCreated, s)
}

// shelfForEdit loads a shelf the current user may edit (404 if they can't
// even see it, so private shelves don't leak their existence).
func shelfForEdit(w http.ResponseWriter, r *http.Request) (*Shelf, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return nil, false
	}
	u := currentUser(r)
	s, err := getShelf(id)
	if err != nil || !s.canView(u) {
		http.Error(w, "no such shelf", http.StatusNotFound)
		return nil, false
	}
	if !s.canEdit(u) {
		http.Error(w, "you can't change this shelf", http.StatusForbidden)
		return nil, false
	}
	return s, true
}

func shelfUpdateHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name, desc := s.Name, s.Description
	if req.Name != nil {
		name = *req.Name
	}
	if req.Description != nil {
		desc = *req.Description
	}
	name, desc, err := cleanShelfFields(name, desc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := appDB.Exec(`UPDATE shelves SET name = ?, description = ?, updated_at = ? WHERE id = ?`, name, desc, nowUnix(), s.ID); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	s, _ = getShelf(s.ID)
	s.CanEdit = true
	writeJSON(w, http.StatusOK, s)
}

func shelfDeleteHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	if _, err := appDB.Exec(`DELETE FROM shelves WHERE id = ?`, s.ID); err != nil {
		http.Error(w, "could not delete", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func shelfAddBookHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	if s.Count >= maxBooksPerShelf {
		http.Error(w, "this shelf is full", http.StatusConflict)
		return
	}
	if _, err := appDB.Exec(`INSERT OR IGNORE INTO shelf_books (shelf_id, book_uuid, added_at) VALUES (?, ?, ?)`, s.ID, uuid, nowUnix()); err != nil {
		http.Error(w, "could not add", http.StatusInternalServerError)
		return
	}
	_, _ = appDB.Exec(`UPDATE shelves SET updated_at = ? WHERE id = ?`, nowUnix(), s.ID)
	w.WriteHeader(http.StatusNoContent)
}

func shelfRemoveBookHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	if _, err := appDB.Exec(`DELETE FROM shelf_books WHERE shelf_id = ? AND book_uuid = ?`, s.ID, r.PathValue("uuid")); err != nil {
		http.Error(w, "could not remove", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// HTTP: pages
// ---------------------------------------------------------------------------

func shelvesPageHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	mine, global, err := listShelves(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	logErr(templates.ExecuteTemplate(w, "shelves.html", map[string]any{
		"Title": SiteTitle, "User": u, "Mine": mine, "Global": global,
	}), "render shelves")
}

// shelfPageHandler: GET /shelf/{id} — the shelf's books on the shared grid.
func shelfPageHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	u := currentUser(r)
	s, err := getShelf(id)
	if err != nil || !s.canView(u) {
		http.NotFound(w, r)
		return
	}
	sub := "your shelf"
	if s.Global {
		sub = "global shelf"
	}
	serveUUIDList(w, r, uuidList{
		Title: s.Name, Sub: sub, BaseURL: "/shelf/" + itoa(s.ID),
		EmptyText: "This shelf is empty. Add books from a book's Shelves menu.",
		Count:     countQuery(`SELECT COUNT(*) FROM shelf_books WHERE shelf_id = ?`, s.ID),
		Page: func(limit, offset int) ([]string, error) {
			return scanStrings(appDB.Query(`SELECT book_uuid FROM shelf_books WHERE shelf_id = ? ORDER BY added_at, book_uuid LIMIT ? OFFSET ?`, s.ID, limit, offset))
		},
	})
}

func registerShelfRoutes() {
	http.HandleFunc("GET /shelves", shelvesPageHandler)
	http.HandleFunc("GET /shelf/{id}", shelfPageHandler)
	http.HandleFunc("GET /api/shelves", shelvesListHandler)
	http.HandleFunc("POST /api/shelves", requireUser(shelfCreateHandler))
	http.HandleFunc("PATCH /api/shelves/{id}", requireUser(shelfUpdateHandler))
	http.HandleFunc("DELETE /api/shelves/{id}", requireUser(shelfDeleteHandler))
	http.HandleFunc("PUT /api/shelves/{id}/books/{uuid}", requireUser(shelfAddBookHandler))
	http.HandleFunc("DELETE /api/shelves/{id}/books/{uuid}", requireUser(shelfRemoveBookHandler))
}
