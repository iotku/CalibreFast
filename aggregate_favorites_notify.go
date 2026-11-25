package main

// News for users who starred an author or series (aggregate_favorites.go):
// when the library's changes (catalogFlush) give a book an author or series
// it didn't have before, each user who starred it gets a notification
// (notifications.go).
//
//   - Gained is worked out from the catalog before and after the batch, so
//     every write path counts the same: an upload or import (a book that
//     wasn't there gains all its authors and its series), an edit adding an
//     author or changing the series, a merge.
//   - A book restored from the Recycling Bin was a tombstone in the old
//     catalog, with the same authors: it gains nothing. Neither does the
//     startup build (there's no "before"), a removal, or an author dropped.
//   - Per user, a book counts once: under its series if they starred that
//     (and the series is what it gained), else under the first starred
//     author it gained. A batch makes one notification per author or
//     series, with how many books ("3 new books by …").
//   - A book the user already has a notification about (say, the saved
//     placeholder it replaced), even a dismissed one, isn't announced again.
//   - Users told the same books share one event (notifications.go).
//   - The person who made the change is told too, if they starred it: the
//     batch doesn't know who that was.

import (
	"log"
	"slices"
	"strings"
	"sync"
)

// favNotifyWG: notifications being written (tests wait on it).
var favNotifyWG sync.WaitGroup

// favGain: a book's authors and series new to it in a batch, as browsed.
type favGain struct {
	uuid, title string
	authors     []string
	series      string
}

// browsedAuthorsSeries: b's authors and series as browsed (after groups).
func browsedAuthorsSeries(c *catalog, b *catalogBook) (authors []string, series string) {
	for _, id := range b.AuthorIDs {
		n, _, _ := groups.resolve("author", c.authorNames[id])
		authors = append(authors, n)
	}
	if b.Series != 0 {
		series, _, _ = groups.resolve("series", strings.TrimSpace(c.seriesNames[b.Series]))
	}
	return authors, series
}

// favoritesCatalogChanged: next rereads the books in uuids (catalogFlush).
// What they gained is worked out here; telling users is done in the
// background, so the flush doesn't wait on the app database.
func favoritesCatalogChanged(old, next *catalog, uuids []string) {
	if appDB == nil || old == nil || next == nil {
		return
	}
	var gains []favGain
	for _, u := range uuids {
		nb, ok := next.lookup(u)
		if !ok {
			continue
		}
		authors, series := browsedAuthorsSeries(next, &nb)
		var oldAuthors []string
		oldSeries := ""
		if i, ok := old.byUUID[u]; ok { // a tombstone counts: restoring isn't news
			oldAuthors, oldSeries = browsedAuthorsSeries(old, &old.books[i])
		}
		g := favGain{uuid: u, title: nb.Title}
		for _, a := range authors {
			if !slices.ContainsFunc(oldAuthors, func(o string) bool { return strings.EqualFold(o, a) }) {
				g.authors = append(g.authors, a)
			}
		}
		if series != "" && !strings.EqualFold(series, oldSeries) {
			g.series = series
		}
		if len(g.authors) > 0 || g.series != "" {
			gains = append(gains, g)
		}
	}
	if len(gains) == 0 {
		return
	}
	favNotifyWG.Go(func() { logErr(notifyFavoriteGains(gains), "favorites: couldn't notify") })
}

// notifyFavoriteGains tells each user who starred a gained author or
// series, one notification per author or series.
func notifyFavoriteGains(gains []favGain) error {
	// Stars imported from Hardcover waiting for these authors are theirs
	// now (internal/hardcoversync/follows.go), so their users are told too.
	var gainedAuthors []string
	for _, g := range gains {
		gainedAuthors = append(gainedAuthors, g.authors...)
	}
	if err := hcSync.WakeWaitingStars(gainedAuthors); err != nil {
		log.Printf("favorites: waiting stars: %v", err)
	}
	rows, err := appDB.Query(`SELECT user_id, kind, name FROM aggregate_favorites
		WHERE removed_at IS NULL AND kind IN ('author', 'series')`)
	if err != nil {
		return err
	}
	fans := map[string][]int64{} // kind + "\x00" + lower-case name as browsed
	for rows.Next() {
		var uid int64
		var kind, name string
		if err := rows.Scan(&uid, &kind, &name); err != nil {
			rows.Close()
			return err
		}
		g, _, _ := groups.resolve(kind, name)
		k := kind + "\x00" + strings.ToLower(g)
		if !slices.Contains(fans[k], uid) {
			fans[k] = append(fans[k], uid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(fans) == 0 {
		return err
	}

	// Per user, the books of each author or series they hear about.
	byUser := map[int64][]*favNews{}
	add := func(uid int64, kind, subject string, g favGain) {
		for _, n := range byUser[uid] {
			if n.kind == kind && strings.EqualFold(n.subject, subject) {
				n.books = append(n.books, g)
				return
			}
		}
		byUser[uid] = append(byUser[uid], &favNews{kind, subject, []favGain{g}})
	}
	for _, g := range gains {
		told := map[int64]bool{}
		if g.series != "" {
			for _, uid := range fans["series\x00"+strings.ToLower(g.series)] {
				add(uid, notifyFavoriteSeries, g.series, g)
				told[uid] = true
			}
		}
		for _, a := range g.authors {
			for _, uid := range fans["author\x00"+strings.ToLower(a)] {
				if !told[uid] {
					add(uid, notifyFavoriteAuthor, a, g)
					told[uid] = true
				}
			}
		}
	}

	return notifyUserFavorites(byUser)
}

// favNews: the books of one author or series a user hears about.
type favNews struct {
	kind, subject string // notifyFavoriteAuthor or notifyFavoriteSeries; its name
	books         []favGain
}

// notifyUserFavorites stores the batch's notifications, leaving out for
// each user the books they've already been told about (dismissed ones
// too: a book is announced once). Users told the same books of the same
// author or series share one event (notifications.go).
func notifyUserFavorites(byUser map[int64][]*favNews) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	uids := make([]int64, 0, len(byUser))
	for uid := range byUser {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	events := map[string]int64{} // kind, subject and the books' uuids: its event
	for _, uid := range uids {
		for _, n := range byUser[uid] {
			var fresh []favGain
			for _, b := range n.books {
				var seen int
				if err := tx.QueryRow(`SELECT COUNT(*) FROM user_notifications un JOIN notification_events e ON e.id = un.event_id
					WHERE un.user_id = ? AND e.book_uuid = ?`, uid, b.uuid).Scan(&seen); err != nil {
					return err
				}
				if seen == 0 {
					fresh = append(fresh, b)
				}
			}
			if len(fresh) == 0 {
				continue
			}
			key := n.kind + "\x00" + strings.ToLower(n.subject)
			for _, b := range fresh {
				key += "\x00" + b.uuid
			}
			event, ok := events[key]
			if !ok {
				if event, err = addEvent(tx, notificationEvent{Kind: n.kind, BookUUID: fresh[0].uuid, Title: fresh[0].title, Subject: n.subject, Count: len(fresh)}); err != nil {
					return err
				}
				events[key] = event
			}
			if err := deliver(tx, uid, event, false); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
