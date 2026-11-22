package main

// A book in the library edited to be a placeholder's book replaces it: when
// the catalog rereads changed books (catalogFlush: the metadata editor,
// bulk edits, reverts, Calibre's own changes), a placeholder is deleted if
// one of them now has
//   - its title by one of its authors (placeholderTitleMatch),
//   - its ISBN, or
//   - its title, series and number.
// The deletion is in the edit log, so it can be undone. A book just added
// isn't matched here: the add decided (placeholdersToReplace).

import (
	"log"
	"strings"

	"github.com/iotku/CalibreFast/pkg/isbn"
)

// placeholderEditedBook is what a library book is matched against
// placeholders by.
type placeholderEditedBook struct {
	title   string
	authors []string
	series  string
	index   float64
	isbns   map[string]bool // ISBN-13s
}

// placeholderMatchesEdited: b is now p's book.
func placeholderMatchesEdited(p *placeholder, b placeholderEditedBook) bool {
	if placeholderTitleMatch(p, b.title, b.authors) {
		return true
	}
	if n := isbn.Normalize(placeholderISBN(p.Meta)); n != "" && b.isbns[n] {
		return true
	}
	return b.series != "" && strings.EqualFold(strings.TrimSpace(p.Meta.Series), b.series) && p.index() == b.index &&
		hcFullTitleKey(p.Meta.Title) != "" && (hcFullTitleKey(p.Meta.Title) == hcFullTitleKey(b.title) ||
		hcTitleKey(b.title) == hcFullTitleKey(p.Meta.Title) || hcTitleKey(p.Meta.Title) == hcFullTitleKey(b.title))
}

// placeholdersCatalogChanged: next rereads the books in uuids; a
// placeholder any of them now stands for is deleted.
func placeholdersCatalogChanged(next *catalog, uuids []string) {
	if appDB == nil || next == nil || len(uuids) == 0 {
		return
	}
	phs := allPlaceholders()
	if len(phs) == 0 {
		return
	}
	var books []placeholderEditedBook
	var bookUUIDs []string
	byID := map[int64]int{}
	for _, u := range uuids {
		if _, added := placeholdersSettled.LoadAndDelete(u); added {
			continue // just added: the add said what it replaces
		}
		cb, ok := next.lookup(u)
		if !ok || cb.Removed {
			continue
		}
		b := placeholderEditedBook{title: cb.Title, isbns: map[string]bool{}}
		for _, a := range cb.AuthorIDs {
			b.authors = append(b.authors, next.authorNames[a])
		}
		if cb.Series != 0 {
			b.series = strings.TrimSpace(next.seriesNames[cb.Series])
			b.index = cb.SeriesIndex
		}
		byID[cb.ID] = len(books)
		books = append(books, b)
		bookUUIDs = append(bookUUIDs, u)
	}
	if len(books) == 0 {
		return
	}
	if calibreDB != nil {
		ids := make([]any, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		rows, err := calibreDB.Query(`SELECT book, val FROM identifiers WHERE type = 'isbn' AND book IN (?`+
			strings.Repeat(",?", len(ids)-1)+`)`, ids...)
		if err != nil {
			log.Println("placeholders: reading ISBNs:", err)
		} else {
			for rows.Next() {
				var id int64
				var val string
				if rows.Scan(&id, &val) == nil {
					if n := isbn.Normalize(val); n != "" {
						books[byID[id]].isbns[n] = true
					}
				}
			}
			rows.Close()
		}
	}
	for _, p := range phs {
		for i, b := range books {
			if !placeholderMatchesEdited(p, b) {
				continue
			}
			if err := replacePlaceholderLogged(p, bookUUIDs[i], whyEdited, nil); err != nil {
				log.Printf("placeholder %s: %v", p.UUID, err)
			}
			break
		}
	}
}
