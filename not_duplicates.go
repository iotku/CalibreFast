package main

// Books an admin marked as not duplicates of each other (bulk editor,
// web/library-edit.ts). The duplicates view groups books that look alike;
// a group whose books are all in one marked set isn't shown there, but in
// its own view ("Marked not duplicates"), from which a set can be unmarked.
// A book that joins the group later (a new upload) makes it show again.
//
// Kept in CalibreFast's own database: nothing is written to the library.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
)

// notDuplicateSet is one marking (TS: NotDuplicateSet in web/types.ts).
type notDuplicateSet struct {
	ID    int64    `json:"id"`
	UUIDs []string `json:"uuids"`
}

func registerNotDuplicateRoutes() {
	http.HandleFunc("POST /api/library/not-duplicates", requireWrites(markNotDuplicatesHandler))
	http.HandleFunc("DELETE /api/library/not-duplicates/{id}", requireWrites(unmarkNotDuplicatesHandler))
}

// notDuplicateSets reads every set.
func notDuplicateSets() ([]notDuplicateSet, error) {
	out := []notDuplicateSet{}
	if appDB == nil {
		return out, nil
	}
	rows, err := appDB.Query(`SELECT set_id, book_uuid FROM not_duplicate_books ORDER BY set_id, book_uuid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var u string
		if err := rows.Scan(&id, &u); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != id {
			out = append(out, notDuplicateSet{ID: id})
		}
		out[len(out)-1].UUIDs = append(out[len(out)-1].UUIDs, u)
	}
	return out, rows.Err()
}

var errNotDuplicateSize = errors.New("mark at least two books")

// markNotDuplicates records that these books aren't duplicates of each other.
func markNotDuplicates(uuids []string, by string) (int64, error) {
	uuids = uniqueStrings(uuids)
	if len(uuids) < 2 {
		return 0, errNotDuplicateSize
	}
	tx, err := appDB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO not_duplicate_sets (created_by, created_at) VALUES (?, ?)`, by, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, u := range uuids {
		if _, err := tx.Exec(`INSERT INTO not_duplicate_books (set_id, book_uuid) VALUES (?, ?)`, id, u); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func markNotDuplicatesHandler(w http.ResponseWriter, r *http.Request) {
	uuids, ok := readUUIDs(w, r)
	if !ok {
		return
	}
	id, err := markNotDuplicates(uuids, currentUser(r).Username)
	if errors.Is(err, errNotDuplicateSize) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		log.Println("not duplicates:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(notDuplicateSet{ID: id, UUIDs: uniqueStrings(uuids)}), "encode not duplicates")
}

func unmarkNotDuplicatesHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	res, err := appDB.Exec(`DELETE FROM not_duplicate_sets WHERE id = ?`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "no such marking", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
