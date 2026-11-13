package main

// The catalog: every book's grid entry and the ids of its tags, authors and
// series, held in memory so pages can be served without reading the library
// (metadata.db and the book folders may be on slow or network storage;
// docs/notes/Discover.md, "Storage model").
//
//   - Built at startup from a few sequential reads of metadata.db
//     (writeLibraryPages), in Latest order (books.timestamp DESC).
//   - Kept current by the write paths: catalogChanged / catalogRemoved re-read
//     only the books named, with indexed queries, and swap in an updated copy.
//     Readers never lock: they take the current *catalog and read it.
//   - Removed books stay as tombstones until the next full build, as the page
//     files keep their gaps (library_pages.go).

import (
	"cmp"
	"database/sql"
	"log"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type catalogBook struct {
	Book             // the grid entry, as in pages/*.json
	ID        int64  // books.id
	Timestamp string // books.timestamp: when it was added (Latest order)
	TagIDs    []int32
	AuthorIDs []int32
	Series    int32    // 0 if none
	HasCover  bool     // books.has_cover
	Formats   []string // lower case, from the data table ("epub", "pdf", …)
	Removed   bool     // recycled since the last full build
}

type catalog struct {
	books       []catalogBook // full build: Latest order; later additions at the end
	byUUID      map[string]int32
	tagNames    map[int32]string
	authorNames map[int32]string
	seriesNames map[int32]string
	// shuffle: every slot of books in a random order, made at each start
	// (Shuffle, docs/notes/Discover.md section 2). Books added while running
	// go at the end, so the shuffle page files before them don't change.
	shuffle []int32
}

var libraryCatalog atomic.Pointer[catalog]

func newCatalog() *catalog {
	return &catalog{
		byUUID:      map[string]int32{},
		tagNames:    map[int32]string{},
		authorNames: map[int32]string{},
		seriesNames: map[int32]string{},
	}
}

// currentCatalog never returns nil (an empty catalog before the first build).
func currentCatalog() *catalog {
	if c := libraryCatalog.Load(); c != nil {
		return c
	}
	return newCatalog()
}

// lookup returns the book with this uuid, unless it isn't known or was removed.
// The result shares its slices with the catalog: don't modify them.
func (c *catalog) lookup(uuid string) (catalogBook, bool) {
	i, ok := c.byUUID[uuid]
	if !ok || c.books[i].Removed {
		return catalogBook{}, false
	}
	return c.books[i], true
}

// latest lists the books not removed, newest added first (as
// ORDER BY timestamp DESC, id DESC: books added since the full build were
// appended at the end).
func (c *catalog) latest() []int32 {
	order := make([]int32, 0, len(c.books))
	for i := range c.books {
		if !c.books[i].Removed {
			order = append(order, int32(i))
		}
	}
	slices.SortStableFunc(order, func(a, b int32) int {
		x, y := &c.books[a], &c.books[b]
		if n := strings.Compare(y.Timestamp, x.Timestamp); n != 0 {
			return n
		}
		return cmp.Compare(y.ID, x.ID)
	})
	return order
}

// randomOrder is the slots from..to-1 in a random order (Fisher–Yates).
func randomOrder(from, to int) []int32 {
	order := make([]int32, 0, to-from)
	for i := from; i < to; i++ {
		order = append(order, int32(i))
	}
	rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order
}

// live counts the books not removed.
func (c *catalog) live() int {
	n := 0
	for i := range c.books {
		if !c.books[i].Removed {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Reading from metadata.db
// ---------------------------------------------------------------------------

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// loadCatalogBooks reads books and their links. uuids nil: every book, in
// Latest order, each table in one sequential pass. Otherwise only those
// books (by books.id), through the link tables' (book, x) indexes.
func loadCatalogBooks(db queryer, uuids []string, names *catalog) ([]catalogBook, error) {
	q := `SELECT b.id, b.timestamp, b.title, b.series_index, b.pubdate, b.path, b.uuid, b.last_modified, COALESCE(b.has_cover, 0)
		FROM books b`
	var args []any
	if uuids != nil {
		q += ` WHERE b.uuid IN (` + placeholders(len(uuids)) + `)`
		args = toArgs(uuids)
	}
	q += ` ORDER BY b.timestamp DESC, b.id DESC`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	var books []catalogBook
	at := map[int64]int{}
	for rows.Next() {
		var b catalogBook
		if err := rows.Scan(&b.ID, &b.Timestamp, &b.Title, &b.SeriesIndex, &b.PubDate, &b.Path, &b.UUID, &b.LastModified, &b.HasCover); err != nil {
			rows.Close()
			return nil, err
		}
		b.LastModified = coverStamp(b.LastModified)
		at[b.ID] = len(books)
		books = append(books, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(books) == 0 {
		return books, nil
	}

	var where string
	if uuids != nil {
		ids := make([]any, 0, len(books))
		for _, b := range books {
			ids = append(ids, b.ID)
		}
		where = ` WHERE l.book IN (` + placeholders(len(ids)) + `)`
		args = ids
	} else {
		args = nil
	}

	// Authors: also the grid's "A & B" (bookAuthorsSQL's order: the link's id).
	authorList := map[int64][]string{}
	err = scanLinks(db, `SELECT l.book, a.id, a.name FROM books_authors_link l JOIN authors a ON a.id = l.author`+where+` ORDER BY l.id`, args,
		func(book int64, id int32, name string) {
			if i, ok := at[book]; ok {
				books[i].AuthorIDs = append(books[i].AuthorIDs, id)
				authorList[book] = append(authorList[book], name)
				names.authorNames[id] = name
			}
		})
	if err != nil {
		return nil, err
	}
	for i := range books {
		books[i].AuthorIDs = slices.Clip(books[i].AuthorIDs)
		books[i].Book.Authors = strings.Join(authorList[books[i].ID], " & ")
	}
	err = scanLinks(db, `SELECT l.book, t.id, t.name FROM books_tags_link l JOIN tags t ON t.id = l.tag`+where+` ORDER BY l.id`, args,
		func(book int64, id int32, name string) {
			if i, ok := at[book]; ok {
				books[i].TagIDs = append(books[i].TagIDs, id)
				names.tagNames[id] = name
			}
		})
	if err != nil {
		return nil, err
	}
	err = scanLinks(db, `SELECT l.book, s.id, s.name FROM books_series_link l JOIN series s ON s.id = l.series`+where, args,
		func(book int64, id int32, name string) {
			if i, ok := at[book]; ok {
				books[i].Series = id
				names.seriesNames[id] = name
			}
		})
	if err != nil {
		return nil, err
	}
	err = scanLinks(db, `SELECT l.book, 0, lower(l.format) FROM data l`+where, args,
		func(book int64, _ int32, format string) {
			if i, ok := at[book]; ok && !slices.Contains(books[i].Formats, format) {
				books[i].Formats = append(books[i].Formats, format)
			}
		})
	if err != nil {
		return nil, err
	}
	for i := range books {
		books[i].TagIDs = slices.Clip(books[i].TagIDs)
	}
	return books, nil
}

func scanLinks(db queryer, q string, args []any, add func(book int64, id int32, name string)) error {
	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var book int64
		var id int32
		var name string
		if err := rows.Scan(&book, &id, &name); err != nil {
			return err
		}
		add(book, id, name)
	}
	return rows.Err()
}

// catalogBuilds counts full reads of the library (for tests).
var catalogBuilds atomic.Int64

// buildCatalog reads every book.
func buildCatalog(db queryer) (*catalog, error) {
	catalogBuilds.Add(1)
	start := time.Now()
	c := newCatalog()
	books, err := loadCatalogBooks(db, nil, c)
	if err != nil {
		return nil, err
	}
	c.books = books
	for i := range books {
		c.byUUID[books[i].UUID] = int32(i)
	}
	c.shuffle = randomOrder(0, len(books))
	log.Printf("catalog: %d books, %d tags, %d authors, %d series in %s",
		len(books), len(c.tagNames), len(c.authorNames), len(c.seriesNames), time.Since(start).Round(time.Millisecond))
	return c, nil
}

// ---------------------------------------------------------------------------
// Changes
// ---------------------------------------------------------------------------

// Changes are queued and applied together by one goroutine, so a bulk edit's
// batches cost one copy of the catalog.
var catalogQueue struct {
	sync.Mutex
	ops       map[string]bool // uuid -> true: re-read, false: removed
	scheduled bool
}

// catalogApplyMu serializes applying changes (the queue's goroutine, and
// catalogFlush from tests and full builds).
var catalogApplyMu sync.Mutex

const catalogCoalesce = 250 * time.Millisecond

// catalogChanged re-reads these books from metadata.db (added, edited, a new
// cover, restored from the Recycling Bin). A uuid no longer in the library
// is removed.
func catalogChanged(uuids ...string) { catalogEnqueue(uuids, true) }

// catalogRemoved marks these books as gone (recycled).
func catalogRemoved(uuids ...string) { catalogEnqueue(uuids, false) }

func catalogEnqueue(uuids []string, reread bool) {
	if len(uuids) == 0 {
		return
	}
	for _, u := range uuids {
		noCover.Delete(u)
	}
	if libraryCatalog.Load() == nil {
		return // not built yet: the build reads everything
	}
	catalogQueue.Lock()
	defer catalogQueue.Unlock()
	if catalogQueue.ops == nil {
		catalogQueue.ops = map[string]bool{}
	}
	for _, u := range uuids {
		if u != "" {
			catalogQueue.ops[u] = reread
		}
	}
	if !catalogQueue.scheduled {
		catalogQueue.scheduled = true
		time.AfterFunc(catalogCoalesce, func() { logErr(catalogFlush(), "catalog: couldn't apply changes") })
	}
}

// catalogFlush applies the queued changes now.
func catalogFlush() error {
	catalogApplyMu.Lock()
	defer catalogApplyMu.Unlock()
	catalogQueue.Lock()
	ops := catalogQueue.ops
	catalogQueue.ops = nil
	catalogQueue.scheduled = false
	catalogQueue.Unlock()
	old := libraryCatalog.Load()
	if len(ops) == 0 || old == nil || calibreDB == nil {
		return nil // nothing to apply, or no catalog built (yet): the build reads everything
	}
	var reread, removed []string
	for u, r := range ops {
		if r {
			reread = append(reread, u)
		} else {
			removed = append(removed, u)
		}
	}
	next, err := applyCatalogChanges(calibreDB, old, reread, removed)
	if err != nil {
		// Put them back for the next change to retry.
		catalogQueue.Lock()
		if catalogQueue.ops == nil {
			catalogQueue.ops = map[string]bool{}
		}
		for u, r := range ops {
			if _, newer := catalogQueue.ops[u]; !newer {
				catalogQueue.ops[u] = r
			}
		}
		catalogQueue.Unlock()
		return err
	}
	libraryCatalog.Store(next)
	scheduleCatalogSnapshot()
	refreshFeatured() // a title or cover on a featured shelf
	refreshTagRows()  // the tags' counts
	return nil
}

// applyCatalogChanges returns a copy of old with the books in reread read
// again and those in removed marked removed. old isn't modified.
func applyCatalogChanges(db queryer, old *catalog, reread, removed []string) (*catalog, error) {
	c := &catalog{
		books:       slices.Clone(old.books),
		byUUID:      maps.Clone(old.byUUID),
		tagNames:    old.tagNames,
		authorNames: old.authorNames,
		seriesNames: old.seriesNames,
		shuffle:     old.shuffle,
	}
	names := newCatalog()
	found := map[string]bool{}
	for chunk := range slices.Chunk(reread, 500) {
		books, err := loadCatalogBooks(db, chunk, names)
		if err != nil {
			return nil, err
		}
		for _, b := range books {
			found[b.UUID] = true
			if i, ok := c.byUUID[b.UUID]; ok {
				c.books[i] = b
			} else {
				c.byUUID[b.UUID] = int32(len(c.books))
				c.books = append(c.books, b)
			}
		}
	}
	if n := len(c.books); n > len(old.books) {
		c.shuffle = append(slices.Clone(old.shuffle), randomOrder(len(old.books), n)...)
	}
	for _, u := range reread {
		if !found[u] {
			removed = append(removed, u) // gone from metadata.db
		}
	}
	for _, u := range removed {
		if i, ok := c.byUUID[u]; ok {
			c.books[i].Removed = true
		}
	}
	// New names (a new tag, author or series): copy the maps only then.
	merge := func(dst *map[int32]string, src map[int32]string) {
		for id, name := range src {
			if (*dst)[id] != name {
				m := maps.Clone(*dst)
				maps.Copy(m, src)
				*dst = m
				return
			}
		}
	}
	merge(&c.tagNames, names.tagNames)
	merge(&c.authorNames, names.authorNames)
	merge(&c.seriesNames, names.seriesNames)
	return c, nil
}
