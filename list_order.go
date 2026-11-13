package main

// Books arranged by hand: Favorites, Want to read, Finished and the shelves
// a user can edit keep the order they gave them (Edit in the list page's
// top bar, web/list-edit.ts).
//
//   - PUT /api/me/lists/{list}/order {"order": [uuid, …]} (favorites,
//     wanted, finished)
//   - PUT /api/shelves/{id}/order {"order": [uuid, …]}
//
// The order is the whole list as the user arranged it. Books left out of it
// (added since the editor loaded, or gone from the library) lose their
// place and go where a new book would: first on the user's lists (newest
// first, as before any arranging), last on a shelf (oldest first).
// A book that changes status loses its place too (userdata.go setStatus).

import (
	"encoding/json"
	"net/http"
)

// The lists' orders: arranged books in their places, the rest by date.
const (
	favoritesOrder = `ORDER BY position IS NOT NULL, position, created_at DESC, book_uuid`
	statusOrder    = `ORDER BY position IS NOT NULL, position, updated_at DESC, book_uuid`
	shelfOrder     = `ORDER BY position IS NULL, position, added_at, book_uuid`
)

// maxArrangeBooks: the most books the editor loads (and an order may name).
const maxArrangeBooks = maxBooksPerShelf

// arrangeable: the user's lists with an order of their own.
var arrangeable = map[string]bool{"favorites": true, "wanted": true, "finished": true}

// readBookOrder reads {"order": [uuid, …]}: at most maxArrangeBooks, no repeats.
func readBookOrder(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var req struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || len(req.Order) > maxArrangeBooks {
		http.Error(w, "invalid order", http.StatusBadRequest)
		return nil, false
	}
	seen := make(map[string]bool, len(req.Order))
	for _, u := range req.Order {
		if u == "" || len(u) > 64 || seen[u] {
			http.Error(w, "invalid order", http.StatusBadRequest)
			return nil, false
		}
		seen[u] = true
	}
	return req.Order, true
}

// saveOrder: clear, then number the books given (books not on the list
// match no row, so they're ignored).
func saveOrder(clear, set string, args []any, order []string) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(clear, args...); err != nil {
		return err
	}
	stmt, err := tx.Prepare(set)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, uuid := range order {
		if _, err := stmt.Exec(append([]any{i, uuid}, args...)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func listOrderHandler(w http.ResponseWriter, r *http.Request) {
	list := r.PathValue("list")
	if !arrangeable[list] {
		http.NotFound(w, r)
		return
	}
	order, ok := readBookOrder(w, r)
	if !ok {
		return
	}
	uid := currentUser(r).ID
	var err error
	if list == "favorites" {
		err = saveOrder(`UPDATE favorites SET position = NULL WHERE user_id = ?`,
			`UPDATE favorites SET position = ? WHERE book_uuid = ? AND user_id = ?`, []any{uid}, order)
	} else {
		err = saveOrder(`UPDATE reading_status SET position = NULL WHERE user_id = ? AND status = ?`,
			`UPDATE reading_status SET position = ? WHERE book_uuid = ? AND user_id = ? AND status = ?`, []any{uid, list}, order)
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func shelfBookOrderHandler(w http.ResponseWriter, r *http.Request) {
	s, ok := shelfForEdit(w, r)
	if !ok {
		return
	}
	order, ok := readBookOrder(w, r)
	if !ok {
		return
	}
	if err := saveOrder(`UPDATE shelf_books SET position = NULL WHERE shelf_id = ?`,
		`UPDATE shelf_books SET position = ? WHERE book_uuid = ? AND shelf_id = ?`, []any{s.ID}, order); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	_, _ = appDB.Exec(`UPDATE shelves SET updated_at = ? WHERE id = ?`, nowUnix(), s.ID)
	shelfChanged(s)
	w.WriteHeader(http.StatusNoContent)
}

// wantedInOrder: u's want-to-read books, in the order of their page.
func wantedInOrder(u *User) ([]string, error) {
	uuids, err := scanStrings(appDB.Query(`SELECT book_uuid FROM reading_status WHERE user_id = ? AND status = 'wanted' `+statusOrder, u.ID))
	if uuids == nil && err == nil {
		uuids = []string{}
	}
	return uuids, err
}

func registerListOrderRoutes() {
	http.HandleFunc("PUT /api/me/lists/{list}/order", requireUser(listOrderHandler))
	http.HandleFunc("PUT /api/shelves/{id}/order", requireUser(shelfBookOrderHandler))
}
