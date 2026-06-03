package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
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

// insertBookIntoCalibreDB inserts a book and all its formats into metadata.db,
// following Calibre's schema exactly so the library remains Calibre-compatible.
func insertBookIntoCalibreDB(db *sql.DB, meta *UploadedBookMeta, formats []formatEntry) (bookID int64, bookUUID string, bookPath string, retErr error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, "", "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if retErr != nil {
			tx.Rollback()
		}
	}()

	bookUUID = uuid.New().String()
	now := calibreDateTime(time.Now())

	authorDir := sanitizePath(meta.Authors[0])
	titleDir := sanitizePath(meta.Title)

	authorSort := authorSortKey(meta.Authors[0])
	res, err := tx.Exec(`
		INSERT INTO books (title, sort, author_sort, timestamp, pubdate, series_index, path, uuid, has_cover, last_modified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		meta.Title,
		meta.Title,
		authorSort,
		now,
		meta.PubDate,
		meta.SeriesIdx,
		"", // We need the bookID to create the full path, we update that later
		bookUUID,
		now,
	)
	if err != nil {
		retErr = fmt.Errorf("insert book: %w", err)
		return
	}
	bookID, err = res.LastInsertId()
	if err != nil {
		retErr = fmt.Errorf("last insert id: %w", err)
		return
	}

	// Update the bookPath now that we have the database incremented ID
	bookPath = authorDir + "/" + titleDir + " (" + strconv.Itoa(int(bookID)) + ")"
	res, err = tx.Exec(`UPDATE books SET path = ? WHERE id = ?`, bookPath, bookID)
	if err != nil {
		retErr = fmt.Errorf("update bookPath: %w", err)
		println("failed to update bookpath:", retErr)
		return
	}

	for _, authorName := range meta.Authors {
		authorID, aErr := upsertAuthor(tx, authorName)
		if aErr != nil {
			retErr = fmt.Errorf("upsert author %q: %w", authorName, aErr)
			return
		}
		if _, aErr = tx.Exec(`INSERT OR IGNORE INTO books_authors_link (book, author) VALUES (?, ?)`, bookID, authorID); aErr != nil {
			retErr = fmt.Errorf("link author: %w", aErr)
			return
		}
	}

	for _, f := range formats {
		if _, fErr := tx.Exec(`
			INSERT INTO data (book, format, uncompressed_size, name)
			VALUES (?, ?, ?, ?)`,
			bookID,
			strings.ToUpper(f.Ext),
			f.Size,
			strings.TrimSuffix(f.Filename, filepath.Ext(f.Filename)),
		); fErr != nil {
			retErr = fmt.Errorf("insert format %s: %w", f.Ext, fErr)
			return
		}
	}

	if meta.Publisher != "" {
		pubID, pErr := upsertSimpleEntity(tx, "publishers", meta.Publisher)
		if pErr != nil {
			retErr = fmt.Errorf("upsert publisher: %w", pErr)
			return
		}
		if _, pErr = tx.Exec(`INSERT OR IGNORE INTO books_publishers_link (book, publisher) VALUES (?, ?)`, bookID, pubID); pErr != nil {
			retErr = fmt.Errorf("link publisher: %w", pErr)
			return
		}
	}

	for _, tag := range meta.Tags {
		tagID, tErr := upsertSimpleEntity(tx, "tags", tag)
		if tErr != nil {
			retErr = fmt.Errorf("upsert tag %q: %w", tag, tErr)
			return
		}
		if _, tErr = tx.Exec(`INSERT OR IGNORE INTO books_tags_link (book, tag) VALUES (?, ?)`, bookID, tagID); tErr != nil {
			retErr = fmt.Errorf("link tag: %w", tErr)
			return
		}
	}

	if meta.Series != "" {
		seriesID, sErr := upsertSimpleEntity(tx, "series", meta.Series)
		if sErr != nil {
			retErr = fmt.Errorf("upsert series: %w", sErr)
			return
		}
		if _, sErr = tx.Exec(`INSERT OR IGNORE INTO books_series_link (book, series) VALUES (?, ?)`, bookID, seriesID); sErr != nil {
			retErr = fmt.Errorf("link series: %w", sErr)
			return
		}
	}

	// Language and identifier are best-effort; don't abort the whole insert on failure.
	if meta.Language != "" && meta.Language != "und" {
		if langID, lErr := upsertLanguage(tx, meta.Language); lErr == nil {
			tx.Exec(`INSERT OR IGNORE INTO books_languages_link (book, lang_code) VALUES (?, ?)`, bookID, langID)
		}
	}
	if meta.Identifier != "" {
		idType, idVal := parseIdentifier(meta.Identifier)
		tx.Exec(`INSERT OR IGNORE INTO identifiers (book, type, val) VALUES (?, ?, ?)`, bookID, idType, idVal)
	}

	if err = tx.Commit(); err != nil {
		retErr = fmt.Errorf("commit: %w", err)
		return
	}

	return bookID, bookUUID, bookPath, nil
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
