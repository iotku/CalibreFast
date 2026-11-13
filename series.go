package main

// Series: shown on a book's page, browsed at /series/<name> (in series
// order), and edited in the metadata editor (single book, import queue,
// merge). In metadata.db a book has at most one series (books_series_link)
// and a number in it (books.series_index); in its metadata.opf they're the
// calibre:series and calibre:series_index meta, as Calibre writes them.
// "Next in series" (recommend.go) depends on both being right.

import (
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxSeriesName = 255

// fillSeries sets Series and SeriesIndex from the OPF's meta, unless
// they're set already: Calibre's calibre:series, or EPUB 3's
// belongs-to-collection (with its group-position).
func (m *OPFMetadata) fillSeries() {
	if m.Series != "" {
		return
	}
	if name := strings.TrimSpace(m.getMeta("calibre:series")); name != "" {
		m.Series = name
		m.SeriesIndex = formatSeriesIndex(m.getMeta("calibre:series_index"))
		return
	}
	for _, c := range m.Meta {
		if c.Property != "belongs-to-collection" || strings.TrimSpace(c.Value) == "" {
			continue
		}
		// A "set" is a collection too; a series is either untyped or typed so.
		kind, pos := "", ""
		for _, r := range m.Meta {
			if c.ID == "" || r.Refines != "#"+c.ID {
				continue
			}
			switch r.Property {
			case "collection-type":
				kind = strings.TrimSpace(r.Value)
			case "group-position":
				pos = strings.TrimSpace(r.Value)
			}
		}
		if kind != "" && kind != "series" {
			continue
		}
		m.Series = strings.TrimSpace(c.Value)
		m.SeriesIndex = formatSeriesIndex(pos)
		return
	}
}

// formatSeriesIndex: "2.0" -> "2", "2.50" -> "2.5"; "" or not a number -> "".
func formatSeriesIndex(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// cleanSeries checks what the editor sent: a name (or none) and its
// number, which defaults to 1 as in Calibre.
func cleanSeries(name, index string) (string, float64, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", 1, nil
	}
	if utf8.RuneCountInString(name) > maxSeriesName {
		return "", 0, errors.New("the series name is too long")
	}
	index = strings.TrimSpace(index)
	if index == "" {
		return name, 1, nil
	}
	f, err := strconv.ParseFloat(index, 64)
	if err != nil || f < 0 || f > 1e6 || math.IsNaN(f) || math.IsInf(f, 0) {
		return "", 0, errors.New("the number in the series must be a number, such as 2 or 2.5")
	}
	return name, f, nil
}

// syncSeriesMeta writes Series and SeriesIndex into the OPF meta
// (calibre:series, calibre:series_index), replacing what was there, or
// removes them when there's no series. EPUB 3 series meta is removed too,
// so the file says one thing.
func (m *OPFMetadata) syncSeriesMeta() {
	name, index, _ := cleanSeries(m.Series, m.SeriesIndex)
	collections := map[string]bool{}
	for _, c := range m.Meta {
		if c.Property == "belongs-to-collection" && c.ID != "" {
			collections["#"+c.ID] = true
		}
	}
	kept := m.Meta[:0:0]
	for _, c := range m.Meta {
		if c.Name == "calibre:series" || c.Name == "calibre:series_index" ||
			c.Property == "belongs-to-collection" || collections[c.Refines] {
			continue
		}
		kept = append(kept, c)
	}
	if name != "" {
		kept = append(kept,
			opfMeta{Name: "calibre:series", Content: name},
			opfMeta{Name: "calibre:series_index", Content: strconv.FormatFloat(index, 'f', -1, 64)})
	}
	m.Meta = kept
	m.Series, m.SeriesIndex = name, ""
	if name != "" {
		m.SeriesIndex = strconv.FormatFloat(index, 'f', -1, 64)
	}
}

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
