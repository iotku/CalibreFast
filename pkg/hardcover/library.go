package hardcover

// A signed-in user's library on Hardcover, for reading sync: who they are,
// their entry for a book and its read in progress, and changing them. Only
// a client made with the user's own token (Config.TokenFunc) may use
// these; it acts as that user.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Hardcover's reading statuses (user_books.status_id).
const (
	StatusWantToRead = 1
	StatusReading    = 2
	StatusRead       = 3
	StatusPaused     = 4
	StatusDNF        = 5
)

// Reading formats (editions.reading_format_id). Many editions have none
// (null), so a filter on it must keep those: in Hasura, as in SQL,
// {_neq: 2} drops a null.
const formatAudio = 2

var (
	qMe = query{`query Me {
  me { id username }
}`, 1}

	qSyncEditions = query{`query SyncEditions($isbns: [String!]!) {
  editions(where: {isbn_13: {_in: $isbns}}, order_by: {users_count: desc}, limit: 20) { id book_id isbn_13 pages reading_format_id book { slug title } }
}`, 1}

	qSyncBooks = query{`query SyncBooks($ids: [Int!]!, $slugs: [String!]!) {
  books(where: {_or: [{id: {_in: $ids}}, {slug: {_in: $slugs}}], canonical_id: {_is_null: true}}, limit: 10) {
    id
    slug
    title
    pages
    editions(where: {pages: {_gt: 0}, _or: [{reading_format_id: {_is_null: true}}, {reading_format_id: {_neq: 2}}]}, order_by: {users_count: desc}, limit: 1) { id pages }
  }
}`, 1}

	qUserBook = query{`query UserBook($user: Int!, $book: Int!) {
  user_books(where: {user_id: {_eq: $user}, book_id: {_eq: $book}}, limit: 1) {
    id
    status_id
    edition_id
    edition { pages }
    user_book_reads(order_by: {id: desc}, limit: 1) { id progress_pages started_at finished_at }
  }
}`, 1}

	qInsertUserBook = query{`mutation InsertUserBook($object: UserBookCreateInput!) {
  insert_user_book(object: $object) { id error }
}`, 1}

	qUpdateUserBook = query{`mutation UpdateUserBook($id: Int!, $object: UserBookUpdateInput!) {
  update_user_book(id: $id, object: $object) { id error }
}`, 1}

	qUpdateUserBookRead = query{`mutation UpdateUserBookRead($id: Int!, $object: DatesReadInput!) {
  update_user_book_read(id: $id, object: $object) { id error }
}`, 1}
)

func init() {
	for name, q := range map[string]query{
		"Me": qMe, "SyncEditions": qSyncEditions, "SyncBooks": qSyncBooks, "UserBook": qUserBook,
		"InsertUserBook": qInsertUserBook, "UpdateUserBook": qUpdateUserBook, "UpdateUserBookRead": qUpdateUserBookRead,
	} {
		allQueries[name] = q
	}
}

// Me is the signed-in user.
type Me struct {
	ID       int64
	Username string
}

// Me answers who the token belongs to.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var data struct {
		Me json.RawMessage `json:"me"`
	}
	if err := c.post(ctx, qMe, nil, &data); err != nil {
		return Me{}, err
	}
	type user struct {
		ID       flexInt `json:"id"`
		Username string  `json:"username"`
	}
	// me is a function returning users: a list of one.
	var list []user
	if err := json.Unmarshal(data.Me, &list); err != nil {
		var one user
		if err := json.Unmarshal(data.Me, &one); err != nil {
			return Me{}, errors.New("Hardcover's answer has no user")
		}
		list = []user{one}
	}
	if len(list) == 0 || list[0].ID <= 0 {
		return Me{}, errors.New("Hardcover's answer has no user")
	}
	return Me{ID: int64(list[0].ID), Username: clip(list[0].Username)}, nil
}

// Edition is the Hardcover book (and edition, if known) a library book is.
type Edition struct {
	BookID    int64
	EditionID int64  // 0: unknown
	Pages     int    // the edition's (or book's) page count; 0: unknown
	Slug      string // the book's (valid, ValidSlug), or ""
	Title     string // the book's
}

// FindEdition finds the book a library book is, best first: the edition
// with one of its ISBN-13s (not an audiobook), else the book by id or slug
// with its most read edition that has a page count. ok is false when
// Hardcover has none of them. Two requests at most.
func (c *Client) FindEdition(ctx context.Context, isbns []string, bookID int64, slug string) (Edition, bool, error) {
	if e, ok, err := c.editionByISBN(ctx, isbns, bookID, false); err != nil || ok {
		return e, ok, err
	}
	return c.bookEdition(ctx, bookID, slug)
}

// BookEdition is bookID, a book chosen by hand, in the edition with one of
// isbns (the library book's) when that edition is of this book; else its
// most read edition with a page count, as FindEdition. An ISBN of another
// book never changes the book. Two requests at most.
func (c *Client) BookEdition(ctx context.Context, bookID int64, isbns []string) (Edition, bool, error) {
	if bookID <= 0 {
		return Edition{}, false, nil
	}
	if e, ok, err := c.editionByISBN(ctx, isbns, bookID, true); err != nil || ok {
		return e, ok, err
	}
	return c.bookEdition(ctx, bookID, "")
}

// editionByISBN: the best edition with one of isbns; with onlyBook, only
// an edition of bookID.
func (c *Client) editionByISBN(ctx context.Context, isbns []string, bookID int64, onlyBook bool) (Edition, bool, error) {
	isbns = cleanISBNs(isbns)
	if len(isbns) > 0 {
		var data struct {
			Editions []struct {
				ID     flexInt `json:"id"`
				BookID flexInt `json:"book_id"`
				Pages  int     `json:"pages"`
				Format flexInt `json:"reading_format_id"` // 0: none given
				Book   struct {
					Slug  string `json:"slug"`
					Title string `json:"title"`
				} `json:"book"`
			} `json:"editions"`
		}
		if err := c.post(ctx, qSyncEditions, map[string]any{"isbns": isbns}, &data); err != nil {
			return Edition{}, false, err
		}
		// Prefer one that isn't an audiobook, then one with a page count,
		// then the book id we were given. An audiobook's ISBN still names
		// the book, but not its pages (they're minutes, if anything).
		var best *Edition
		bestScore := -1
		for _, e := range data.Editions {
			if e.ID <= 0 || e.BookID <= 0 || (onlyBook && int64(e.BookID) != bookID) {
				continue
			}
			cand := Edition{BookID: int64(e.BookID), EditionID: int64(e.ID), Pages: max(e.Pages, 0),
				Slug: cleanSlug(e.Book.Slug), Title: e.Book.Title}
			audio := e.Format == formatAudio
			if audio {
				cand.Pages = 0
			}
			s := score(cand, bookID)
			if !audio {
				s += 4
			}
			if best == nil || s > bestScore {
				best, bestScore = &cand, s
			}
		}
		if best != nil {
			return *best, true, nil
		}
	}
	return Edition{}, false, nil
}

// bookEdition: the book by id or slug, in its most read edition with a
// page count.
func (c *Client) bookEdition(ctx context.Context, bookID int64, slug string) (Edition, bool, error) {
	if !ValidSlug(slug) {
		slug = ""
	}
	if bookID <= 0 && slug == "" {
		return Edition{}, false, nil
	}
	ids := []int64{}
	if bookID > 0 {
		ids = append(ids, bookID)
	}
	slugs := []string{}
	if slug != "" {
		slugs = append(slugs, slug)
	}
	var data struct {
		Books []struct {
			ID       flexInt `json:"id"`
			Slug     string  `json:"slug"`
			Title    string  `json:"title"`
			Pages    int     `json:"pages"`
			Editions []struct {
				ID    flexInt `json:"id"`
				Pages int     `json:"pages"`
			} `json:"editions"`
		} `json:"books"`
	}
	if err := c.post(ctx, qSyncBooks, map[string]any{"ids": ids, "slugs": slugs}, &data); err != nil {
		return Edition{}, false, err
	}
	var found *Edition
	for _, b := range data.Books {
		if b.ID <= 0 {
			continue
		}
		e := Edition{BookID: int64(b.ID), Pages: max(b.Pages, 0), Slug: cleanSlug(b.Slug), Title: b.Title}
		if len(b.Editions) > 0 && b.Editions[0].ID > 0 {
			e.EditionID, e.Pages = int64(b.Editions[0].ID), b.Editions[0].Pages
		}
		// The id outranks the slug: a slug can be renamed.
		if found == nil || int64(b.ID) == bookID {
			found = &e
		}
	}
	if found == nil {
		return Edition{}, false, nil
	}
	return *found, true, nil
}

func score(e Edition, bookID int64) int {
	s := 0
	if e.Pages > 0 {
		s += 2
	}
	if bookID > 0 && e.BookID == bookID {
		s++
	}
	return s
}

func cleanISBNs(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.ReplaceAll(strings.TrimSpace(s), "-", "")
		if len(s) != 13 || strings.Trim(s, "0123456789") != "" {
			continue
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
		if len(out) == 10 {
			break
		}
	}
	return out
}

// UserBook is a user's entry for a book, and its latest read (ReadID 0:
// none; Finished set: it's done, and none is in progress).
type UserBook struct {
	ID            int64
	StatusID      int
	EditionID     int64
	EditionPages  int // the entry's edition's page count; 0: unknown
	ReadID        int64
	ProgressPages int
	StartedAt     string // YYYY-MM-DD or ""
	FinishedAt    string // YYYY-MM-DD or ""
}

// UserBook answers user's entry for book, or nil if they have none.
func (c *Client) UserBook(ctx context.Context, user, book int64) (*UserBook, error) {
	var data struct {
		UserBooks []struct {
			ID        flexInt  `json:"id"`
			StatusID  int      `json:"status_id"`
			EditionID *flexInt `json:"edition_id"`
			Edition   *struct {
				Pages int `json:"pages"`
			} `json:"edition"`
			Reads []struct {
				ID            flexInt `json:"id"`
				ProgressPages *int    `json:"progress_pages"`
				StartedAt     *string `json:"started_at"`
				FinishedAt    *string `json:"finished_at"`
			} `json:"user_book_reads"`
		} `json:"user_books"`
	}
	if err := c.post(ctx, qUserBook, map[string]any{"user": user, "book": book}, &data); err != nil {
		return nil, err
	}
	if len(data.UserBooks) == 0 || data.UserBooks[0].ID <= 0 {
		return nil, nil
	}
	b := data.UserBooks[0]
	ub := &UserBook{ID: int64(b.ID), StatusID: b.StatusID}
	if b.EditionID != nil {
		ub.EditionID = int64(*b.EditionID)
	}
	if len(b.Reads) > 0 {
		r := b.Reads[0]
		ub.ReadID = int64(r.ID)
		if r.ProgressPages != nil {
			ub.ProgressPages = *r.ProgressPages
		}
		ub.StartedAt, ub.FinishedAt = day(r.StartedAt), day(r.FinishedAt)
	}
	if b.Edition != nil {
		ub.EditionPages = max(b.Edition.Pages, 0)
	}
	return ub, nil
}

// day is a date (or time) answered as its YYYY-MM-DD.
func day(s *string) string {
	if s == nil || len(*s) < 10 {
		return ""
	}
	return (*s)[:10]
}

// mutationResult is what the library mutations answer: an id, or an error
// Hardcover explains.
type mutationResult struct {
	ID    *flexInt `json:"id"`
	Error *string  `json:"error"`
}

func (m mutationResult) check(what string) (int64, error) {
	if m.Error != nil && *m.Error != "" {
		return 0, fmt.Errorf("Hardcover couldn't %s: %s", what, clip(*m.Error))
	}
	if m.ID == nil {
		return 0, nil
	}
	return int64(*m.ID), nil
}

// AddUserBook adds book to the user's library with status (edition 0:
// none) and answers the entry's id.
func (c *Client) AddUserBook(ctx context.Context, book int64, status int, edition int64) (int64, error) {
	obj := map[string]any{"book_id": book, "status_id": status}
	if edition > 0 {
		obj["edition_id"] = edition
	}
	var data struct {
		R mutationResult `json:"insert_user_book"`
	}
	if err := c.post(ctx, qInsertUserBook, map[string]any{"object": obj}, &data); err != nil {
		return 0, err
	}
	return data.R.check("add the book")
}

// SetUserBook changes an entry: its status, and its edition when > 0.
func (c *Client) SetUserBook(ctx context.Context, id int64, status int, edition int64) error {
	obj := map[string]any{"status_id": status}
	if edition > 0 {
		obj["edition_id"] = edition
	}
	var data struct {
		R mutationResult `json:"update_user_book"`
	}
	if err := c.post(ctx, qUpdateUserBook, map[string]any{"id": id, "object": obj}, &data); err != nil {
		return err
	}
	_, err := data.R.check("change the book's status")
	return err
}

// ReadUpdate changes a read; zero fields are left as they are.
type ReadUpdate struct {
	ProgressPages int       // > 0
	StartedAt     string    // YYYY-MM-DD
	FinishedAt    string    // YYYY-MM-DD
	ActionAt      time.Time // when the progress was made (dates its journal entry)
}

// UpdateRead changes the read id.
func (c *Client) UpdateRead(ctx context.Context, id int64, u ReadUpdate) error {
	obj := map[string]any{}
	if u.ProgressPages > 0 {
		obj["progress_pages"] = u.ProgressPages
	}
	if u.StartedAt != "" {
		obj["started_at"] = u.StartedAt
	}
	if u.FinishedAt != "" {
		obj["finished_at"] = u.FinishedAt
	}
	if len(obj) == 0 {
		return nil
	}
	if !u.ActionAt.IsZero() {
		obj["action_at"] = u.ActionAt.UTC().Format(time.RFC3339)
	}
	var data struct {
		R mutationResult `json:"update_user_book_read"`
	}
	if err := c.post(ctx, qUpdateUserBookRead, map[string]any{"id": id, "object": obj}, &data); err != nil {
		return err
	}
	_, err := data.R.check("change the read")
	return err
}
