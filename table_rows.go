package main

// The bulk editor's rows for the whole library, from the in-memory catalog
// (catalog.go) rather than metadata.db: tens of thousands of rows in a few
// milliseconds, with no reads of the library's storage. The browser asks
// for them in pages, the first few hundred in its sort order and then the
// rest (web/library-edit.ts, loadTable), so the table shows at once and
// fills in (wholeTable). Each answer carries a version: a reload that sends it back
// (?have=) gets {"unchanged": true} instead of every row again when
// nothing changed.
//
// Rows of a few books (?ids=) and the rows the bulk queue starts from are
// still read from metadata.db (tableBooks): the queue needs last_modified
// as stored, which the catalog doesn't keep.

import (
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// tablePageMax: the most rows one ?limit= answers.
const tablePageMax = 20000

// tableView is the catalog's books as table rows, in catalog order, with
// the orders asked for so far.
type tableView struct {
	cat    *catalog
	gen    uint64
	rows   []tableBook
	slots  []int32            // each row's slot in cat.books, ascending
	orders map[string][]int32 // sort spec ("title", "-added") → indexes into rows
}

var tableViews struct {
	sync.Mutex
	cur *tableView
	gen uint64
}

// currentTableView answers the rows of the current catalog, or nil before
// the catalog is built (the caller reads metadata.db then).
func currentTableView() *tableView {
	c := libraryCatalog.Load()
	if c == nil {
		return nil
	}
	tableViews.Lock()
	defer tableViews.Unlock()
	if v := tableViews.cur; v != nil && v.cat == c {
		return v
	}
	tableViews.gen++
	rows, slots := catalogTableBooks(c)
	v := &tableView{cat: c, gen: tableViews.gen, rows: rows, slots: slots, orders: map[string][]int32{}}
	tableViews.cur = v
	return v
}

// catalogTableBooks: the rows of every book not removed, with each one's slot in c.books.
func catalogTableBooks(c *catalog) ([]tableBook, []int32) {
	out := make([]tableBook, 0, len(c.books))
	slots := make([]int32, 0, len(c.books))
	for i := range c.books {
		cb := &c.books[i]
		if cb.Removed {
			continue
		}
		b := tableBook{
			ID: cb.ID, UUID: cb.UUID, Title: cb.Title, Authors: cb.Authors,
			Series: c.seriesNames[cb.Series], SeriesIndex: cb.SeriesIndex,
			Publisher: c.publisherNames[cb.Publisher],
			PubDate:   tableDate(cb.PubDate), Added: tableDate(cb.Timestamp),
			Tags: make([]string, 0, len(cb.TagIDs)), Languages: make([]string, 0, len(cb.LangIDs)),
			HasCover: cb.HasCover, path: cb.Path,
		}
		if cb.Series == 0 {
			b.Series = ""
		}
		if cb.Publisher == 0 {
			b.Publisher = ""
		}
		for _, id := range cb.TagIDs {
			b.Tags = append(b.Tags, c.tagNames[id])
		}
		for _, id := range cb.LangIDs {
			b.Languages = append(b.Languages, c.langCodes[id])
		}
		if len(cb.ISBNs) > 0 {
			b.ISBN = cb.ISBNs[0]
		}
		if len(cb.Formats) > 0 {
			b.Formats = slices.Sorted(slices.Values(cb.Formats))
		} else {
			// The folder's files: read once per book (resolveFormats' cache).
			f := folderFormats(cb.UUID, cb.Path)
			b.Formats = f
			b.FormatsFromFolder = len(f) > 0
		}
		out = append(out, b)
		slots = append(slots, int32(i))
	}
	return out, slots
}

// tableSortKeys: the columns ?sort= may name (web/library-edit.ts COLUMNS).
var tableSortKeys = map[string]func(b *tableBook) string{
	"title":     func(b *tableBook) string { return b.Title },
	"authors":   func(b *tableBook) string { return b.Authors },
	"series":    func(b *tableBook) string { return b.Series },
	"publisher": func(b *tableBook) string { return b.Publisher },
	"pubdate":   func(b *tableBook) string { return b.PubDate },
	"languages": func(b *tableBook) string { return strings.Join(b.Languages, ", ") },
	"tags":      func(b *tableBook) string { return strings.Join(b.Tags, ", ") },
	"formats":   func(b *tableBook) string { return strings.Join(b.Formats, ", ") },
	"added":     func(b *tableBook) string { return b.Added },
}

// tableFold is the browser's fold (library-edit.ts): accents off, lower case.
func tableFold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// order: the rows in this sort ("title", "-added"; anything else is
// title), close to the browser's compare: empty values last either way,
// then title, then id. The browser sorts what it has again, so this only
// has to bring the right books first.
func (v *tableView) order(spec string) []int32 {
	key, desc := strings.CutPrefix(spec, "-")
	text, ok := tableSortKeys[key]
	if !ok {
		key, desc, text = "title", false, tableSortKeys["title"]
	}
	if desc {
		spec = "-" + key
	} else {
		spec = key
	}
	tableViews.Lock()
	defer tableViews.Unlock()
	if o, ok := v.orders[spec]; ok {
		return o
	}
	keys := make([]string, len(v.rows))
	titles := make([]string, len(v.rows))
	for i := range v.rows {
		keys[i] = tableFold(text(&v.rows[i]))
		titles[i] = tableFold(v.rows[i].Title)
	}
	o := make([]int32, len(v.rows))
	for i := range o {
		o[i] = int32(i)
	}
	slices.SortStableFunc(o, func(a, b int32) int {
		x, y := keys[a], keys[b]
		if (x == "") != (y == "") {
			if x == "" {
				return 1
			}
			return -1
		}
		c := strings.Compare(x, y)
		if c == 0 && key == "series" {
			c = cmp.Compare(v.rows[a].SeriesIndex, v.rows[b].SeriesIndex)
		}
		if desc {
			c = -c
		}
		if c != 0 {
			return c
		}
		if c = strings.Compare(titles[a], titles[b]); c != 0 {
			return c
		}
		return cmp.Compare(v.rows[a].ID, v.rows[b].ID)
	})
	v.orders[spec] = o
	return o
}

// tableVersion: what the whole-library answer depends on. A restart, a
// catalog change, a change to the bulk queue's rows or to the books marked
// not duplicates gives a new one.
func tableVersion(v *tableView, notDups []notDuplicateSet) string {
	bulkQueue.mu.Lock()
	rev := bulkQueue.rowsRev
	bulkQueue.mu.Unlock()
	body, _ := json.Marshal(notDups)
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x.%x.%x.%s", serverStarted, v.gen, rev, hex.EncodeToString(sum[:6]))
}

// tableResponse is GET /api/library/table's answer (TS: TableData).
type tableResponse struct {
	Books         []tableBook       `json:"books"`
	Total         int               `json:"total"` // books in the library
	Version       string            `json:"version,omitempty"`
	NotDuplicates []notDuplicateSet `json:"not_duplicates"`
	// Next: the ?from= that answers the rows after these; 0 when there are none.
	Next      int  `json:"next,omitempty"`
	Unchanged bool `json:"unchanged,omitempty"`
}

// wholeTable answers the library's rows from the catalog, false before
// it's built. Each answer has the books marked not duplicates.
//
//   - no parameters: every row (the imports queue's shape; old pages).
//   - ?sort=&limit=: the first rows in that order (tableView.order), to
//     show at once.
//   - ?from=&limit=: the rest, in catalog order from that slot on, with
//     Next. Catalog slots don't move as books are edited, added or removed
//     (catalog.go), so the pages cover every book once however the library
//     changes in between; the browser keeps one row per uuid, the last it
//     was sent.
//   - ?have=<version>: just {"unchanged": true} if that's still the version.
func wholeTable(w http.ResponseWriter, r *http.Request) bool {
	v := currentTableView()
	if v == nil {
		return false
	}
	q := r.URL.Query()
	notDups, err := notDuplicateSets()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	res := tableResponse{Total: len(v.rows), Version: tableVersion(v, notDups), NotDuplicates: notDups}
	if have := q.Get("have"); have != "" && have == res.Version {
		writeTableJSON(w, r, tableResponse{Books: []tableBook{}, Total: res.Total, Version: res.Version, Unchanged: true})
		return true
	}
	limit := len(v.rows)
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			http.Error(w, "bad limit", http.StatusBadRequest)
			return true
		}
		limit = min(limit, n, tablePageMax)
	}
	var books []tableBook
	if s := q.Get("from"); s != "" {
		from, err := strconv.Atoi(s)
		if err != nil || from < 0 {
			http.Error(w, "bad from", http.StatusBadRequest)
			return true
		}
		at, _ := slices.BinarySearch(v.slots, int32(min(from, math.MaxInt32)))
		end := min(at+limit, len(v.rows))
		books = slices.Clone(v.rows[at:end])
		if end < len(v.rows) {
			res.Next = int(v.slots[end])
		}
	} else {
		order := v.order(q.Get("sort"))
		books = make([]tableBook, 0, limit)
		for _, i := range order[:limit] {
			books = append(books, v.rows[i])
		}
	}
	pendingBulkRows(books)
	res.Books = books
	writeTableJSON(w, r, res)
	return true
}

// writeTableJSON sends v, gzipped when the browser takes it: the rows are
// repetitive and shrink to a tenth or so.
func writeTableJSON(w http.ResponseWriter, r *http.Request, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Add("Vary", "Accept-Encoding")
	if !acceptsGzip(r) {
		logErr(json.NewEncoder(w).Encode(v), "encode library table")
		return
	}
	h.Set("Content-Encoding", "gzip")
	zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	logErr(json.NewEncoder(zw).Encode(v), "encode library table")
	logErr(zw.Close(), "gzip library table")
}
