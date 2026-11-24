package hardcoversync

// Placeholders (books not in the library yet) are synced like books, as
// want to read, by the Hardcover book their own identifiers name, which
// are the library's: see the app's hardcover_placeholder.go. Here: what's
// kept about a placeholder's sync when its Hardcover book changes, or a
// library book replaces it.

import (
	"log"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// BookChanged: placeholder id is now Hardcover book ed (nil:
// not known). What was looked up for it is forgotten (or is ed), and every
// user syncing it but except has it sent again, a "not found" included.
func (s *Service) BookChanged(id string, ed *hardcover.Edition, except int64) {
	if s == nil {
		return
	}
	if ed != nil {
		_, _ = s.db.Exec(`INSERT INTO hardcover_books (book_uuid, hc_book_id, hc_edition_id, pages, slug, title, resolved_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (book_uuid) DO UPDATE SET hc_book_id = excluded.hc_book_id, hc_edition_id = excluded.hc_edition_id,
				pages = excluded.pages, slug = excluded.slug, title = excluded.title, resolved_at = excluded.resolved_at`,
			id, ed.BookID, ed.EditionID, ed.Pages, ed.Slug, ed.Title, s.now().Unix())
	} else {
		_, _ = s.db.Exec(`DELETE FROM hardcover_books WHERE book_uuid = ?`, id)
	}
	rows, err := s.db.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ? AND user_id != ?`, id, except)
	if err != nil {
		log.Println("hardcover placeholder:", err)
		return
	}
	var users []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	for _, u := range users {
		s.Resend(u, id) // only for those linked who share it (mark)
	}
}

// PlaceholderReplaced: library book bookUUID has replaced placeholder
// id. Whoever shared the placeholder with Hardcover shares the book (and
// one who left it out of sharing every book leaves the book out), and
// what was kept about the placeholder's sync goes. The book is sent as it
// stands (its status: the Saved moved to it by notifications.go).
func (s *Service) PlaceholderReplaced(id, bookUUID string) {
	if s == nil || id == "" || bookUUID == "" {
		return
	}
	var users []int64
	rows, err := s.db.Query(`SELECT user_id FROM hardcover_shared WHERE book_uuid = ?`, id)
	if err == nil {
		for rows.Next() {
			var u int64
			if rows.Scan(&u) == nil {
				users = append(users, u)
			}
		}
		rows.Close()
	}
	for _, q := range []string{
		`INSERT OR IGNORE INTO hardcover_shared (user_id, book_uuid, shared_at) SELECT user_id, ?1, shared_at FROM hardcover_shared WHERE book_uuid = ?2`,
		`INSERT OR IGNORE INTO hardcover_unshared (user_id, book_uuid, unshared_at) SELECT x.user_id, ?1, x.unshared_at FROM hardcover_unshared x
			WHERE x.book_uuid = ?2 AND NOT EXISTS (SELECT 1 FROM hardcover_shared s WHERE s.user_id = x.user_id AND s.book_uuid = ?1)`,
		`DELETE FROM hardcover_shared WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_unshared WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_sync WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_books WHERE book_uuid = ?2`,
		`DELETE FROM hardcover_user_books WHERE book_uuid = ?2`,
	} {
		if _, err := s.db.Exec(q, bookUUID, id); err != nil {
			log.Printf("hardcover placeholder %s: %v", id, err)
		}
	}
	// And those who share every book, with a status for it now.
	if rows, err := s.db.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ?`, bookUUID); err == nil {
		for rows.Next() {
			var u int64
			if rows.Scan(&u) == nil {
				users = append(users, u)
			}
		}
		rows.Close()
	}
	for _, u := range users {
		s.Touch(u, bookUUID, 0)
	}
}
