package main

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var titleSortRE = regexp.MustCompile(`(?i)^(a|an|the)\s+`)

func titleSort(title string) string {
	title = strings.TrimSpace(title)

	m := titleSortRE.FindStringSubmatch(title)
	if m == nil {
		return title
	}

	article := strings.TrimSpace(m[1])
	rest := strings.TrimSpace(title[len(m[0]):])

	if rest == "" {
		return title
	}

	article = strings.ToUpper(article[:1]) + strings.ToLower(article[1:])

	return rest + ", " + article
}

func calibreDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000-07:00")
}

// deleteBookFromCalibreDB removes book entries from the provided CalibreDB and associated tables
func deleteBookFromCalibreDB(db *sql.DB, bookID int64) error { // TODO: NEEDS TESTING
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { // TODO: is this actually good?
		if err != nil {
			tx.Rollback()
		}
	}()
	tables := []string{ // TODO: Verify these are all the tabes
		"books_authors_link",
		"books_tags_link",
		"books_series_link",
		"books_publishers_link",
		"books_languages_link",
		"data",
		"identifiers",
	}

	for _, table := range tables {
		_, err := tx.Exec(`DELETE FROM `+table+` WHERE book = ?`, bookID)
		if err != nil {
			return fmt.Errorf("delete from %s: %w", table, err)
		}
	}

	// TODO: Should we remove the author if this book was the only match

	// Finally delete the book itself
	res, err := tx.Exec(`DELETE FROM books WHERE id = ?`, bookID)
	if err != nil {
		return fmt.Errorf("delete book: %w", err)
	}

	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("expected to delete 1 book, deleted %d", n)
	}

	return nil
}
