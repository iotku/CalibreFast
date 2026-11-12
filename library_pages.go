package main

// Small edits to pages/*.json when books leave or come back (recycle.go),
// instead of writing every page again. Only the pages holding those books
// are rewritten; the full order is restored by the next writeLibraryPages
// (at startup).
//
// A removed book leaves a gap at the end of its page: the grid's count
// (totalBooks) stays the same, so no other page's books shift, and the grid
// shows the missing slot as empty (MISSING in web/app-htmx.ts).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// pagesMu serializes everything that writes pages/*.json.
	pagesMu sync.Mutex
	// bookPages: uuid -> the page files holding it ([]int). After an upload a
	// book can be on page 1 and still on the page it was on before.
	bookPages sync.Map
	// pageSlots: uuid -> where a book was removed from in this run
	// ([]pageSlot), so restoring puts it back in the same place.
	pageSlots sync.Map
)

type pageSlot struct {
	Page, Pos int
	Book      Book
}

func pageFile(page int) string {
	return filepath.Join("pages", "page"+strconv.Itoa(page)+".json")
}

func readPage(page int) ([]Book, error) {
	data, err := os.ReadFile(pageFile(page))
	if err != nil {
		return nil, err
	}
	var books []Book
	return books, json.Unmarshal(data, &books)
}

// notePage records that a page file holds a book. Callers hold pagesMu.
func notePage(uuid string, page int) {
	var pages []int
	if v, ok := bookPages.Load(uuid); ok {
		pages = v.([]int)
	}
	if !slices.Contains(pages, page) {
		bookPages.Store(uuid, append(slices.Clone(pages), page))
	}
}

// removeFromPages takes books out of the page files that hold them.
func removeFromPages(uuids []string) error {
	pagesMu.Lock()
	defer pagesMu.Unlock()
	gone := map[string]bool{}
	pages := map[int]bool{}
	for _, u := range uuids {
		gone[u] = true
		if v, ok := bookPages.Load(u); ok {
			for _, p := range v.([]int) {
				pages[p] = true
			}
		}
	}
	var errs []error
	changed := false
	for page := range pages {
		books, err := readPage(page)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		kept := books[:0:0]
		for i, b := range books {
			if !gone[b.UUID] {
				kept = append(kept, b)
				continue
			}
			var slots []pageSlot
			if v, ok := pageSlots.Load(b.UUID); ok {
				slots = v.([]pageSlot)
			}
			pageSlots.Store(b.UUID, append(slices.Clone(slots), pageSlot{page, i, b}))
		}
		if len(kept) == len(books) {
			continue
		}
		if err := writePage(page, kept); err != nil {
			errs = append(errs, err)
			continue
		}
		changed = true
	}
	for _, u := range uuids {
		bookPages.Delete(u)
	}
	if changed {
		pagesVersion.Store(time.Now().UnixNano())
	}
	return errors.Join(errs...)
}

// restoreToPages puts a book that came back into the pages: where it was
// removed from in this run, or else (removed before a restart) at the end
// of the last page, until the next full rewrite puts it in order.
func restoreToPages(uuid string) error {
	pagesMu.Lock()
	defer pagesMu.Unlock()
	defer pagesVersion.Store(time.Now().UnixNano())

	if v, ok := pageSlots.LoadAndDelete(uuid); ok {
		placed := false
		for _, s := range v.([]pageSlot) {
			books, err := readPage(s.Page)
			if err != nil || len(books) >= booksPerPage {
				continue // rewritten since and full: the book goes at the end
			}
			if !slices.ContainsFunc(books, func(b Book) bool { return b.UUID == uuid }) {
				books = slices.Insert(books, min(s.Pos, len(books)), s.Book)
				if err := writePage(s.Page, books); err != nil {
					return err
				}
			}
			notePage(uuid, s.Page)
			placed = true
		}
		if placed {
			return nil
		}
	}

	var b Book
	if err := calibreDB.QueryRow(`SELECT b.title, `+bookAuthorsSQL+`, b.series_index, b.pubdate, b.path, b.uuid, b.last_modified
		FROM books b WHERE b.uuid = ?`, uuid).Scan(&b.Title, &b.Authors, &b.SeriesIndex, &b.PubDate, &b.Path, &b.UUID, &b.LastModified); err != nil {
		return err
	}
	b.LastModified = coverStamp(b.LastModified)
	total := int(totalBooks.Load())
	page := max(1, (total+booksPerPage-1)/booksPerPage)
	books, err := readPage(page)
	if errors.Is(err, fs.ErrNotExist) {
		books, err = nil, nil
	}
	if err != nil {
		return err
	}
	if len(books) >= booksPerPage {
		page, books = page+1, nil
	}
	books = append(books, b)
	if err := writePage(page, books); err != nil {
		return err
	}
	notePage(uuid, page)
	totalBooks.Store(int64(max(total, (page-1)*booksPerPage+len(books))))
	return nil
}

// shuffleTotal is how many books pages/shuffle/*.json hold.
var shuffleTotal atomic.Int64

func shufflePageFile(page int) string {
	return filepath.Join("pages", "shuffle", "page"+strconv.Itoa(page)+".json")
}

// writeShufflePages writes pages/shuffle/pageN.json: the books not removed,
// in the catalog's random order, booksPerPage per page. Like the library
// pages, only files whose contents change are written: an edit rewrites the
// page holding the book, an upload only the last page (new books go at the
// end of the order). Sets shuffleTotal; doesn't bump pagesVersion (callers
// do). Callers hold pagesMu.
func writeShufflePages(c *catalog) (changed bool, err error) {
	var books []Book
	page := 1
	flush := func() error {
		data, err := json.Marshal(books)
		if err != nil {
			return err
		}
		path := shufflePageFile(page)
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			return nil
		}
		if err := writeJSONFile(path, data); err != nil {
			return err
		}
		changed = true
		return nil
	}
	n := 0
	for _, i := range c.shuffle {
		b := &c.books[i]
		if b.Removed {
			continue
		}
		books = append(books, b.Book)
		n++
		if len(books) == booksPerPage {
			if err := flush(); err != nil {
				return changed, err
			}
			page++
			books = books[:0]
		}
	}
	if len(books) > 0 {
		if err := flush(); err != nil {
			return changed, err
		}
	}
	if shuffleTotal.Swap(int64(n)) != int64(n) {
		changed = true
	}
	return changed, nil
}

// refreshShufflePages brings pages/shuffle/ up to date after books were
// removed or restored (the library pages keep gaps then, but a random order
// has no place to keep: the files after a removed book shift).
func refreshShufflePages() {
	if libraryCatalog.Load() == nil {
		return
	}
	logErr(catalogFlush(), "shuffle pages: couldn't apply the catalog's changes")
	pagesMu.Lock()
	defer pagesMu.Unlock()
	changed, err := writeShufflePages(currentCatalog())
	logErr(err, "shuffle pages: couldn't write")
	if changed {
		pagesVersion.Store(time.Now().UnixNano())
	}
}

// writePagesFromCatalog writes pages/*.json from the catalog: every book
// not removed, newest added first, booksPerPage per page. Only files whose
// contents change are written, so after an edit only the pages holding the
// edited books are, and the others stay cached in browsers. Sets totalBooks,
// and pagesVersion when anything was written. Callers hold pagesMu.
func writePagesFromCatalog(c *catalog) (changed bool, err error) {
	bookPages.Clear()
	pageSlots.Clear()
	order := c.latest()
	for start := 0; start < len(order); start += booksPerPage {
		page := start/booksPerPage + 1
		books := make([]Book, 0, booksPerPage)
		for _, i := range order[start:min(start+booksPerPage, len(order))] {
			b := c.books[i].Book
			modTimeCache.Store(b.UUID, b.LastModified)
			uuidPathIndex.Store(b.UUID, b.Path)
			notePage(b.UUID, page)
			books = append(books, b)
		}
		data, err := json.Marshal(books)
		if err != nil {
			return changed, err
		}
		if old, err := os.ReadFile(pageFile(page)); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := writePageJSON(page, data); err != nil {
			return changed, err
		}
		changed = true
	}
	if total := int64(len(order)); totalBooks.Swap(total) != total {
		changed = true
	}
	shuffled, err := writeShufflePages(c)
	if err != nil {
		return changed, err
	}
	changed = changed || shuffled
	if changed {
		pagesVersion.Store(time.Now().UnixNano())
	}
	return changed, nil
}
