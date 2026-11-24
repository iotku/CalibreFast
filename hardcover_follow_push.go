package main

// Sending starred authors to Hardcover as follows, and unfollowing there
// when the user asks (see hardcover_follows.go for the other way, and
// docs/notes/Hardcover-favorites.md for the API).
//
//   - Following is the user's choice (hardcover_links.share_follows, off
//     until they turn it on): starring an author here then follows it on
//     Hardcover. Turning it on sends the authors they starred here before
//     (not those brought in from Hardcover, which they follow already).
//   - Unfollowing happens only when the user asks, each time: taking the ★
//     off an author they follow on Hardcover opens a dialog (aggregate-fav.ts)
//     whose "Also unfollow on Hardcover" box starts ticked, or not, as they
//     chose (hardcover_links.unfollow_default). It applies to any follow,
//     whoever made it, since they asked.
//   - Changes wait in hardcover_follow_queue, one row per author (a later
//     change replaces an earlier one), and the reading sync's worker sends
//     them (hcFollowStep), with its limits and its waits after errors.
//   - The author's Hardcover id: the star's (an import or an earlier send
//     stored it), else a lookup's (hardcover_authors), else Hardcover's
//     authors with the star's name or its group members' (AuthorsByName),
//     if exactly one matches as hcNameKey has it. Otherwise the author is set
//     aside, shown on the account page with "Try again".
//   - Before following or unfollowing, Hardcover is asked whether the user
//     follows them, so nothing is followed twice, and a follow made here is
//     noted on the star (hc_ref).

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// hcFollowPrefs: what a user chose about follows on Hardcover.
type hcFollowPrefs struct {
	Linked          bool // a working link
	CanWrite        bool // the link may follow and unfollow (write:social)
	Share           bool // follow the authors starred here
	UnfollowDefault bool // the unstar dialog's box starts ticked
}

func hcLoadFollowPrefs(userID int64) hcFollowPrefs {
	var p hcFollowPrefs
	var scope string
	err := appDB.QueryRow(`SELECT scope, share_follows, unfollow_default FROM hardcover_links WHERE user_id = ? AND broken = ''`, userID).
		Scan(&scope, &p.Share, &p.UnfollowDefault)
	if err != nil {
		return hcFollowPrefs{UnfollowDefault: true}
	}
	p.Linked = hardcoverSyncEnabled()
	p.CanWrite = p.Linked && hardcover.HasScope(scope, "write:social")
	return p
}

// hcSaveFollowPrefs stores userID's choices. Turning following on sends
// the authors they starred here; turning it off drops follows not sent yet.
func hcSaveFollowPrefs(userID int64, share, unfollowDefault bool) error {
	was := hcLoadFollowPrefs(userID).Share
	if _, err := appDB.Exec(`UPDATE hardcover_links SET share_follows = ?, unfollow_default = ? WHERE user_id = ?`, share, unfollowDefault, userID); err != nil {
		return err
	}
	bumpFavs(userID) // the unstar dialog's defaults
	switch {
	case share && !was:
		names, err := scanStrings(appDB.Query(`SELECT name FROM aggregate_favorites WHERE user_id = ? AND kind = 'author'
			AND removed_at IS NULL AND origin = 'local' AND hc_ref IS NULL`, userID))
		if err != nil {
			return err
		}
		for _, n := range names {
			hcQueueFollow(userID, n, "follow", 0)
		}
	case !share:
		_, err := appDB.Exec(`DELETE FROM hardcover_follow_queue WHERE user_id = ? AND op = 'follow'`, userID)
		return err
	}
	return nil
}

// hcQueueFollow queues following (op "follow") or unfollowing the starred
// author name (as browsed; hcID: its Hardcover id, 0 if not known), if the
// user's link may: following also needs them to have turned it on.
func hcQueueFollow(userID int64, name, op string, hcID int64) {
	p := hcLoadFollowPrefs(userID)
	if !p.CanWrite || (op == "follow" && !p.Share) {
		return
	}
	if _, err := appDB.Exec(`INSERT INTO hardcover_follow_queue (user_id, name, op, hc_id, due_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id, name) DO UPDATE SET op = excluded.op, due_at = excluded.due_at, attempts = 0, error = '',
			hc_id = CASE WHEN excluded.hc_id > 0 THEN excluded.hc_id ELSE hc_id END`,
		userID, name, op, hcID, hcNow().Unix()); err != nil {
		log.Println("hardcover follows: queue:", err)
		return
	}
	hcSyncWake()
}

// hcFollowedHere: for userID's starred authors, which they follow on
// Hardcover as far as is known here (lower-case names): one brought in
// from there, or one whose Hardcover id is known, which is set only when
// it's followed (brought in, chosen, followed from here, or found
// followed already). For the unstar dialog; the worker checks before it
// unfollows.
func hcFollowedHere(userID int64) map[string]bool {
	names, err := scanStrings(appDB.Query(`SELECT name FROM aggregate_favorites WHERE user_id = ? AND kind = 'author' AND removed_at IS NULL
		AND (origin = 'hardcover' OR hc_id IS NOT NULL)`, userID))
	out := map[string]bool{}
	if err != nil {
		return out
	}
	for _, n := range names {
		g, _, _ := groups.resolve("author", n)
		out[strings.ToLower(g)] = true
	}
	return out
}

// hcAuthorID finds the Hardcover id of the starred author name (see the top).
func hcAuthorID(ctx context.Context, c *hardcover.Client, name string) (int64, error) {
	g, members, _ := groups.resolve("author", name)
	var id int64
	if err := appDB.QueryRow(`SELECT hc_id FROM hardcover_authors WHERE name = ? AND hc_id > 0`, g).Scan(&id); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	names := slices.Compact(append([]string{g}, members...))
	keys := map[string]bool{}
	for _, n := range names {
		keys[hcNameKey(n)] = true
	}
	found, err := c.AuthorsByName(ctx, names)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for _, a := range found {
		if keys[hcNameKey(a.Name)] && !slices.Contains(ids, a.ID) {
			ids = append(ids, a.ID)
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return 0, &hcSkip{"not found on Hardcover"}
	default:
		return 0, &hcSkip{"several authors on Hardcover have this name"}
	}
}

// hcPushFollow sends one queued change.
func hcPushFollow(ctx context.Context, userID int64, name, op string, hcID int64) error {
	acc, err := hcLoadAccount(userID)
	if err != nil {
		return err
	}
	if acc == nil || acc.Broken != "" {
		return errHCNotLinked
	}
	p := hcLoadFollowPrefs(userID)
	if !p.CanWrite || (op == "follow" && !p.Share) {
		return nil // no longer allowed: dropped
	}
	c := hcUserClient(userID)
	if op == "follow" {
		var starID sql.NullInt64
		err := appDB.QueryRow(`SELECT hc_id FROM aggregate_favorites WHERE user_id = ? AND kind = 'author' AND name = ? AND removed_at IS NULL`, userID, name).Scan(&starID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // unstarred since
		}
		if err != nil {
			return err
		}
		if hcID == 0 && starID.Valid {
			hcID = starID.Int64
		}
	}
	if hcID == 0 {
		if hcID, err = hcAuthorID(ctx, c, name); err != nil {
			return err
		}
	}
	following, err := c.IsFollowing(ctx, acc.HCUserID, hcID)
	if err != nil {
		return err
	}
	if op == "unfollow" {
		if following > 0 {
			if err := c.UnfollowAuthor(ctx, hcID); err != nil {
				return err
			}
		}
		if _, err := appDB.Exec(`DELETE FROM hardcover_follows WHERE user_id = ? AND hc_id = ?`, userID, hcID); err != nil {
			return err
		}
		// Not followed any more: a star put back isn't shown as followed.
		_, err = appDB.Exec(`UPDATE aggregate_favorites SET hc_ref = NULL, hc_id = NULL WHERE user_id = ? AND kind = 'author' AND hc_id = ?`, userID, hcID)
		bumpFavs(userID)
		return err
	}
	if following > 0 { // followed there already: not ours
		_, err = appDB.Exec(`UPDATE aggregate_favorites SET hc_id = ? WHERE user_id = ? AND kind = 'author' AND name = ?`, hcID, userID, name)
		bumpFavs(userID) // now known to be followed (the unstar dialog)
		return err
	}
	ref, err := c.FollowAuthor(ctx, hcID)
	if err != nil {
		return err
	}
	_, err = appDB.Exec(`UPDATE aggregate_favorites SET hc_id = ?, hc_ref = ? WHERE user_id = ? AND kind = 'author' AND name = ?`, hcID, ref, userID, name)
	bumpFavs(userID)
	return err
}

// hcFollowStep sends the change due longest among working links, as
// hcSyncStep does for books. ran: one was tried; next: when the next is
// due (zero: none).
func hcFollowStep(ctx context.Context) (ran bool, next time.Time) {
	now := hcNow()
	var userID, hcID int64
	var name, op string
	var attempts int
	err := appDB.QueryRow(`SELECT q.user_id, q.name, q.op, q.hc_id, q.attempts FROM hardcover_follow_queue q
		JOIN hardcover_links l ON l.user_id = q.user_id AND l.broken = ''
		JOIN users u ON u.id = q.user_id AND u.deleted_at IS NULL
		WHERE q.due_at > 0 AND q.due_at <= ? ORDER BY q.due_at LIMIT 1`, now.Unix()).Scan(&userID, &name, &op, &hcID, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		var nb sql.NullInt64
		_ = appDB.QueryRow(`SELECT MIN(q.due_at) FROM hardcover_follow_queue q
			JOIN hardcover_links l ON l.user_id = q.user_id AND l.broken = ''
			JOIN users u ON u.id = q.user_id AND u.deleted_at IS NULL WHERE q.due_at > 0`).Scan(&nb)
		if nb.Valid {
			return false, time.Unix(nb.Int64, 0)
		}
		return false, time.Time{}
	}
	if err != nil {
		log.Println("hardcover follows:", err)
		return false, now.Add(time.Minute)
	}
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	err = hcPushFollow(pctx, userID, name, op, hcID)
	cancel()
	var wait *hardcover.WaitError
	var skip *hcSkip
	switch {
	case ctx.Err() != nil:
	case err == nil, errors.Is(err, errHCNotLinked), errors.Is(err, hardcover.ErrSignedOut):
		_, err = appDB.Exec(`DELETE FROM hardcover_follow_queue WHERE user_id = ? AND name = ? AND op = ?`, userID, name, op)
	case errors.As(err, &wait):
		_, err = appDB.Exec(`UPDATE hardcover_follow_queue SET due_at = ? WHERE user_id = ? AND due_at > 0 AND due_at < ?`,
			wait.Until.Unix(), userID, wait.Until.Unix())
	case errors.As(err, &skip):
		log.Printf("hardcover follows: user %d, %q: set aside: %s", userID, name, skip.why)
		_, err = appDB.Exec(`UPDATE hardcover_follow_queue SET due_at = 0, attempts = 0, error = ? WHERE user_id = ? AND name = ?`, skip.why, userID, name)
	default:
		backoff := min(time.Minute*time.Duration(math.Pow(4, float64(min(attempts, 6)))), hcMaxBackoff)
		msg := err.Error()
		log.Printf("hardcover follows: user %d, %q: %s; trying again in %s", userID, name, msg, backoff)
		_, err = appDB.Exec(`UPDATE hardcover_follow_queue SET due_at = ?, attempts = attempts + 1, error = ? WHERE user_id = ? AND name = ?`,
			now.Add(backoff).Unix(), msg, userID, name)
	}
	if err != nil {
		log.Println("hardcover follows:", err)
	}
	return true, time.Time{}
}

// hcFollowProblem: an author that couldn't be sent, for the account page.
type hcFollowProblem struct {
	Name, Op, Error string
}

// hcFollowQueueView: what waits to be sent for userID, and what was set aside.
func hcFollowQueueView(userID int64) (waiting int, problems []hcFollowProblem, followedHere int) {
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_follow_queue WHERE user_id = ? AND due_at > 0`, userID).Scan(&waiting)
	_ = appDB.QueryRow(`SELECT COUNT(*) FROM aggregate_favorites WHERE user_id = ? AND kind = 'author' AND removed_at IS NULL AND hc_ref IS NOT NULL`, userID).Scan(&followedHere)
	rows, err := appDB.Query(`SELECT name, op, error FROM hardcover_follow_queue WHERE user_id = ? AND due_at = 0 ORDER BY name COLLATE NOCASE`, userID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p hcFollowProblem
		if rows.Scan(&p.Name, &p.Op, &p.Error) == nil {
			problems = append(problems, p)
		}
	}
	return
}

// hcRetryFollow: "Try again" on an author set aside.
func hcRetryFollow(userID int64, name string) error {
	if _, err := appDB.Exec(`UPDATE hardcover_follow_queue SET due_at = ?, attempts = 0, error = '' WHERE user_id = ? AND name = ? AND due_at = 0`,
		hcNow().Unix(), userID, name); err != nil {
		return err
	}
	hcSyncWake()
	return nil
}
