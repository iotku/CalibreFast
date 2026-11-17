package main

// Series: shown on a book's page, browsed at /series/<name> (in series
// order), and edited in the metadata editor (single book, import queue,
// merge). In metadata.db a book has at most one series (books_series_link)
// and a number in it (books.series_index); in its metadata.opf they're the
// calibre:series and calibre:series_index meta, as Calibre writes them.
// "Next in series" (recommend.go) depends on both being right.

import (
	"database/sql"
	"strconv"
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
