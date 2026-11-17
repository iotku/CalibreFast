package main

// Series on the home page, per user (app data only; book details come from
// the catalog):
//
//   - Pinned series (series_pins): a series' page has Pin to home. Its row
//     is among the pinned shelves and tags, in one order with them
//     (shelf_pins.go), and shows the series in order (seriesCompare),
//     leaving out the books the user finished unless they turned on the
//     row's Show finished.
//       PUT / DELETE /api/me/series-pins/{name}
//       PATCH /api/me/series-pins/{name} {"show_finished": true}
//   - On Deck: for each series the user finished a book of, the next book
//     after the furthest one finished that they haven't finished, unless
//     they're reading it (it's in Continue reading) or took it off with
//     the card's ✕ (on_deck_dismissed). Most recently finished series
//     first. Recommended's "Next in series" leaves these out.
//       DELETE /api/me/on-deck/{uuid}

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// userFinished: the books u finished.
func userFinished(u *User) (map[string]bool, error) {
	uuids, err := scanStrings(appDB.Query(`SELECT book_uuid FROM reading_status WHERE user_id = ? AND status = 'finished'`, u.ID))
	out := make(map[string]bool, len(uuids))
	for _, id := range uuids {
		out[id] = true
	}
	return out, err
}

// fillSeriesRows adds the books of the series rows of out (at those
// places): in series order, without the finished ones unless the row
// shows them.
func fillSeriesRows(u *User, out []homeShelf, at []int) error {
	finished, err := userFinished(u)
	if err != nil {
		return err
	}
	ix := currentRecIndex()
	c := ix.cat
	for _, i := range at {
		row := &out[i]
		slots := seriesBooks(ix, row.Name)
		row.Count = len(slots)
		row.Books = []Book{}
		for _, slot := range slots {
			b := &c.books[slot]
			done := finished[b.UUID]
			if done {
				row.Finished++
			}
			if (done && !row.ShowFinished) || len(row.Books) == shelfRowBooks {
				continue
			}
			row.Books = append(row.Books, b.Book)
		}
	}
	return nil
}

// seriesPinNames: the names u pinned that are browsed as name now.
func seriesPinNames(u *User, name string) ([]string, error) {
	names, err := scanStrings(appDB.Query(`SELECT name FROM series_pins WHERE user_id = ?`, u.ID))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		if strings.EqualFold(seriesEntity(n), name) {
			out = append(out, n)
		}
	}
	return out, nil
}

// seriesPinnedFor: whether the series (as browsed) is on u's page.
func seriesPinnedFor(u *User, name string) bool {
	names, err := seriesPinNames(u, name)
	return err == nil && len(names) > 0
}

// seriesFromPath: the series a request names, as browsed.
func seriesFromPath(r *http.Request) string {
	return seriesEntity(strings.TrimSpace(r.PathValue("name")))
}

// PUT /api/me/series-pins/{name}: a series on the user's page (at the end).
func seriesPinHandler(w http.ResponseWriter, r *http.Request) {
	name := seriesFromPath(r)
	if name == "" || len(name) > maxDiscoverTagNameLen || len(seriesBooks(currentRecIndex(), name)) == 0 {
		http.Error(w, "no such series", http.StatusNotFound)
		return
	}
	u := currentUser(r)
	if seriesPinnedFor(u, name) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := appDB.Exec(`INSERT INTO series_pins (user_id, name, position) VALUES (?1, ?2, `+nextPinPosition+`)
		ON CONFLICT (user_id, name) DO NOTHING`, u.ID, name); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/series-pins/{name}: off the user's page (under any name
// that's browsed as this one).
func seriesUnpinHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	names, err := seriesPinNames(u, seriesFromPath(r))
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	for _, n := range names {
		if _, err := appDB.Exec(`DELETE FROM series_pins WHERE user_id = ? AND name = ?`, u.ID, n); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// PATCH /api/me/series-pins/{name} {"show_finished": bool}: whether the
// row shows the books the user finished.
func seriesPinPatchHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShowFinished *bool `json:"show_finished"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.ShowFinished == nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	u := currentUser(r)
	names, err := seriesPinNames(u, seriesFromPath(r))
	if err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	if len(names) == 0 {
		http.Error(w, "series not pinned", http.StatusNotFound)
		return
	}
	for _, n := range names {
		if _, err := appDB.Exec(`UPDATE series_pins SET show_finished = ? WHERE user_id = ? AND name = ?`, *req.ShowFinished, u.ID, n); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// On Deck
// ---------------------------------------------------------------------------

// onDeck: u's On Deck books (see the top of the file), most recently
// finished series first, at most homeRowMax. active: when each book's
// status last changed (userReading), for the order; nil when it doesn't
// matter.
func onDeck(u *User, m readingMarks, active map[string]int64) ([]Book, error) {
	dismissed := map[string]bool{}
	uuids, err := scanStrings(appDB.Query(`SELECT book_uuid FROM on_deck_dismissed WHERE user_id = ?`, u.ID))
	if err != nil {
		return nil, err
	}
	for _, id := range uuids {
		dismissed[id] = true
	}
	return nextInSeries(currentRecIndex(), m, dismissed, active), nil
}

// nextInSeries works out On Deck from the user's marks.
func nextInSeries(ix *recIndex, m readingMarks, dismissed map[string]bool, active map[string]int64) []Book {
	c := ix.cat
	finished := make(map[string]bool, len(m.Finished))
	furthest := map[int32]int32{} // series -> slot of the furthest finished
	last := map[int32]int64{}     // series -> when a book of it was last finished
	for _, id := range m.Finished {
		finished[id] = true
		i, ok := c.byUUID[id]
		if !ok || c.books[i].Removed || c.books[i].Series == 0 {
			continue
		}
		s := c.books[i].Series
		if f, ok := furthest[s]; !ok || seriesLess(&c.books[f], &c.books[i]) {
			furthest[s] = i
		}
		last[s] = max(last[s], active[id])
	}
	type card struct {
		slot int32
		at   int64
	}
	var cards []card
	for s, f := range furthest {
		for _, slot := range ix.series[s] {
			b := &c.books[slot]
			if !seriesLess(&c.books[f], b) || finished[b.UUID] {
				continue
			}
			// The next one. Being read: it's in Continue reading.
			if _, reading := m.Reading[b.UUID]; !reading && !dismissed[b.UUID] {
				cards = append(cards, card{slot, last[s]})
			}
			break
		}
	}
	slices.SortFunc(cards, func(a, b card) int {
		if a.at != b.at {
			return cmp.Compare(b.at, a.at)
		}
		return cmp.Compare(c.books[a.slot].Sort, c.books[b.slot].Sort)
	})
	out := []Book{}
	for _, d := range cards[:min(len(cards), homeRowMax)] {
		out = append(out, c.books[d.slot].Book)
	}
	return out
}

// recHide: the books Recommended leaves out for u: those they're not
// interested in, and those On Deck has.
func recHide(u *User, m readingMarks) (map[string]bool, error) {
	hide, err := notInterested(u)
	if err != nil {
		return nil, err
	}
	deck, err := onDeck(u, m, nil)
	if err != nil {
		return nil, err
	}
	for _, b := range deck {
		hide[b.UUID] = true
	}
	return hide, nil
}

// DELETE /api/me/on-deck/{uuid}: off On Deck for good.
func onDeckDismissHandler(w http.ResponseWriter, r *http.Request) {
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	if _, err := appDB.Exec(`INSERT OR IGNORE INTO on_deck_dismissed (user_id, book_uuid, created_at) VALUES (?, ?, ?)`,
		currentUser(r).ID, uuid, nowUnix()); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func registerSeriesPinRoutes() {
	http.HandleFunc("PUT /api/me/series-pins/{name}", requireUser(seriesPinHandler))
	http.HandleFunc("DELETE /api/me/series-pins/{name}", requireUser(seriesUnpinHandler))
	http.HandleFunc("PATCH /api/me/series-pins/{name}", requireUser(seriesPinPatchHandler))
	http.HandleFunc("DELETE /api/me/on-deck/{uuid}", requireUser(onDeckDismissHandler))
}
