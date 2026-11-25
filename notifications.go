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
//   - gets a notification ("Saved book added to library: … by …"; one
//     event for all of them, appdb.go migration 49): the bar's bell shows
//     how many are unseen, /me/notifications lists them;
//   - has the library book Saved instead, unless they already gave it a
//     status of their own;
//   - and, if they finished the book before it in the series, finds it On
//     Deck: nextInSeries shows the next unfinished book after the furthest
//     finished one, which this now is; any old dismissal of it is cleared.
//
// Routes:
//   GET    /me/notifications              the page: active, then Earlier (dismissed)
//   GET    /api/me/notifications          the active ones, newest first
//   POST   /api/me/notifications/seen     all seen (the bell's count goes)
//   DELETE /api/me/notifications/{id}     one dismissed (to history; an account kind's deleted)
//   DELETE /api/me/notifications          all active ones dismissed

import (
	"database/sql"
	"fmt"
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
	// notificationsMax: the most kept per user, active and dismissed
	// together; the oldest dismissed go first.
	notificationsMax = 200
	// notificationsKeepDismissed: how long a dismissed one stays in history.
	notificationsKeepDismissed = 30 * 24 * time.Hour
	// notificationsSweepEvery: how often every user's are pruned
	// (startAccountPurge's ticker runs it).
	notificationsSweepEvery = 24 * time.Hour
)

// accountKinds: notifications about the user's account, asking them to do
// something (link Hardcover again). They aren't news to look back on:
// dismissing one deletes it, and so does the thing being done.
var accountKinds = map[string]bool{notifyHardcoverRelink: true}

// Storage (appdb.go migration 49): an event is stored once in
// notification_events, however many users it reaches; each user's copy is
// a user_notifications row (on_deck, seen_at, dismissed_at). A dismissed one
// is kept as history (the page's "Earlier") for notificationsKeepDismissed,
// within notificationsMax per user; an event nobody has any more goes.

// notification is one, as the page and API show it. ID is the event's.
type notification struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	BookUUID  string `json:"book_uuid"`
	Title     string `json:"title"`
	OnDeck    bool   `json:"on_deck"`
	CreatedAt int64  `json:"created_at"` // unix seconds
	Seen      bool   `json:"seen"`
	Href      string `json:"href"`
	// Authors: a placeholder_added book's authors ("A & B"), as saved.
	Authors string `json:"authors,omitempty"`
	// Subject: the author or series a favorite_* notification is about;
	// Count: how many of its books are new (BookUUID and Title: one of them).
	Subject string `json:"subject,omitempty"`
	Count   int    `json:"count"`
}

// notificationEvent: what addEvent stores, once for all its users.
type notificationEvent struct {
	Kind, BookUUID, Title, Authors, Subject string
	Count                                   int
}

// addEvent stores an event, returning its id; deliver gives it to users.
func addEvent(ex execer, e notificationEvent) (int64, error) {
	if e.Count < 1 {
		e.Count = 1
	}
	res, err := ex.Exec(`INSERT INTO notification_events (kind, book_uuid, title, authors, subject, count, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.Kind, e.BookUUID, e.Title, e.Authors, e.Subject, e.Count, nowUnix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// deliver gives event eventID to userID (onDeck: theirs), then prunes
// theirs (pruneUserNotifications). Delivered twice, it's once.
func deliver(ex execer, userID, eventID int64, onDeck bool) error {
	if _, err := ex.Exec(`INSERT OR IGNORE INTO user_notifications (user_id, event_id, on_deck, created_at) VALUES (?, ?, ?, ?)`,
		userID, eventID, onDeck, nowUnix()); err != nil {
		return err
	}
	pushNotify(ex, userID) // sent after pushDelay, once this transaction is done
	now := time.Now()
	pruned, err := pruneUserNotifications(ex, userID, now)
	if err != nil || pruned == 0 {
		return err
	}
	return gcNotificationEvents(ex, now)
}

// pruneUserNotifications: userID's dismissed ones older than
// notificationsKeepDismissed go, and beyond notificationsMax the oldest,
// dismissed first. It returns how many went; the events left with nobody
// go in gcNotificationEvents.
func pruneUserNotifications(ex execer, userID int64, now time.Time) (int64, error) {
	res, err := ex.Exec(`DELETE FROM user_notifications WHERE user_id = ? AND dismissed_at IS NOT NULL AND dismissed_at < ?`,
		userID, now.Add(-notificationsKeepDismissed).Unix())
	if err != nil {
		return 0, err
	}
	old, _ := res.RowsAffected()
	res, err = ex.Exec(`DELETE FROM user_notifications WHERE user_id = ? AND event_id NOT IN
		(SELECT event_id FROM user_notifications WHERE user_id = ?
		 ORDER BY dismissed_at IS NULL DESC, created_at DESC, event_id DESC LIMIT ?)`, userID, userID, notificationsMax)
	if err != nil {
		return 0, err
	}
	over, _ := res.RowsAffected()
	return old + over, nil
}

// eventGCGrace: an event this new is kept with nobody, as it may be about
// to be delivered (placeholderReplacedForSavers stores it first).
const eventGCGrace = time.Hour

// gcNotificationEvents deletes the events no user has any more.
func gcNotificationEvents(ex execer, now time.Time) error {
	_, err := ex.Exec(`DELETE FROM notification_events WHERE created_at < ? AND NOT EXISTS
		(SELECT 1 FROM user_notifications WHERE event_id = notification_events.id)`, now.Add(-eventGCGrace).Unix())
	return err
}

// sweepNotifications prunes every user's (pruneUserNotifications) and the
// events left with nobody.
func sweepNotifications(now time.Time) error {
	if appDB == nil {
		return nil
	}
	rows, err := appDB.Query(`SELECT DISTINCT user_id FROM user_notifications`)
	if err != nil {
		return err
	}
	var users []int64
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, u := range users {
		if _, err := pruneUserNotifications(appDB, u, now); err != nil {
			return err
		}
	}
	return gcNotificationEvents(appDB, now)
}

// notificationsSweptAt: when maybeSweepNotifications last swept.
var notificationsSweptAt time.Time

// maybeSweepNotifications runs sweepNotifications once every
// notificationsSweepEvery; the account purge's ticker calls it
// (account_delete.go), from one goroutine.
func maybeSweepNotifications(now time.Time) {
	if !notificationsSweptAt.IsZero() && now.Sub(notificationsSweptAt) < notificationsSweepEvery {
		return
	}
	notificationsSweptAt = now
	logErr(sweepNotifications(now), "notifications: sweep")
}

// deleteUserNotificationsOfKind deletes userID's notifications of kind (an
// account kind: those don't go to history) with their events. An account
// kind's event is the user's own (ReplaceRelinkNotice), so nobody else's goes.
func deleteUserNotificationsOfKind(ex execer, userID int64, kind string) error {
	if !accountKinds[kind] {
		return fmt.Errorf("notifications: %q isn't an account kind", kind)
	}
	_, err := ex.Exec(`DELETE FROM notification_events WHERE kind = ? AND id IN
		(SELECT event_id FROM user_notifications WHERE user_id = ?)`, kind, userID)
	return err // their user_notifications go with them (ON DELETE CASCADE)
}

// unseenNotifications: how many the bar's bell counts.
func unseenNotifications(userID int64) int {
	var n int
	if appDB == nil || appDB.QueryRow(`SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND seen_at IS NULL AND dismissed_at IS NULL`, userID).Scan(&n) != nil {
		return 0
	}
	return n
}

// userNotifications: userID's active ones (not dismissed), newest first.
func userNotifications(userID int64) ([]notification, error) {
	return queryNotifications(`un.dismissed_at IS NULL ORDER BY un.created_at DESC, un.event_id DESC`, userID)
}

// dismissedNotifications: userID's history, the most recently dismissed first.
func dismissedNotifications(userID int64) ([]notification, error) {
	return queryNotifications(`un.dismissed_at IS NOT NULL ORDER BY un.dismissed_at DESC, un.created_at DESC, un.event_id DESC`, userID)
}

// queryNotifications: userID's, filtered and ordered by tail (a fixed
// string from the two above, never data).
func queryNotifications(tail string, userID int64) ([]notification, error) {
	rows, err := appDB.Query(`SELECT e.id, e.kind, e.book_uuid, e.title, e.authors, un.on_deck, e.subject, e.count, un.created_at, un.seen_at IS NOT NULL
		FROM user_notifications un JOIN notification_events e ON e.id = un.event_id
		WHERE un.user_id = ? AND `+tail, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []notification{}
	for rows.Next() {
		var n notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.BookUUID, &n.Title, &n.Authors, &n.OnDeck, &n.Subject, &n.Count, &n.CreatedAt, &n.Seen); err != nil {
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
	if len(savers) == 0 {
		return
	}
	// One event for all of them; On deck is each one's own.
	event, err := addEvent(appDB, notificationEvent{Kind: notifyPlaceholderAdded, BookUUID: bookUUID, Title: p.Meta.Title, Authors: p.authors()})
	if err != nil {
		log.Printf("placeholder %s: its notification: %v", p.UUID, err)
		return
	}
	for _, uid := range savers {
		if err := moveSavedPlaceholder(uid, p, bookUUID, event); err != nil {
			log.Printf("placeholder %s: user %d: %v", p.UUID, uid, err)
		}
	}
}

// moveSavedPlaceholder moves uid's Saved from p to bookUUID and delivers
// them event (its notification, placeholderReplacedForSavers).
func moveSavedPlaceholder(uid int64, p *placeholder, bookUUID string, event int64) error {
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
	// Told already that the book is new by an author or in a series they
	// starred (catalogFlush may get there first): this says it better.
	// A notice about several books stays; it's about the others too.
	if _, err := tx.Exec(`DELETE FROM user_notifications WHERE user_id = ? AND event_id IN (
		SELECT id FROM notification_events WHERE book_uuid = ? AND kind IN (?, ?) AND count <= 1)`,
		uid, bookUUID, notifyFavoriteAuthor, notifyFavoriteSeries); err != nil {
		return err
	}
	if err := deliver(tx, uid, event, onDeck); err != nil {
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
	// Earlier: the dismissed ones, kept a while (read only; their links work).
	Earlier []notificationView
}

type notificationView struct {
	notification
	When string
	// Account: an account kind (accountKinds), deleted when dismissed.
	Account bool
}

func notificationViews(ns []notification) []notificationView {
	var out []notificationView
	for _, n := range ns {
		out = append(out, notificationView{n, time.Unix(n.CreatedAt, 0).Format("2 Jan 2006"), accountKinds[n.Kind]})
	}
	return out
}

// GET /me/notifications: the page. Opening it doesn't mark them seen (a GET
// changes nothing); web/notifications.ts does, once it's shown.
func notificationsPage(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	ns, err := userNotifications(uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	earlier, err := dismissedNotifications(uid)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	d := notificationsPageData{Title: SiteTitle, Notifications: notificationViews(ns), Earlier: notificationViews(earlier)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "notifications.html", d), "render notifications")
}

// GET /api/me/notifications: the active ones (the bar's menu).
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
	if _, err := appDB.Exec(`UPDATE user_notifications SET seen_at = ? WHERE user_id = ? AND seen_at IS NULL`, nowUnix(), currentUser(r).ID); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dismissNotifications dismisses userID's active notification eventID, or
// all of them when eventID is 0. Each is kept as history (dismissed_at),
// but an account kind's goes, with its event (the user's own).
func dismissNotifications(userID, eventID int64) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT e.id, e.kind FROM user_notifications un JOIN notification_events e ON e.id = un.event_id
		WHERE un.user_id = ? AND un.dismissed_at IS NULL AND (? = 0 OR e.id = ?)`, userID, eventID, eventID)
	if err != nil {
		return err
	}
	type found struct {
		id      int64
		account bool
	}
	var all []found
	for rows.Next() {
		var f found
		var kind string
		if err := rows.Scan(&f.id, &kind); err != nil {
			rows.Close()
			return err
		}
		f.account = accountKinds[kind]
		all = append(all, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := nowUnix()
	for _, f := range all {
		if f.account {
			// Its user_notifications row goes with it (ON DELETE CASCADE).
			_, err = tx.Exec(`DELETE FROM notification_events WHERE id = ?`, f.id)
		} else {
			_, err = tx.Exec(`UPDATE user_notifications SET dismissed_at = ?, seen_at = COALESCE(seen_at, ?) WHERE user_id = ? AND event_id = ?`,
				now, now, userID, f.id)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DELETE /api/me/notifications/{id}: one dismissed.
func notificationDeleteHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := dismissNotifications(currentUser(r).ID, id); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/me/notifications: all the active ones dismissed.
func notificationsClearHandler(w http.ResponseWriter, r *http.Request) {
	if err := dismissNotifications(currentUser(r).ID, 0); err != nil {
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
