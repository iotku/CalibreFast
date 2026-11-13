package main

// The user's side of "Recommended for you" (recommend.go):
//
//   - Not interested: PUT /api/me/books/{uuid}/not-interested
//     {"not_interested": bool}. The book never comes up in the user's
//     recommendations again (until they take it back).
//   - The row stays the same from load to load: its randomness comes from
//     a seed kept for the user, and it shows the page of the ranked list
//     they're on. POST /api/me/recommended/next steps to the next page,
//     and after the last back to the first with a new seed; it answers the
//     new row.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
)

// recState: the user's seed and page (their id and the first page until
// they first ask for more).
func recState(u *User) (seed uint64, page int, err error) {
	var s int64
	err = appDB.QueryRow(`SELECT seed, page FROM rec_state WHERE user_id = ?`, u.ID).Scan(&s, &page)
	if errors.Is(err, sql.ErrNoRows) {
		return uint64(u.ID), 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return uint64(s), page, nil
}

// notInterested: the books the user said they aren't interested in.
func notInterested(u *User) (map[string]bool, error) {
	uuids, err := scanStrings(appDB.Query(`SELECT book_uuid FROM not_interested WHERE user_id = ?`, u.ID))
	out := make(map[string]bool, len(uuids))
	for _, id := range uuids {
		out[id] = true
	}
	return out, err
}

// userRecommend: the page of the user's row they're on, and their tags.
func userRecommend(u *User, marks readingMarks) (recs []recBook, tags []string, err error) {
	seed, page, err := recState(u)
	if err != nil {
		return nil, nil, err
	}
	hide, err := notInterested(u)
	if err != nil {
		return nil, nil, err
	}
	recs, _, tags = recommend(marks, hide, rand.New(rand.NewPCG(seed, 0)), page)
	return recs, tags, nil
}

type recNextAnswer struct {
	Recommended []recBook `json:"recommended"`
}

func recNextHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	marks, _, err := userReading(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	seed, page, err := recState(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	hide, err := notInterested(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	_, pages, _ := recommend(marks, hide, rand.New(rand.NewPCG(seed, 0)), page)
	page++
	if page >= pages {
		// Past the last page: the first again, ranked with new randomness.
		page, seed = 0, rand.Uint64()>>1 // fits SQLite's signed integer
	}
	if _, err := appDB.Exec(`INSERT INTO rec_state (user_id, seed, page) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET seed = excluded.seed, page = excluded.page`, u.ID, int64(seed), page); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	recs, _, _ := recommend(marks, hide, rand.New(rand.NewPCG(seed, 0)), page)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, recNextAnswer{Recommended: recs})
}

func setNotInterestedHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	var req struct {
		NotInterested bool `json:"not_interested"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var err error
	if req.NotInterested {
		_, err = appDB.Exec(`INSERT OR IGNORE INTO not_interested (user_id, book_uuid, created_at) VALUES (?, ?, ?)`, u.ID, uuid, nowUnix())
	} else {
		_, err = appDB.Exec(`DELETE FROM not_interested WHERE user_id = ? AND book_uuid = ?`, u.ID, uuid)
	}
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"not_interested": req.NotInterested})
}
