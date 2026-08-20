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

	authorDir := sanitizePath(meta.Creators[0])
	titleDir := sanitizePath(meta.Title)

	authorSort := authorSortKey(meta.Creators[0])
	seriesIndex := meta.getMeta("calibre:series_index") // TODO: do we actually set this anywhere?
	if seriesIndex == "" {
		seriesIndex = "1.0"
	}

	pubDate := normalizeDate(meta.Date)
	tSort := titleSort(meta.Title)

	res, err := tx.Exec(`
		INSERT INTO books (title, sort, author_sort, timestamp, pubdate, series_index, path, uuid, has_cover, last_modified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		meta.Title,
		tSort,
		authorSort,
		now,
		pubDate,
		seriesIndex,
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

	for _, authorName := range meta.Creators {
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

	for _, tag := range meta.Subjects {
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

	seriesName := meta.getMeta("calibre:series")
	if seriesName != "" {
		seriesID, sErr := upsertSimpleEntity(tx, "series", seriesName)
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
	for _, id := range meta.Identifiers {
		if id.Value == "" {
			continue
		}
		idType, idVal := parseIdentifier(id)
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

// updateBookInCalibreDB updates a book's metadata in the Calibre database and its on-disk metadata.opf.
// It ensures strict Calibre database compatibility, including cleaning up orphaned authors, tags,
// publishers, and languages when the edited book was the only book linked to them.
func updateBookInCalibreDB(db *sql.DB, baseDir string, bookUUID string, meta *OPFMetadata) (string, error) {
	var bookID int64
	var bookPath string
	err := db.QueryRow(`SELECT id, path FROM books WHERE uuid = ?`, bookUUID).Scan(&bookID, &bookPath)
	if err != nil {
		return "", fmt.Errorf("book with uuid %s not found: %w", bookUUID, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	now := calibreDateTime(time.Now())
	tSort := titleSort(meta.Title)
	if meta.Title == "" {
		meta.Title = "Unknown"
		tSort = "Unknown"
	}

	authorSortParts := make([]string, 0, len(meta.Creators))
	for _, c := range meta.Creators {
		trimmed := strings.TrimSpace(c)
		if trimmed != "" {
			authorSortParts = append(authorSortParts, authorSortKey(trimmed))
		}
	}
	authorSort := strings.Join(authorSortParts, " & ")
	if authorSort == "" {
		authorSort = "Unknown"
	}

	pubDate := meta.Date
	if pubDate == "" {
		pubDate = "0101-01-01 00:00:00+00:00"
	}

	// 1. Update books table
	_, err = tx.Exec(`
		UPDATE books SET
			title = ?,
			sort = ?,
			author_sort = ?,
			pubdate = ?,
			last_modified = ?
		WHERE id = ?`,
		meta.Title,
		tSort,
		authorSort,
		pubDate,
		now,
		bookID,
	)
	if err != nil {
		return "", fmt.Errorf("update books table: %w", err)
	}

	// 2. Update comments (description)
	_, err = tx.Exec(`DELETE FROM comments WHERE book = ?`, bookID)
	if err != nil {
		return "", fmt.Errorf("delete comments: %w", err)
	}
	if strings.TrimSpace(meta.Description) != "" {
		_, err = tx.Exec(`INSERT INTO comments (book, text) VALUES (?, ?)`, bookID, meta.Description)
		if err != nil {
			return "", fmt.Errorf("insert comments: %w", err)
		}
	}

	// 3. Authors and books_authors_link
	var oldAuthorIDs []int64
	rows, err := tx.Query(`SELECT author FROM books_authors_link WHERE book = ?`, bookID)
	if err != nil {
		return "", fmt.Errorf("fetch current authors: %w", err)
	}
	for rows.Next() {
		var aID int64
		if err := rows.Scan(&aID); err == nil {
			oldAuthorIDs = append(oldAuthorIDs, aID)
		}
	}
	rows.Close()

	newAuthorIDMap := make(map[int64]bool)
	for _, authorName := range meta.Creators {
		trimmed := strings.TrimSpace(authorName)
		if trimmed == "" {
			continue
		}
		aID, aErr := upsertAuthor(tx, trimmed)
		if aErr != nil {
			return "", fmt.Errorf("upsert author %q: %w", trimmed, aErr)
		}
		newAuthorIDMap[aID] = true
		if _, aErr = tx.Exec(`INSERT OR IGNORE INTO books_authors_link (book, author) VALUES (?, ?)`, bookID, aID); aErr != nil {
			return "", fmt.Errorf("link author: %w", aErr)
		}
	}

	for _, oldID := range oldAuthorIDs {
		if !newAuthorIDMap[oldID] {
			if _, dErr := tx.Exec(`DELETE FROM books_authors_link WHERE book = ? AND author = ?`, bookID, oldID); dErr != nil {
				return "", fmt.Errorf("unlink author %d: %w", oldID, dErr)
			}
			// Clean up orphan author ONLY if this book was the only book linked to that author
			var count int
			if cErr := tx.QueryRow(`SELECT COUNT(*) FROM books_authors_link WHERE author = ?`, oldID).Scan(&count); cErr == nil && count == 0 {
				if _, delErr := tx.Exec(`DELETE FROM authors WHERE id = ?`, oldID); delErr != nil {
					return "", fmt.Errorf("clean up orphan author %d: %w", oldID, delErr)
				}
			}
		}
	}

	// 4. Tags and books_tags_link
	var oldTagIDs []int64
	rows, err = tx.Query(`SELECT tag FROM books_tags_link WHERE book = ?`, bookID)
	if err != nil {
		return "", fmt.Errorf("fetch current tags: %w", err)
	}
	for rows.Next() {
		var tID int64
		if err := rows.Scan(&tID); err == nil {
			oldTagIDs = append(oldTagIDs, tID)
		}
	}
	rows.Close()

	newTagIDMap := make(map[int64]bool)
	for _, tagName := range meta.Subjects {
		trimmed := strings.TrimSpace(tagName)
		if trimmed == "" {
			continue
		}
		tID, tErr := upsertSimpleEntity(tx, "tags", trimmed)
		if tErr != nil {
			return "", fmt.Errorf("upsert tag %q: %w", trimmed, tErr)
		}
		newTagIDMap[tID] = true
		if _, tErr = tx.Exec(`INSERT OR IGNORE INTO books_tags_link (book, tag) VALUES (?, ?)`, bookID, tID); tErr != nil {
			return "", fmt.Errorf("link tag: %w", tErr)
		}
	}

	for _, oldID := range oldTagIDs {
		if !newTagIDMap[oldID] {
			if _, dErr := tx.Exec(`DELETE FROM books_tags_link WHERE book = ? AND tag = ?`, bookID, oldID); dErr != nil {
				return "", fmt.Errorf("unlink tag %d: %w", oldID, dErr)
			}
			var count int
			if cErr := tx.QueryRow(`SELECT COUNT(*) FROM books_tags_link WHERE tag = ?`, oldID).Scan(&count); cErr == nil && count == 0 {
				if _, delErr := tx.Exec(`DELETE FROM tags WHERE id = ?`, oldID); delErr != nil {
					return "", fmt.Errorf("clean up orphan tag %d: %w", oldID, delErr)
				}
			}
		}
	}

	// 5. Publishers and books_publishers_link
	var oldPubIDs []int64
	rows, err = tx.Query(`SELECT publisher FROM books_publishers_link WHERE book = ?`, bookID)
	if err != nil {
		return "", fmt.Errorf("fetch current publishers: %w", err)
	}
	for rows.Next() {
		var pID int64
		if err := rows.Scan(&pID); err == nil {
			oldPubIDs = append(oldPubIDs, pID)
		}
	}
	rows.Close()

	var newPubID int64
	pubTrimmed := strings.TrimSpace(meta.Publisher)
	if pubTrimmed != "" {
		newPubID, err = upsertSimpleEntity(tx, "publishers", pubTrimmed)
		if err != nil {
			return "", fmt.Errorf("upsert publisher %q: %w", pubTrimmed, err)
		}
		if _, err = tx.Exec(`INSERT OR IGNORE INTO books_publishers_link (book, publisher) VALUES (?, ?)`, bookID, newPubID); err != nil {
			return "", fmt.Errorf("link publisher: %w", err)
		}
	}

	for _, oldID := range oldPubIDs {
		if oldID != newPubID {
			if _, dErr := tx.Exec(`DELETE FROM books_publishers_link WHERE book = ? AND publisher = ?`, bookID, oldID); dErr != nil {
				return "", fmt.Errorf("unlink publisher %d: %w", oldID, dErr)
			}
			var count int
			if cErr := tx.QueryRow(`SELECT COUNT(*) FROM books_publishers_link WHERE publisher = ?`, oldID).Scan(&count); cErr == nil && count == 0 {
				if _, delErr := tx.Exec(`DELETE FROM publishers WHERE id = ?`, oldID); delErr != nil {
					return "", fmt.Errorf("clean up orphan publisher %d: %w", oldID, delErr)
				}
			}
		}
	}

	// 6. Languages and books_languages_link
	var oldLangIDs []int64
	rows, err = tx.Query(`SELECT lang_code FROM books_languages_link WHERE book = ?`, bookID)
	if err != nil {
		return "", fmt.Errorf("fetch current languages: %w", err)
	}
	for rows.Next() {
		var lID int64
		if err := rows.Scan(&lID); err == nil {
			oldLangIDs = append(oldLangIDs, lID)
		}
	}
	rows.Close()

	var newLangID int64
	langTrimmed := strings.TrimSpace(meta.Language)
	if langTrimmed != "" && langTrimmed != "und" {
		newLangID, err = upsertLanguage(tx, langTrimmed)
		if err == nil && newLangID > 0 {
			tx.Exec(`INSERT OR IGNORE INTO books_languages_link (book, lang_code) VALUES (?, ?)`, bookID, newLangID)
		}
	}

	for _, oldID := range oldLangIDs {
		if oldID != newLangID {
			tx.Exec(`DELETE FROM books_languages_link WHERE book = ? AND lang_code = ?`, bookID, oldID)
			var count int
			if cErr := tx.QueryRow(`SELECT COUNT(*) FROM books_languages_link WHERE lang_code = ?`, oldID).Scan(&count); cErr == nil && count == 0 {
				tx.Exec(`DELETE FROM languages WHERE id = ?`, oldID)
			}
		}
	}

	// 7. Identifiers
	if _, err = tx.Exec(`DELETE FROM identifiers WHERE book = ?`, bookID); err != nil {
		return "", fmt.Errorf("delete old identifiers: %w", err)
	}
	for _, id := range meta.Identifiers {
		if strings.TrimSpace(id.Value) == "" {
			continue
		}
		idType, idVal := parseIdentifier(id)
		if idVal != "" {
			if _, err = tx.Exec(`INSERT OR IGNORE INTO identifiers (book, type, val) VALUES (?, ?, ?)`, bookID, idType, idVal); err != nil {
				return "", fmt.Errorf("insert identifier %s=%s: %w", idType, idVal, err)
			}
		}
	}

	seriesName := meta.getMeta("calibre:series")
	if seriesName != "" {
		seriesID, sErr := upsertSimpleEntity(tx, "series", seriesName)
		if sErr == nil {
			tx.Exec(`INSERT OR IGNORE INTO books_series_link (book, series) VALUES (?, ?)`, bookID, seriesID)
		}
	}

	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	// 8. Update metadata.opf on disk
	if baseDir != "" && bookPath != "" {
		opfPath := filepath.Join(baseDir, bookPath, "metadata.opf")
		existingOPF, lErr := loadOPF(opfPath)
		if lErr != nil || existingOPF == nil {
			existingOPF = &OPF{
				Metadata: *meta,
			}
		} else {
			existingOPF.Metadata.Title = meta.Title
			existingOPF.Metadata.Creators = meta.Creators
			existingOPF.Metadata.Publisher = meta.Publisher
			existingOPF.Metadata.Date = meta.Date
			existingOPF.Metadata.Language = meta.Language
			existingOPF.Metadata.Subjects = meta.Subjects
			existingOPF.Metadata.Description = meta.Description
			existingOPF.Metadata.Identifiers = meta.Identifiers
		}
		_ = writeOPF(existingOPF, opfPath)
	}

	uuidPathIndex.Store(bookUUID, bookPath)

	return bookPath, nil
}

