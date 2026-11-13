package main

// The user's side of "Recommended for you" (recommend.go):
//
//   - Not interested: PUT /api/me/books/{uuid}/not-interested
//     {"not_interested": bool}. The book never comes up in the user's
//     recommendations again (until they take it back).
//   - The row stays the same from load to load: it's kept as last shown
//     (rec_state.shown). A book that can't stay (marked since, hidden, or
//     gone from the library) is replaced where it was by the next of the
//     ranked list, so marking one doesn't change the others, though the
//     marks change the ranking. The list's randomness comes from a seed
//     kept for the user, and fills come from the page they're on.
//   - POST /api/me/recommended/next: a new row from the next page, and
//     after the last back to the first with a new seed.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"slices"
)

// recShown: a book of the row as last shown, and why it was there.
type recShown struct {
	UUID    string `json:"u"`
	Kind    string `json:"k"`
	Because string `json:"b,omitempty"`
}

// recState: the user's seed, page and row as last shown (their id, the
// first page and no row until they first see one).
func recState(u *User) (seed uint64, page int, shown []recShown, err error) {
	var s int64
	var row sql.NullString
	err = appDB.QueryRow(`SELECT seed, page, shown FROM rec_state WHERE user_id = ?`, u.ID).Scan(&s, &page, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return uint64(u.ID), 0, nil, nil
	}
	if err != nil {
		return 0, 0, nil, err
	}
	if row.Valid {
		shown = []recShown{}
		_ = json.Unmarshal([]byte(row.String), &shown) // unreadable: a fresh row
	}
	return uint64(s), page, shown, nil
}

func saveRecState(u *User, seed uint64, page int, recs []recBook) error {
	row := make([]recShown, len(recs))
	for i, r := range recs {
		row[i] = recShown{r.Book.UUID, r.Kind, r.Because}
	}
	js, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = appDB.Exec(`INSERT INTO rec_state (user_id, seed, page, shown) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET seed = excluded.seed, page = excluded.page, shown = excluded.shown`,
		u.ID, int64(seed), page, string(js))
	return err
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

// stillRecommended: whether a book of the row as shown can stay: in the
// library, not hidden, and not marked since (a next in series may be on
// the want-to-read list, as recommend allows).
func stillRecommended(c *catalog, s recShown, m readingMarks, hide map[string]bool) (catalogBook, bool) {
	b, ok := c.lookup(s.UUID)
	if !ok || hide[s.UUID] {
		return b, false
	}
	_, reading := m.Reading[s.UUID]
	if reading || slices.Contains(m.Finished, s.UUID) || slices.Contains(m.Favorites, s.UUID) {
		return b, false
	}
	return b, s.Kind == "series" || !slices.Contains(m.Wanted, s.UUID)
}

// settleRow: the row as shown, with each book that can't stay replaced
// where it was by the next of the list (from the page the user is on) not
// already in it, and topped up to a full row. changed: it isn't the row
// as shown.
func settleRow(c *catalog, shown []recShown, l recList, page int, m readingMarks, hide map[string]bool) (row []recBook, changed bool) {
	row = make([]recBook, len(shown))
	in := map[string]bool{}
	gaps := 0
	for i, s := range shown {
		if b, ok := stillRecommended(c, s, m, hide); ok {
			row[i] = recBook{Book: b.Book, Kind: s.Kind, Because: s.Because}
			in[s.UUID] = true
		} else {
			gaps++
		}
	}
	need := gaps + max(0, recRowMax-len(shown))
	var fill []recBook
	if n := l.len(); n > 0 && need > 0 {
		from := (max(0, page) * recRowMax) % n
		for k := 0; k < n && len(fill) < need; k++ {
			i := (from + k) % n
			if u := l.uuid(i); !in[u] {
				in[u] = true
				fill = append(fill, l.at(i))
			}
		}
	}
	out := []recBook{}
	for i := range row {
		switch {
		case row[i].Book.UUID != "":
			out = append(out, row[i])
		case len(fill) > 0:
			out, fill = append(out, fill[0]), fill[1:]
		}
	}
	out = append(out, fill...)
	return out, gaps > 0 || len(out) != len(shown)
}

// userRecommend: the user's row, and their tags. The row stays as it was
// last shown, but for books that can't stay: each is replaced in its place.
func userRecommend(u *User, marks readingMarks) (recs []recBook, tags []string, err error) {
	seed, page, shown, err := recState(u)
	if err != nil {
		return nil, nil, err
	}
	hide, err := notInterested(u)
	if err != nil {
		return nil, nil, err
	}
	l, tags := rankRecs(marks, hide, rand.New(rand.NewPCG(seed, 0)))
	changed := true
	if shown == nil {
		recs = l.page(page)
	} else {
		recs, changed = settleRow(l.cat, shown, l, page, marks, hide)
	}
	if changed {
		err = saveRecState(u, seed, page, recs)
	}
	return recs, tags, err
}

type recNextAnswer struct {
	Recommended []recBook `json:"recommended"`
}

// recNextHandler: the next page of the list, leaving out what's on screen
// where the list is long enough to.
func recNextHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	marks, _, err := userReading(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	seed, page, shown, err := recState(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	hide, err := notInterested(u)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	l, _ := rankRecs(marks, hide, rand.New(rand.NewPCG(seed, 0)))
	page++
	if page >= l.pages() {
		// Past the last page: the first again, ranked with new randomness.
		page, seed = 0, rand.Uint64()>>1 // fits SQLite's signed integer
		l, _ = rankRecs(marks, hide, rand.New(rand.NewPCG(seed, 0)))
	}
	// A blank row filled from the page: what was on screen comes last.
	onScreen := map[string]bool{}
	for _, s := range shown {
		onScreen[s.UUID] = true
	}
	recs := []recBook{}
	var later []int
	for k := 0; k < l.len() && len(recs) < recRowMax; k++ {
		i := (page*recRowMax + k) % l.len()
		if onScreen[l.uuid(i)] {
			later = append(later, i)
			continue
		}
		recs = append(recs, l.at(i))
	}
	for _, i := range later {
		if len(recs) >= recRowMax {
			break
		}
		recs = append(recs, l.at(i))
	}
	if err := saveRecState(u, seed, page, recs); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
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
