package main

// A shared book's sync on the book page: how it stands (sent, waiting,
// failing, or not found on Hardcover), and choosing which Hardcover book it
// is. Any linked user can choose, editor or not: the choice is theirs and
// is used for their own sync only; the library's identifiers are left
// alone (an editor fixes those in the metadata editor).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// hcNotFound is a skipped book's error (hcSkip). Rows from before it was
// worded so start the same way.
const hcNotFound = "not found on Hardcover: it has no ISBN or Hardcover link that Hardcover knows"

func hcIsNotFound(msg string) bool { return strings.HasPrefix(msg, "not found on Hardcover") }

// hcCatalogClient: the client to look books up with. The server's own
// token when there is one, so a search isn't tied to the user's account;
// else the user's (it has read:catalog).
func hcCatalogClient(user *hardcover.Client) *hardcover.Client {
	if hardcoverEnabled() {
		return hcClient
	}
	return user
}

// hcUserMatch: the Hardcover book userID chose for uuid, if any.
func hcUserMatch(userID int64, uuid string) (hardcover.Edition, bool) {
	var ed hardcover.Edition
	err := appDB.QueryRow(`SELECT hc_book_id, hc_edition_id, pages, slug, title FROM hardcover_user_books
		WHERE user_id = ? AND book_uuid = ?`, userID, uuid).Scan(&ed.BookID, &ed.EditionID, &ed.Pages, &ed.Slug, &ed.Title)
	return ed, err == nil && ed.BookID > 0
}

func hcSetUserMatch(userID int64, uuid string, ed hardcover.Edition) error {
	_, err := appDB.Exec(`INSERT INTO hardcover_user_books (user_id, book_uuid, hc_book_id, hc_edition_id, pages, slug, title, chosen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, book_uuid) DO UPDATE SET hc_book_id = excluded.hc_book_id, hc_edition_id = excluded.hc_edition_id,
			pages = excluded.pages, slug = excluded.slug, title = excluded.title, chosen_at = excluded.chosen_at`,
		userID, uuid, ed.BookID, ed.EditionID, ed.Pages, ed.Slug, ed.Title, hcNow().Unix())
	return err
}

// hcResend sends a book again from scratch: its last error is forgotten.
func hcResend(userID int64, uuid string) {
	_, _ = appDB.Exec(`UPDATE hardcover_sync SET error = '', attempts = 0 WHERE user_id = ? AND book_uuid = ?`, userID, uuid)
	hcSyncTouch(userID, uuid, 0)
}

// hcBookStatus is GET /api/me/books/{uuid}/hardcover.
type hcBookStatus struct {
	Shared bool `json:"shared"`
	All    bool `json:"all"` // every book is shared
	// State: "off" (not shared), "no_status" (shared, but nothing to
	// send: no want to read / reading / read), "waiting", "sent",
	// "failing" (being retried) or "not_found".
	State   string       `json:"state"`
	Error   string       `json:"error,omitempty"`
	RetryAt int64        `json:"retry_at,omitempty"` // unix ms
	SentAt  int64        `json:"sent_at,omitempty"`  // unix ms
	Match   *hcMatchView `json:"match,omitempty"`
	// Query: what to search Hardcover for, to start with.
	Query string `json:"query"`
	// Placeholder: a book not in the library yet, sent as want to read
	// (hardcover_placeholder.go). Its Hardcover book is the library's,
	// not the user's: CanChoose when it has none yet, or for an editor.
	Placeholder bool `json:"placeholder,omitempty"`
	CanChoose   bool `json:"can_choose"`
}

// hcBookStateFor: hcBookState, with what the request's user may change.
func hcBookStateFor(r *http.Request, userID int64, uuid string) hcBookStatus {
	st := hcBookState(userID, uuid)
	st.CanChoose = true
	if p := placeholderByUUID(uuid); p != nil {
		st.Placeholder = true
		st.CanChoose = hcPlaceholderBookID(p) == 0 || canEditLibrary(r)
	}
	return st
}

// hcMatchView: the Hardcover book a library book is sent as.
type hcMatchView struct {
	BookID int64  `json:"book_id"`
	Slug   string `json:"slug,omitempty"`
	Title  string `json:"title,omitempty"`
	Chosen bool   `json:"chosen"` // by the user, not found from the library's identifiers
}

func hcBookState(userID int64, uuid string) hcBookStatus {
	st := hcBookStatus{Shared: hcShared(userID, uuid), All: hcLoadShare(userID).All, State: "off"}
	if title, authors, ok := hcBookTitle(uuid); ok {
		st.Query = strings.TrimSpace(title + " " + firstAuthor(authors))
	}
	if ed, ok := hcUserMatch(userID, uuid); ok {
		st.Match = &hcMatchView{BookID: ed.BookID, Slug: ed.Slug, Title: ed.Title, Chosen: true}
	} else {
		var ed hardcover.Edition
		if appDB.QueryRow(`SELECT hc_book_id, slug, title FROM hardcover_books WHERE book_uuid = ? AND hc_book_id > 0`, uuid).
			Scan(&ed.BookID, &ed.Slug, &ed.Title) == nil {
			st.Match = &hcMatchView{BookID: ed.BookID, Slug: ed.Slug, Title: ed.Title}
		} else if p := placeholderByUUID(uuid); p != nil && hcPlaceholderBookID(p) > 0 {
			// Not looked up yet: as the placeholder names it.
			st.Match = &hcMatchView{BookID: hcPlaceholderBookID(p), Slug: hcBookSlug(p.Meta), Title: p.Meta.Title}
		}
	}
	if !st.Shared {
		return st
	}
	var due, sent int64
	var attempts int
	var msg string
	err := appDB.QueryRow(`SELECT due_at, attempts, error, sent_at FROM hardcover_sync WHERE user_id = ? AND book_uuid = ?`,
		userID, uuid).Scan(&due, &attempts, &msg, &sent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Println("hardcover book:", err)
	}
	if sent > 0 {
		st.SentAt = sent * 1000
	}
	status, _ := readStatus(userID, uuid)
	_, sendable := hcStatusIDs[status]
	switch {
	case due == 0 && hcIsNotFound(msg):
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

// hcFoundBook is one search result.
type hcFoundBook struct {
	BookID  int64  `json:"book_id"`
	Slug    string `json:"slug,omitempty"`
	Title   string `json:"title"`
	Authors string `json:"authors,omitempty"`
	Year    string `json:"year,omitempty"`
}

// hcLinkTarget: a Hardcover book named by a pasted link
// (https://hardcover.app/books/<slug>), or by its id.
func hcLinkTarget(q string) (slug string, id int64, ok bool) {
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

// hcBookFor answers for a book the user shares on a linked account; else
// it writes the error and answers false.
func hcBookFor(w http.ResponseWriter, r *http.Request) (*User, string, bool) {
	u := currentUser(r)
	uuid, ok := requireSyncBook(w, r)
	if !ok {
		return nil, "", false
	}
	if !hardcoverSyncEnabled() || !hcIsLinked(u.ID) {
		http.Error(w, "Link Hardcover on your account page first.", http.StatusConflict)
		return nil, "", false
	}
	return u, uuid, true
}

// hardcoverBookStatusHandler: GET /api/me/books/{uuid}/hardcover.
func hardcoverBookStatusHandler(w http.ResponseWriter, r *http.Request) {
	u, uuid, ok := hcBookFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, hcBookStateFor(r, u.ID, uuid))
}

// hardcoverBookSearchHandler: GET /api/me/books/{uuid}/hardcover/search?q=
// Only for a book the user shares: a search tells Hardcover what it's for.
func hardcoverBookSearchHandler(w http.ResponseWriter, r *http.Request) {
	u, uuid, ok := hcBookFor(w, r)
	if !ok {
		return
	}
	if !hcShared(u.ID, uuid) {
		http.Error(w, "Turn on Sync to Hardcover first.", http.StatusConflict)
		return
	}
	q := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if q == "" || len(q) > 200 {
		http.Error(w, "Type a title, or paste the book's link on hardcover.app.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	c := hcCatalogClient(hcUserClient(u.ID))
	out := []hcFoundBook{}
	if slug, id, isLink := hcLinkTarget(q); isLink {
		ed, found, err := c.FindEdition(ctx, nil, id, slug)
		if err != nil {
			hcSearchFailed(w, err)
			return
		}
		if found {
			out = append(out, hcFoundBook{BookID: ed.BookID, Slug: ed.Slug, Title: ed.Title})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	books, err := c.LookupBooks(ctx, hardcover.BookQuery{Title: q})
	if err != nil {
		hcSearchFailed(w, err)
		return
	}
	for _, b := range books {
		if b.ID <= 0 {
			continue
		}
		year, _, _ := strings.Cut(b.Released, "-")
		out = append(out, hcFoundBook{BookID: b.ID, Slug: b.Slug, Title: b.Title, Authors: strings.Join(b.Authors, ", "), Year: year})
	}
	writeJSON(w, http.StatusOK, out)
}

func hcSearchFailed(w http.ResponseWriter, err error) {
	log.Println("hardcover search:", err)
	var wait *hardcover.WaitError
	if errors.As(err, &wait) {
		http.Error(w, "Hardcover is busy; try again in a minute.", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "Couldn't search Hardcover.", http.StatusBadGateway)
}

// hardcoverBookMatchHandler: PUT /api/me/books/{uuid}/hardcover/match
// {"book_id": n} sends the book as that Hardcover book (for this user);
// DELETE goes back to the match from the library's identifiers.
func hardcoverBookMatchHandler(w http.ResponseWriter, r *http.Request) {
	u, uuid, ok := hcBookFor(w, r)
	if !ok {
		return
	}
	if !hcShared(u.ID, uuid) {
		http.Error(w, "Turn on Sync to Hardcover first.", http.StatusConflict)
		return
	}
	ph := placeholderByUUID(uuid)
	if ph != nil && r.Method == http.MethodDelete {
		http.Error(w, "A placeholder's Hardcover book is the library's: choose another instead.", http.StatusBadRequest)
		return
	}
	editor := canEditLibrary(r)
	if ph != nil && !editor && hcPlaceholderBookID(ph) > 0 {
		http.Error(w, "This book already has its Hardcover book; an editor can change it.", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		if _, err := appDB.Exec(`DELETE FROM hardcover_user_books WHERE user_id = ? AND book_uuid = ?`, u.ID, uuid); err != nil {
			log.Println("hardcover match:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		hcResend(u.ID, uuid)
		writeJSON(w, http.StatusOK, hcBookStateFor(r, u.ID, uuid))
		return
	}
	var req struct {
		BookID int64 `json:"book_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.BookID <= 0 {
		http.Error(w, `need {"book_id": n}`, http.StatusBadRequest)
		return
	}
	// Hardcover says what the book is (its edition, pages and title):
	// nothing of it is taken from the request but the id.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ed, found, err := hcCatalogClient(hcUserClient(u.ID)).FindEdition(ctx, nil, req.BookID, "")
	if err != nil {
		hcSearchFailed(w, err)
		return
	}
	if !found {
		http.Error(w, "Hardcover has no such book.", http.StatusNotFound)
		return
	}
	if ph != nil {
		// The placeholder's own identifiers, for everyone: the first to
		// choose names it; after that, only an editor changes it.
		if err := hcSetPlaceholderBook(uuid, ed, u, !editor, u.ID); errors.Is(err, errHCPlaceholderSet) {
			http.Error(w, "Someone chose its Hardcover book a moment ago; an editor can change it.", http.StatusConflict)
			return
		} else if err != nil {
			log.Println("hardcover match:", err)
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
	} else if err := hcSetUserMatch(u.ID, uuid, ed); err != nil {
		log.Println("hardcover match:", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	hcResend(u.ID, uuid)
	writeJSON(w, http.StatusOK, hcBookStateFor(r, u.ID, uuid))
}

// hcProblemBooks: the books userID shares that can't be sent (not found,
// or failing), for the account page.
func hcProblemBooks(userID int64) []hcSharedBook {
	rows, err := appDB.Query(`SELECT book_uuid FROM hardcover_sync WHERE user_id = ? AND error != '' ORDER BY book_uuid`, userID)
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
	var out []hcSharedBook
	for _, u := range uuids {
		if title, authors, ok := hcBookTitle(u); ok {
			out = append(out, hcSharedBook{UUID: u, Title: title, Authors: authors})
		}
	}
	return out
}

// hcBookTitle: a library book's title and authors ("A & B"), from the
// catalog, or the library for a book added a moment ago.
func hcBookTitle(uuid string) (title, authors string, ok bool) {
	if b, ok := currentCatalog().lookup(uuid); ok {
		return b.Title, b.Authors, true
	}
	if p := placeholderByUUID(uuid); p != nil {
		return p.Meta.Title, p.authors(), true
	}
	err := calibreDB.QueryRow(`SELECT b.title, COALESCE((SELECT group_concat(a.name, ' & ') FROM books_authors_link l
		JOIN authors a ON a.id = l.author WHERE l.book = b.id), '') FROM books b WHERE b.uuid = ?`, uuid).Scan(&title, &authors)
	return title, authors, err == nil
}
