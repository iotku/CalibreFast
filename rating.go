package main

// Calibre's ratings (books_ratings_link → ratings.rating, 0–10 half-stars):
// shown in the book popup, sorted and searched by (catalog_sort.go,
// search.go), and set by editors in the metadata editor (database.go).

import (
	"database/sql"
	"strconv"
	"strings"
)

// setBookRating replaces a book's rating; 0 removes it.
func setBookRating(tx *sql.Tx, bookID int64, rating int) error {
	if _, err := tx.Exec(`DELETE FROM books_ratings_link WHERE book = ?`, bookID); err != nil {
		return err
	}
	if rating <= 0 {
		return nil
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO ratings (rating) VALUES (?)`, rating); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO books_ratings_link (book, rating) SELECT ?, id FROM ratings WHERE rating = ?`, bookID, rating)
	return err
}

// libraryRating is a book's rating in metadata.db (0: none). Read there,
// as the series is (librarySeriesFields), so the editor and the popup show a
// rating just saved, before the catalog catches up.
func libraryRating(uuid string) int {
	if calibreDB == nil {
		return 0
	}
	var r sql.NullInt64
	where, args := bookWhere(uuid) // by id: no scan of books (catalog.go)
	_ = calibreDB.QueryRow(`SELECT (SELECT r.rating FROM books_ratings_link l JOIN ratings r ON r.id = l.rating WHERE l.book = b.id)
		FROM books b WHERE `+where, args...).Scan(&r)
	return min(max(int(r.Int64), 0), 10)
}

// ratingStars is what the popup shows for a rating: five stars, each full,
// half or empty.
type ratingStars struct {
	Stars []string // "full", "half", "empty"
	Text  string   // "Rated 4.5 out of 5"
}

func starsFor(rating int) *ratingStars {
	if rating <= 0 {
		return nil
	}
	s := &ratingStars{}
	for i := 1; i <= 5; i++ {
		switch {
		case rating >= 2*i:
			s.Stars = append(s.Stars, "full")
		case rating == 2*i-1:
			s.Stars = append(s.Stars, "half")
		default:
			s.Stars = append(s.Stars, "empty")
		}
	}
	s.Text = "Rated " + strings.TrimSuffix(strconv.FormatFloat(float64(rating)/2, 'f', 1, 64), ".0") + " out of 5"
	return s
}
