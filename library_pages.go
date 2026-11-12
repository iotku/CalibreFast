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
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
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
	page := max(1, (totalBooks+booksPerPage-1)/booksPerPage)
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
	totalBooks = max(totalBooks, (page-1)*booksPerPage+len(books))
	return nil
}

// refreshInPages rewrites the books' entries in the page files that hold
// them, after their metadata changed (bulk edits, merges), so the grid
// shows the new titles and authors before the next full rewrite.
func refreshInPages(uuids []string) error {
	pagesMu.Lock()
	defer pagesMu.Unlock()
	pages := map[int]bool{}
	wanted := map[string]bool{}
	for _, u := range uuids {
		if v, ok := bookPages.Load(u); ok {
			wanted[u] = true
			for _, p := range v.([]int) {
				pages[p] = true
			}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	fresh := map[string]Book{}
	list := make([]string, 0, len(wanted))
	for u := range wanted {
		list = append(list, u)
	}
	for chunk := range slices.Chunk(list, 500) {
		rows, err := calibreDB.Query(`SELECT b.title, `+bookAuthorsSQL+`, b.series_index, b.pubdate, b.path, b.uuid, b.last_modified
			FROM books b WHERE b.uuid IN (`+placeholders(len(chunk))+`)`, toArgs(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var b Book
			if err := rows.Scan(&b.Title, &b.Authors, &b.SeriesIndex, &b.PubDate, &b.Path, &b.UUID, &b.LastModified); err != nil {
				rows.Close()
				return err
			}
			b.LastModified = coverStamp(b.LastModified)
			fresh[b.UUID] = b
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
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
		edited := false
		for i, b := range books {
			if nb, ok := fresh[b.UUID]; ok && nb != b {
				books[i] = nb
				edited = true
			}
		}
		if !edited {
			continue
		}
		if err := writePage(page, books); err != nil {
			errs = append(errs, err)
			continue
		}
		changed = true
	}
	if changed {
		pagesVersion.Store(time.Now().UnixNano())
	}
	return errors.Join(errs...)
}
