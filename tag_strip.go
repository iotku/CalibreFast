package main

// A tag's books a strip at a time, in the shuffle order (docs/notes/Discover.md,
// section 3): what the Discover page's tag rows show and walk with their
// Shuffle and Previous buttons, and where a pinned tag's row starts.
//
//   - The order is the catalog's shuffle order, kept to the tag's books (as
//     browsed: a group stands for its members). It wraps at the end, so
//     walking a strip at a time shows every book of the tag before any
//     comes back.
//   - GET /api/tag-strip?tag=X&start=K: the strip from K (at random without
//     K), and where the next one starts. Signed in, the books the user
//     finished are left out. Behind the catalog's own guard, like
//     /api/books. Nothing is read from the library.

import (
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const tagStripMax = 24 // books in a strip, at most

// tagIndex: every tag's books (lower-case name as browsed -> book slots), in
// the shuffle order. Made once per catalog and groups.
type tagIndex struct {
	cat       *catalog
	groupsVer uint64
	books     map[string][]int32
}

var (
	tagIndexMu  sync.Mutex
	tagIndexNow atomic.Pointer[tagIndex]
)

func currentTagIndex() *tagIndex {
	c := currentCatalog()
	gv := groups.currentVersion()
	if ix := tagIndexNow.Load(); ix != nil && ix.cat == c && ix.groupsVer == gv {
		return ix
	}
	tagIndexMu.Lock()
	defer tagIndexMu.Unlock()
	if ix := tagIndexNow.Load(); ix != nil && ix.cat == c && ix.groupsVer == gv {
		return ix
	}
	keyOf := make(map[int32]string, len(c.tagNames))
	for id, name := range c.tagNames {
		keyOf[id] = strings.ToLower(tagEntity(name))
	}
	ix := &tagIndex{cat: c, groupsVer: gv, books: map[string][]int32{}}
	var seen []string // the book's tags so far: a group's members count once
	for _, slot := range c.shuffle {
		b := &c.books[slot]
		if b.Removed {
			continue
		}
		seen = seen[:0]
		for _, t := range b.TagIDs {
			key, ok := keyOf[t]
			if !ok || slices.Contains(seen, key) {
				continue
			}
			seen = append(seen, key)
			ix.books[key] = append(ix.books[key], slot)
		}
	}
	tagIndexNow.Store(ix)
	return ix
}

// count: how many books the tag (as browsed) has.
func (ix *tagIndex) count(name string) int {
	return len(ix.books[strings.ToLower(name)])
}

// strip: up to n of the tag's books from start (taken mod the count),
// wrapping, leaving out those skip says to; and where the next strip starts.
func (ix *tagIndex) strip(name string, start, n int, skip func(uuid string) bool) (books []Book, next int) {
	list := ix.books[strings.ToLower(name)]
	books = []Book{}
	if len(list) == 0 {
		return books, 0
	}
	start = ((start % len(list)) + len(list)) % len(list)
	i := start
	for scanned := 0; len(books) < n && scanned < len(list); scanned++ {
		b := &ix.cat.books[list[i]]
		if skip == nil || !skip(b.UUID) {
			books = append(books, b.Book)
		}
		i = (i + 1) % len(list)
	}
	// Went all the way round (a tag smaller than a strip): start the next
	// one further on anyway, so it isn't the same books in the same order.
	if i == start {
		i = (start + max(1, len(books)/2)) % len(list)
	}
	return books, i
}

type tagStripAnswer struct {
	Books []Book `json:"books"`
	Start int    `json:"start"`
	Next  int    `json:"next"`
	Count int    `json:"count"`
}

func tagStripHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("tag"))
	n, err := strconv.Atoi(q.Get("n"))
	if err != nil || n <= 0 || n > tagStripMax {
		n = tagStripMax
	}
	if name == "" || len(name) > maxDiscoverTagNameLen {
		http.Error(w, "invalid tag", http.StatusBadRequest)
		return
	}
	ix := currentTagIndex()
	name = tagEntity(name)
	count := ix.count(name)
	if count == 0 {
		http.Error(w, "no such tag", http.StatusNotFound)
		return
	}
	start, err := strconv.Atoi(q.Get("start"))
	if err != nil || start < 0 {
		start = rand.IntN(count)
	}
	start %= count
	var skip func(string) bool
	if u := currentUser(r); u != nil && appDB != nil {
		uuids, err := scanStrings(appDB.Query(`SELECT book_uuid FROM reading_status WHERE user_id = ? AND status = 'finished'`, u.ID))
		if err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		finished := make(map[string]bool, len(uuids))
		for _, id := range uuids {
			finished[id] = true
		}
		skip = func(uuid string) bool { return finished[uuid] }
	}
	books, next := ix.strip(name, start, n, skip)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, tagStripAnswer{Books: books, Start: start, Next: next, Count: count})
}
