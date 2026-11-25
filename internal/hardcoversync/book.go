package hardcoversync

// A shared book's sync on the book page: how it stands (sent, waiting,
// failing, or not found on Hardcover), and choosing which Hardcover book it
// is. Any linked user can choose, editor or not: the choice is theirs and
// is used for their own sync only; the library's identifiers are left
// alone (an editor fixes those in the metadata editor).

import (
	"database/sql"
	"errors"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// NotFoundMsg is a skipped book's error (skipError). Rows from before it was
// worded so start the same way.
const NotFoundMsg = "not found on Hardcover: it has no ISBN or Hardcover link that Hardcover knows"

func isNotFound(msg string) bool { return strings.HasPrefix(msg, "not found on Hardcover") }

// CatalogClient: the client to look books up with. The server's own
// token when there is one, so a search isn't tied to the user's account;
// else the user's (it has read:catalog).
func (s *Service) CatalogClient(user *hardcover.Client) *hardcover.Client {
	if c := s.catalog(); c != nil {
		return c
	}
	return user
}

// UserMatch: the Hardcover book userID chose for uuid, if any.
func (s *Service) UserMatch(userID int64, uuid string) (hardcover.Edition, bool) {
	var ed hardcover.Edition
	err := s.db.QueryRow(`SELECT hc_book_id, hc_edition_id, pages, slug, title FROM hardcover_user_books
		WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&ed.BookID, &ed.EditionID, &ed.Pages, &ed.Slug, &ed.Title)
	return ed, err == nil && ed.BookID > 0
}

// SetUserMatch keeps the book userID chose; its edition is set on their
// Hardcover entry at the next sync, even if the entry has another.
func (s *Service) SetUserMatch(userID int64, uuid string, ed hardcover.Edition) error {
	_, err := s.db.Exec(`INSERT INTO hardcover_user_books (user_id, book_uuid, hc_book_id, hc_edition_id, pages, slug, title, chosen_at, set_edition)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, book_uuid) DO UPDATE SET hc_book_id = excluded.hc_book_id, hc_edition_id = excluded.hc_edition_id,
			pages = excluded.pages, slug = excluded.slug, title = excluded.title, chosen_at = excluded.chosen_at,
			set_edition = excluded.set_edition`,
		userID, uuid, ed.BookID, ed.EditionID, ed.Pages, ed.Slug, ed.Title, s.now().Unix(), ed.EditionID > 0)
	return err
}

// editionToSet: the edition userID chose for uuid, if it's yet to be set
// on their Hardcover entry; else 0.
func (s *Service) editionToSet(userID int64, uuid string) int64 {
	var id int64
	_ = s.db.QueryRow(`SELECT hc_edition_id FROM hardcover_user_books WHERE user_id = ? AND book_uuid = ? AND set_edition`,
		userID, uuid).Scan(&id)
	return id
}

// Resend sends a book again from scratch: its last error is forgotten.
func (s *Service) Resend(userID int64, uuid string) {
	if s == nil {
		return
	}
	_, _ = s.db.Exec(`UPDATE hardcover_sync SET error = '', attempts = 0 WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
	s.Touch(userID, uuid, 0)
}

// BookStatus is GET /api/me/books/{uuid}/hardcover.
type BookStatus struct {
	Shared bool `json:"shared"`
	All    bool `json:"all"` // every book is shared
	// State: "off" (not shared), "no_status" (shared, but nothing to
	// send: no want to read / reading / read), "waiting", "sent",
	// "failing" (being retried) or "not_found".
	State   string     `json:"state"`
	Error   string     `json:"error,omitempty"`
	RetryAt int64      `json:"retry_at,omitempty"` // unix ms
	SentAt  int64      `json:"sent_at,omitempty"`  // unix ms
	Match   *MatchView `json:"match,omitempty"`
	// Query: what to search Hardcover for, to start with.
	Query string `json:"query"`
	// Placeholder: a book not in the library yet, sent as want to read
	// (hardcover_placeholder.go). Its Hardcover book is the library's,
	// not the user's: CanChoose when it has none yet, or for an editor.
	Placeholder bool `json:"placeholder,omitempty"`
	CanChoose   bool `json:"can_choose"`
}

// MatchView: the Hardcover book a library book is sent as.
type MatchView struct {
	BookID int64  `json:"book_id"`
	Slug   string `json:"slug,omitempty"`
	Title  string `json:"title,omitempty"`
	Chosen bool   `json:"chosen"` // by the user, not found from the library's identifiers
}

func (s *Service) BookState(userID int64, uuid string) BookStatus {
	st := BookStatus{Shared: s.Shared(userID, uuid), All: s.LoadShare(userID).All, State: "off"}
	if title, authors, ok := s.host.BookTitle(uuid); ok {
		st.Query = strings.TrimSpace(title + " " + firstAuthor(authors))
	}
	if ed, ok := s.UserMatch(userID, uuid); ok {
		st.Match = &MatchView{BookID: ed.BookID, Slug: ed.Slug, Title: ed.Title, Chosen: true}
	} else {
		var ed hardcover.Edition
		if s.db.QueryRow(`SELECT hc_book_id, slug, title FROM hardcover_books WHERE book_uuid = ? AND hc_book_id > 0`, uuid).
			Scan(&ed.BookID, &ed.Slug, &ed.Title) == nil {
			st.Match = &MatchView{BookID: ed.BookID, Slug: ed.Slug, Title: ed.Title}
		} else if id, slug, title, ok := s.host.Placeholder(uuid); ok && id > 0 {
			// Not looked up yet: as the placeholder names it.
			st.Match = &MatchView{BookID: id, Slug: slug, Title: title}
		}
	}
	if !st.Shared {
		return st
	}
	var due, sent int64
	var attempts int
	var msg string
	err := s.db.QueryRow(`SELECT due_at, attempts, error, sent_at FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`,
		userID, uuid).Scan(&due, &attempts, &msg, &sent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Println("hardcover book:", err)
	}
	if sent > 0 {
		st.SentAt = sent * 1000
	}
	status := s.host.ReadStatus(userID, uuid)
	_, sendable := statusIDs[status]
	switch {
	case due == 0 && isNotFound(msg):
		st.State, st.Error = "not_found", msg
	case due > 0 && attempts > 0:
		st.State, st.Error, st.RetryAt = "failing", msg, due*1000
	case due > 0 && sendable:
		st.State = "waiting"
	case !sendable:
		st.State = "no_status"
	case sent > 0:
		st.State = "sent"
	default:
		st.State = "waiting"
	}
	return st
}

// firstAuthor: the first of a catalog book's authors ("A & B").
func firstAuthor(authors string) string {
	a, _, _ := strings.Cut(authors, "&")
	return strings.TrimSpace(a)
}

// LinkTarget: a Hardcover book named by a pasted link
// (https://hardcover.app/books/<slug>), or by its id.
func LinkTarget(q string) (slug string, id int64, ok bool) {
	if n, err := strconv.ParseInt(q, 10, 64); err == nil && n > 0 {
		return "", n, true
	}
	u, err := url.Parse(q)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(strings.TrimPrefix(u.Hostname(), "www."), "hardcover.app") {
		return "", 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "books" || !hardcover.ValidSlug(parts[1]) {
		return "", 0, false
	}
	return parts[1], 0, true
}

// ProblemBooks: the books userID shares that can't be sent (not found,
// or failing), for the account page.
func (s *Service) ProblemBooks(userID int64) []SharedBook {
	rows, err := s.db.Query(`SELECT book_uuid FROM hardcover_sync WHERE user_id = ? AND error != '' ORDER BY book_uuid`, userID)
	if err != nil {
		return nil
	}
	var uuids []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			uuids = append(uuids, u)
		}
	}
	rows.Close()
	var out []SharedBook
	for _, u := range uuids {
		if title, authors, ok := s.host.BookTitle(u); ok {
			out = append(out, SharedBook{UUID: u, Title: title, Authors: authors})
		}
	}
	return out
}

// ClearUserMatch forgets the Hardcover book userID chose for uuid: it's
// sent as the library's identifiers name it again.
func (s *Service) ClearUserMatch(userID int64, uuid string) error {
	if _, err := s.db.Exec(`DELETE FROM hardcover_user_books WHERE user_id = ? AND book_uuid = ?`, userID, uuid); err != nil {
		return err
	}
	s.Resend(userID, uuid)
	return nil
}
