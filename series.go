package main

// Series: shown on a book's page, browsed at /series/<name> (in series
// order), and edited in the metadata editor (single book, import queue,
// merge). In metadata.db a book has at most one series (books_series_link)
// and a number in it (books.series_index); in its metadata.opf they're the
// calibre:series and calibre:series_index meta, as Calibre writes them.
// "Next in series" (recommend.go) depends on both being right.

import (
	"cmp"
	"database/sql"
	"slices"
	"strconv"
	"strings"
)

// setBookSeries replaces a book's series in metadata.db: its link and
// books.series_index (1 without a series, as Calibre).
func setBookSeries(tx *sql.Tx, bookID int64, name string, index float64) error {
	if _, err := tx.Exec(`DELETE FROM books_series_link WHERE book = ?`, bookID); err != nil {
		return err
	}
	if name != "" {
		id, err := upsertSimpleEntity(tx, "series", name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO books_series_link (book, series) VALUES (?, ?)`, bookID, id); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE books SET series_index = ? WHERE id = ?`, index, bookID)
	return err
}

// librarySeries: a library book's series as metadata.db has it (from the
// catalog), for the editor's "current" values and the book page.
func librarySeries(uuid string) (name string, index float64, ok bool) {
	c := currentCatalog()
	b, found := c.lookup(uuid)
	if !found || b.Series == 0 {
		return "", 0, false
	}
	return c.seriesNames[b.Series], b.SeriesIndex, true
}

// librarySeriesFields: librarySeries as the editor's fields ("" without one).
func librarySeriesFields(uuid string) (name, index string) {
	if n, i, ok := librarySeries(uuid); ok {
		return n, strconv.FormatFloat(i, 'f', -1, 64)
	}
	return "", ""
}

// The order a series reads in, everywhere it's listed (its page, a pinned
// series on Home, On Deck, Next in series): by number; books with the same
// number (a series numbered all 1, or not numbered at all) oldest
// publication first, an unknown date after the known ones; then by title
// as sorted. seriesOrderSQL is the same for metadata.db (alias b).
const seriesOrderSQL = `b.series_index, (b.pubdate IS NULL OR substr(b.pubdate, 1, 10) < '0102'), substr(b.pubdate, 1, 10), b.sort, b.id`

// seriesDate: a pubdate's day ("2005-06-01"), "" when unknown (Calibre's
// "undefined" date is in the year 101).
func seriesDate(pubdate string) string {
	if len(pubdate) < 10 || pubdate < "0102" {
		return ""
	}
	return pubdate[:10]
}

// seriesCompare orders two books of a series (see seriesOrderSQL).
func seriesCompare(a, b *catalogBook) int {
	if c := cmp.Compare(a.SeriesIndex, b.SeriesIndex); c != 0 {
		return c
	}
	da, db := seriesDate(a.PubDate), seriesDate(b.PubDate)
	if (da == "") != (db == "") {
		if da == "" {
			return 1
		}
		return -1
	}
	if c := strings.Compare(da, db); c != 0 {
		return c
	}
	if c := strings.Compare(a.Sort, b.Sort); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// seriesLess: whether a comes before b in their series.
func seriesLess(a, b *catalogBook) bool { return seriesCompare(a, b) < 0 }

// sortSeries puts slots of c in series order.
func sortSeries(c *catalog, slots []int32) {
	slices.SortStableFunc(slots, func(a, b int32) int { return seriesCompare(&c.books[a], &c.books[b]) })
}

// seriesEntity: a series as browsed (its group's name, when it's in one).
func seriesEntity(name string) string {
	n, _, _ := groups.resolve("series", name)
	return n
}

// seriesBooks: the books of a series as browsed (a group: every member's),
// in series order, from the catalog.
func seriesBooks(ix *recIndex, name string) []int32 {
	_, members, _ := groups.resolve("series", name)
	want := map[string]bool{}
	for _, m := range members {
		want[strings.ToLower(m)] = true
	}
	var slots []int32
	for id, n := range ix.cat.seriesNames {
		if want[strings.ToLower(n)] {
			slots = append(slots, ix.series[id]...)
		}
	}
	sortSeries(ix.cat, slots)
	return slots
}
