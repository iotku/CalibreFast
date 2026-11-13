package main

// GET /api/me/home: what the Discover page shows a signed-in user that's
// theirs (docs/notes/Discover.md, section 4), in one request:
//   - marks: the same as /api/me/reading, for every card on the page;
//   - reading: the books being read, most recently active first (the newer
//     of the status change and the last position, from any device);
//   - wanted: want to read, most recently added first;
// each capped, with the full counts for "See all". Book details come from
// the catalog: nothing is read from the library.
//
// GET /api/books?uuids=a,b,…: grid entries for up to maxBookLookup books
// (the Discover page's "on this device" row for signed-out visitors).

import (
	"net/http"
	"slices"
	"strings"
)

const (
	homeRowMax    = 20
	maxBookLookup = 50
)

type homeAnswer struct {
	Marks        readingMarks `json:"marks"`
	Reading      []Book       `json:"reading"`
	ReadingCount int          `json:"reading_count"`
	Wanted       []Book       `json:"wanted"`
	WantedCount  int          `json:"wanted_count"`
	// The shelf rows: pinned ones, and featured ones not unpinned
	// (shelf_pins.go).
	Shelves []homeShelf `json:"shelves"`
	// Recommended for you, and the user's strongest tags (recommend.go).
	Recommended []recBook `json:"recommended"`
	Tags        []string  `json:"tags"`
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	marks, active, err := userReading(currentUser(r))
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	newestFirst := func(uuids []string) []string {
		out := slices.Clone(uuids)
		slices.SortStableFunc(out, func(a, b string) int {
			if active[a] != active[b] {
				if active[a] > active[b] {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		return out
	}
	reading := make([]string, 0, len(marks.Reading))
	for uuid := range marks.Reading {
		reading = append(reading, uuid)
	}
	reading, wanted := newestFirst(reading), newestFirst(marks.Wanted)

	ans := homeAnswer{Marks: marks, ReadingCount: len(reading), WantedCount: len(wanted)}
	if ans.Reading, err = booksByUUIDs(reading[:min(len(reading), homeRowMax)]); err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if ans.Wanted, err = booksByUUIDs(wanted[:min(len(wanted), homeRowMax)]); err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if ans.Recommended, ans.Tags, err = userRecommend(currentUser(r), marks); err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	if ans.Shelves, err = pinnedShelves(currentUser(r), shelfRowsMax); err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, ans)
}

// booksHandler answers grid entries for the books named, in that order,
// skipping any the catalog doesn't have. Behind the catalog's own guard (withAuth:
// sign-in unless -public), like the page files it stands in for.
func booksHandler(w http.ResponseWriter, r *http.Request) {
	var uuids []string
	for _, u := range strings.Split(r.URL.Query().Get("uuids"), ",") {
		if u = strings.TrimSpace(u); u != "" && !slices.Contains(uuids, u) {
			uuids = append(uuids, u)
		}
	}
	if len(uuids) > maxBookLookup {
		http.Error(w, "too many books", http.StatusBadRequest)
		return
	}
	// From the catalog only: anyone who can browse may ask, and an unknown
	// id shouldn't cost a read of the library.
	c := currentCatalog()
	books := make([]Book, 0, len(uuids))
	for _, u := range uuids {
		if b, ok := c.lookup(u); ok {
			books = append(books, b.Book)
		}
	}
	w.Header().Set("Cache-Control", "private, max-age=60")
	writeJSON(w, http.StatusOK, books)
}
