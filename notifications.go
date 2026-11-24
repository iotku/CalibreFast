package main

// Notifications: the bar's bell counts the unseen ones, /me/notifications
// lists them. Two kinds of news:
//
//   - A saved placeholder is in the library now (below).
//   - An author or series the user starred gained a book: one notification
//     per author or series per batch of changes, with how many books
//     (favorite_author / favorite_series, aggregate_favorites_notify.go).
//   - Their Hardcover link was made before linking asked for something new:
//     link again to use it (hardcover_relink, internal/hardcoversync/relink.go).
//
// Saving books that aren't in the library yet, and being told when they are.
//
// A placeholder (placeholders.go) can be Saved like a library book: its
// reading_status row is keyed by the placeholder's uuid, status "wanted"
// only (nothing else makes sense for a book that can't be opened). It shows
// in Saved and in Discover's "On deck / Saved for later" row as the
// placeholder's card (booksByUUIDs), and isn't downloaded (prefs.go skips
// placeholders).
//
// When a library book replaces the placeholder (replacePlaceholder), each
// user who saved it
//   - gets a notification (this file's table, appdb.go migration 39): the
//     bar's bell shows how many are unseen, /me/notifications lists them;
//   - has the library book Saved instead, unless they already gave it a
//     status of their own;
//   - and, if they finished the book before it in the series, finds it On
//     Deck: nextInSeries shows the next unfinished book after the furthest
//     finished one, which this now is; any old dismissal of it is cleared.
//
// Routes:
//   GET    /me/notifications              the page
//   GET    /api/me/notifications          the list, newest first
//   POST   /api/me/notifications/seen     all seen (the bell's count goes)
//   DELETE /api/me/notifications/{id}     one dismissed
//   DELETE /api/me/notifications          all dismissed

import (
	"database/sql"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	notifyPlaceholderAdded = "placeholder_added"
	notifyFavoriteAuthor   = "favorite_author"
	notifyFavoriteSeries   = "favorite_series"
	notifyHardcoverRelink  = "hardcover_relink"
	// notificationsMax: the most kept per user; older ones are dropped.
	notificationsMax = 200
)

// notification is one, as the page and API show it.
type notification struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	BookUUID  string `json:"book_uuid"`
	Title     string `json:"title"`
	OnDeck    bool   `json:"on_deck"`
	CreatedAt int64  `json:"created_at"` // unix seconds
	Seen      bool   `json:"seen"`
	Href      string `json:"href"`
	// Subject: the author or series a favorite_* notification is about;
	// Count: how many of its books are new (BookUUID and Title: one of them).
	Subject string `json:"subject,omitempty"`
	Count   int    `json:"count"`
}

// addNotification stores one for userID, dropping the oldest beyond
// notificationsMax. subject and count: see notification.
func addNotification(ex execer, userID int64, kind, bookUUID, title string, onDeck bool, subject string, count int) error {
	if _, err := ex.Exec(`INSERT INTO notifications (user_id, kind, book_uuid, title, on_deck, subject, count, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, kind, bookUUID, title, onDeck, subject, count, nowUnix()); err != nil {
		return err
	}
	_, err := ex.Exec(`DELETE FROM notifications WHERE user_id = ? AND id NOT IN
		(SELECT id FROM notifications WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?)`, userID, userID, notificationsMax)
	return err
}

// unseenNotifications: how many the bar's bell counts.
func unseenNotifications(userID int64) int {
	var n int
	if appDB == nil || appDB.QueryRow(`SELECT COUNT(*) FROM notifications WHERE user_id = ? AND seen_at IS NULL`, userID).Scan(&n) != nil {
		return 0
	}
	return n
}

func userNotifications(userID int64) ([]notification, error) {
	rows, err := appDB.Query(`SELECT id, kind, book_uuid, title, on_deck, subject, count, created_at, seen_at IS NOT NULL
		FROM notifications WHERE user_id = ? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notification{}
	for rows.Next() {
		var n notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.BookUUID, &n.Title, &n.OnDeck, &n.Subject, &n.Count, &n.CreatedAt, &n.Seen); err != nil {
			return nil, err
		}
		switch {
		case n.Kind == notifyHardcoverRelink:
			n.Href = "/account#hardcover"
		case n.Count > 1 && n.Kind == notifyFavoriteAuthor:
			n.Href = categoryByKey("author").href(n.Subject) // several: the author's page
		case n.Count > 1 && n.Kind == notifyFavoriteSeries:
			n.Href = categoryByKey("series").href(n.Subject)
		case n.BookUUID != "":
			n.Href = "/book/get/" + url.PathEscape(n.BookUUID)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// A saved placeholder replaced by a library book
// ---------------------------------------------------------------------------

// placeholderSavers: the users who saved placeholder id.
func placeholderSavers(id string) ([]int64, error) {
	rows, err := appDB.Query(`SELECT user_id FROM reading_status WHERE book_uuid = ? AND status = 'wanted'`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			ids = append(ids, u)
		}
	}
	return ids, rows.Err()
}

// finishedBookBefore: userID finished the library book just before
// placeholder p in its series (the one with the highest number below p's).
func finishedBookBefore(userID int64, p *placeholder) bool {
	series := strings.TrimSpace(p.Meta.Series)
	if series == "" {
		return false
	}
	at := p.index()
	c := currentCatalog()
	prev := -1.0
	var prevUUIDs []string
	for i := range c.books {
		b := &c.books[i]
		if b.Removed || b.Series == 0 || b.SeriesIndex >= at || !strings.EqualFold(strings.TrimSpace(c.seriesNames[b.Series]), series) {
			continue
		}
		switch {
		case b.SeriesIndex > prev:
			prev, prevUUIDs = b.SeriesIndex, []string{b.UUID}
		case b.SeriesIndex == prev:
			prevUUIDs = append(prevUUIDs, b.UUID) // two editions of it: either will do
		}
	}
	for _, u := range prevUUIDs {
		if s, _ := readStatus(userID, u); s == "finished" {
			return true
		}
	}
	return false
}

// placeholderReplacedForSavers: library book bookUUID has replaced
// placeholder p. Its savers are told, and have the book Saved, or On Deck
// (see the top of the file).
func placeholderReplacedForSavers(p *placeholder, bookUUID string) {
	if appDB == nil || bookUUID == "" {
		return
	}
	// After the Saved moves to the book: it's sent as that.
	defer placeholderReplacedForSyncs(p.UUID, bookUUID)
	savers, err := placeholderSavers(p.UUID)
	if err != nil {
		log.Printf("placeholder %s: its savers: %v", p.UUID, err)
		return
	}
	for _, uid := range savers {
		if err := moveSavedPlaceholder(uid, p, bookUUID); err != nil {
			log.Printf("placeholder %s: user %d: %v", p.UUID, uid, err)
		}
	}
}

func moveSavedPlaceholder(uid int64, p *placeholder, bookUUID string) error {
	onDeck := finishedBookBefore(uid, p)
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Saved moves to the book, unless it has a status of its own already
	// (read elsewhere, say): that one stands.
	var cur string
	switch err := tx.QueryRow(`SELECT status FROM reading_status WHERE user_id = ? AND book_uuid = ?`, uid, bookUUID).Scan(&cur); {
	case err == sql.ErrNoRows:
		if _, err := tx.Exec(`UPDATE reading_status SET book_uuid = ?, updated_at = ? WHERE user_id = ? AND book_uuid = ?`,
			bookUUID, nowUnix(), uid, p.UUID); err != nil {
			return err
		}
		if _, err := claimMarkChange(tx, uid, bookUUID, "status", nowMilli()); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(`DELETE FROM reading_status WHERE user_id = ? AND book_uuid = ?`, uid, p.UUID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM mark_changes WHERE user_id = ? AND book_uuid = ?`, uid, p.UUID); err != nil {
		return err
	}
	if onDeck {
		if _, err := tx.Exec(`DELETE FROM on_deck_dismissed WHERE user_id = ? AND book_uuid IN (?, ?)`, uid, bookUUID, p.UUID); err != nil {
			return err
		}
	}
	if err := addNotification(tx, uid, notifyPlaceholderAdded, bookUUID, p.Meta.Title, onDeck, "", 1); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// notificationsPageData: what notifications.html shows. Each notice's text
// is built in the template (html/template escapes the title).
type notificationsPageData struct {
	Title         string
	Notifications []notificationView
}

type notificationView struct {
	notification
	When string
}

// GET /me/notifications: the page. Opening it doesn't mark them seen (a GET
// changes nothing); web/notifications.ts does, once it's shown.
func notificationsPage(w http.ResponseWriter, r *http.Request) {
	ns, err := userNotifications(currentUser(r).ID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	d := notificationsPageData{Title: SiteTitle}
	for _, n := range ns {
		d.Notifications = append(d.Notifications, notificationView{n, time.Unix(n.CreatedAt, 0).Format("2 Jan 2006")})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "notifications.html", d), "render notifications")
}

func notificationsHandler(w http.ResponseWriter, r *http.Request) {
	ns, err := userNotifications(currentUser(r).ID)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, ns)
}

func notificationsSeenHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := appDB.Exec(`UPDATE notifications SET seen_at = ? WHERE user_id = ? AND seen_at IS NULL`, nowUnix(), currentUser(r).ID); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func notificationDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := appDB.Exec(`DELETE FROM notifications WHERE id = ? AND user_id = ?`, id, currentUser(r).ID); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func notificationsClearHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := appDB.Exec(`DELETE FROM notifications WHERE user_id = ?`, currentUser(r).ID); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
