package main

// Importing the authors a user follows on Hardcover as starred authors
// (aggregate_favorites.go), from the account page. Read-only on Hardcover;
// see docs/notes/Hardcover-favorites.md for the API.
//
// Each followed author becomes one of:
//
//   - starred: the library author it is, starred. Matched by the Hardcover
//     id an author lookup stored (hardcover_authors.hc_id), else by name as
//     hcNameKey has it (case, accents, periods, word order and "Last,
//     First" don't count), through groups, so it stars the author as
//     browsed. A choice the user made (below) comes first.
//   - waiting: no such author here: starred under Hardcover's name all the
//     same. Such a star isn't listed while the library has no books by that
//     name; when one arrives (or an author whose name matches the same way:
//     hcWakeWaitingStars), the star becomes the library author's and the
//     user is told (aggregate_favorites_notify.go).
//   - confirm: only near matches (the same surname and first initial, or
//     several authors matching): the user picks one, or says it's none of
//     them, on the account page. Each choice is kept
//     (hardcover_aggregate_matches) and can be undone.
//   - skipped: the user had starred it and taken the star off here: a
//     later import doesn't put it back (the star's tombstone,
//     aggregate_favorites.removed_at).
//
// Stars made this way are origin 'hardcover' with the author's Hardcover id
// (hc_id). Importing only adds: an author unfollowed on Hardcover keeps its
// star. hardcover_follows keeps the last import for the account page.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// errHCFollowScope: the link predates follows: it has to be made again.
var errHCFollowScope = errors.New("link Hardcover again to allow reading the authors you follow")

// hcFollowsImportTime bounds an import (each thousand follows is a request).
const hcFollowsImportTime = time.Minute

// hcAuthorIndex: the library's authors as browsed, by hcNameKey of their
// name and their group members' names, and by surname and first initial.
type hcAuthorIndex struct {
	exact map[string][]string
	loose map[string][]string
}

// hcLooseKey: surname and first initial ("tolkien j"), or "" for a name of
// one word.
func hcLooseKey(name string) string {
	if last, first, ok := strings.Cut(name, ","); ok {
		name = first + " " + last
	}
	words := strings.Fields(foldWords(strings.ReplaceAll(name, ".", " ")))
	if len(words) < 2 {
		return ""
	}
	return words[len(words)-1] + " " + words[0][:1]
}

func libraryAuthorIndex() hcAuthorIndex {
	ix := hcAuthorIndex{exact: map[string][]string{}, loose: map[string][]string{}}
	add := func(m map[string][]string, k, name string) {
		if k != "" && !slices.Contains(m[k], name) {
			m[k] = append(m[k], name)
		}
	}
	for _, it := range getAggregateData(categoryByKey("author")).All {
		for _, n := range append([]string{it.Name}, it.Members...) {
			add(ix.exact, hcNameKey(n), it.Name)
			add(ix.loose, hcLooseKey(n), it.Name)
		}
	}
	return ix
}

// hcFollowOutcome: what a followed author becomes (see the top).
type hcFollowOutcome struct {
	state      string // starred, waiting, confirm (skipped: after hcStarFollow)
	local      string // the name to star
	candidates []string
}

// hcClassifyFollow decides what f becomes for userID: the user's own
// choice, else the author's stored Hardcover id, else its name.
func hcClassifyFollow(tx *sql.Tx, userID int64, f hardcover.FollowedAuthor, ix hcAuthorIndex) (hcFollowOutcome, error) {
	rows, err := tx.Query(`SELECT COALESCE(local_name, ''), decision FROM hardcover_aggregate_matches
		WHERE user_id = ? AND kind = 'author' AND hc_id = ?`, userID, f.AuthorID)
	if err != nil {
		return hcFollowOutcome{}, err
	}
	rejected := map[string]bool{}
	none := false
	for rows.Next() {
		var local, decision string
		if err := rows.Scan(&local, &decision); err != nil {
			rows.Close()
			return hcFollowOutcome{}, err
		}
		switch {
		case decision == "match":
			rows.Close()
			return hcFollowOutcome{state: "starred", local: local}, nil
		case local == "":
			none = true // "none of these": not in the library
		default:
			rejected[strings.ToLower(local)] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return hcFollowOutcome{}, err
	}
	if none {
		return hcFollowOutcome{state: "waiting", local: f.Name}, nil
	}
	ok := func(names []string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return rejected[strings.ToLower(n)] })
	}
	// A lookup already tied a library author to this Hardcover author.
	var looked string
	if err := tx.QueryRow(`SELECT name FROM hardcover_authors WHERE hc_id = ? LIMIT 1`, f.AuthorID).Scan(&looked); err == nil {
		if v, found := aggValue(categoryByKey("author"), looked); found && !rejected[strings.ToLower(v.Name)] {
			return hcFollowOutcome{state: "starred", local: v.Name}, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return hcFollowOutcome{}, err
	}
	switch exact := ok(ix.exact[hcNameKey(f.Name)]); len(exact) {
	case 1:
		return hcFollowOutcome{state: "starred", local: exact[0]}, nil
	case 0:
	default:
		return hcFollowOutcome{state: "confirm", candidates: exact}, nil
	}
	if near := ok(ix.loose[hcLooseKey(f.Name)]); len(near) > 0 {
		return hcFollowOutcome{state: "confirm", candidates: near}, nil
	}
	return hcFollowOutcome{state: "waiting", local: f.Name}, nil
}

// hcStarFollow stars name for userID as imported from Hardcover author
// hcID. added: a new star; skipped: the user took this star off before.
func hcStarFollow(tx *sql.Tx, userID, hcID int64, name string) (added, skipped bool, err error) {
	g, _, _ := groups.resolve("author", name)
	rows, err := tx.Query(`SELECT name, removed_at IS NOT NULL FROM aggregate_favorites WHERE user_id = ? AND kind = 'author'`, userID)
	if err != nil {
		return false, false, err
	}
	var live []string
	removed := false
	n := 0
	for rows.Next() {
		var have string
		var gone bool
		if err := rows.Scan(&have, &gone); err != nil {
			rows.Close()
			return false, false, err
		}
		if hg, _, _ := groups.resolve("author", have); strings.EqualFold(hg, g) {
			if gone {
				removed = true
			} else {
				live = append(live, have)
			}
		}
		if !gone {
			n++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	switch {
	case len(live) > 0: // starred already: note where on Hardcover it is
		_, err = tx.Exec(`UPDATE aggregate_favorites SET hc_id = COALESCE(hc_id, ?) WHERE user_id = ? AND kind = 'author' AND name = ?`, hcID, userID, live[0])
		return false, false, err
	case removed:
		return false, true, nil
	}
	var total int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM aggregate_favorites WHERE user_id = ? AND removed_at IS NULL`, userID).Scan(&total); err != nil {
		return false, false, err
	}
	if total >= maxAggFavorites {
		return false, true, nil
	}
	_, err = tx.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, origin, hc_id, created_at) VALUES (?, 'author', ?, 'hardcover', ?, ?)`,
		userID, g, hcID, time.Now().UnixMilli())
	return err == nil, false, err
}

// hcApplyFollow classifies f, stars it if that's what it becomes, and
// keeps the outcome in hardcover_follows. added: a star was added.
func hcApplyFollow(tx *sql.Tx, userID int64, f hardcover.FollowedAuthor, ix hcAuthorIndex) (state string, added bool, err error) {
	out, err := hcClassifyFollow(tx, userID, f, ix)
	if err != nil {
		return "", false, err
	}
	if out.state == "starred" || out.state == "waiting" {
		var skipped bool
		if added, skipped, err = hcStarFollow(tx, userID, f.AuthorID, out.local); err != nil {
			return "", false, err
		}
		if skipped {
			out.state = "skipped"
		}
	}
	cands, err := json.Marshal(append([]string{}, out.candidates...))
	if err != nil {
		return "", false, err
	}
	_, err = tx.Exec(`INSERT INTO hardcover_follows (user_id, hc_id, hc_name, follow_id, state, local_name, candidates, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, hc_id) DO UPDATE SET hc_name = excluded.hc_name, follow_id = excluded.follow_id,
			state = excluded.state, local_name = excluded.local_name, candidates = excluded.candidates, seen_at = excluded.seen_at`,
		userID, f.AuthorID, f.Name, f.FollowID, out.state, out.local, string(cands), time.Now().Unix())
	return out.state, added, err
}

// hcFollowsResult: an import's outcome, by what the follows became.
type hcFollowsResult struct {
	Total, Starred, Waiting, Confirm, Skipped, Added int
}

// hcCanReadFollows: userID's link may read their follows.
func hcCanReadFollows(userID int64) bool {
	var scope string
	_ = appDB.QueryRow(`SELECT scope FROM hardcover_links WHERE user_id = ? AND broken = ''`, userID).Scan(&scope)
	return hardcover.HasScope(scope, "read:social")
}

// hcImportFollows reads the authors userID follows on Hardcover and stars
// them here (see the top).
func hcImportFollows(ctx context.Context, userID int64) (hcFollowsResult, error) {
	var res hcFollowsResult
	acc, err := hcLoadAccount(userID)
	if err != nil {
		return res, err
	}
	if acc == nil {
		return res, errors.New("Hardcover isn't linked")
	}
	if !hcCanReadFollows(userID) {
		return res, errHCFollowScope
	}
	ctx, cancel := context.WithTimeout(ctx, hcFollowsImportTime)
	defer cancel()
	follows, err := hcUserClient(userID).FollowedAuthors(ctx, acc.HCUserID)
	if err != nil {
		return res, err
	}
	ix := libraryAuthorIndex()
	tx, err := appDB.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	// The last import's list is replaced: follows gone from Hardcover go
	// from it (their stars stay).
	if _, err := tx.Exec(`DELETE FROM hardcover_follows WHERE user_id = ?`, userID); err != nil {
		return res, err
	}
	for _, f := range follows {
		state, added, err := hcApplyFollow(tx, userID, f, ix)
		if err != nil {
			return res, err
		}
		res.Total++
		if added {
			res.Added++
		}
		switch state {
		case "starred":
			res.Starred++
		case "waiting":
			res.Waiting++
		case "confirm":
			res.Confirm++
		default:
			res.Skipped++
		}
	}
	if err := touchFavs(tx, userID, time.Now().UnixMilli()); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// hcFollowDecision records the user's choice for followed author hcID
// (from the last import): local, a library author offered for it; or ""
// for none of those offered. Undo (undo true) forgets the choices made for
// it, and takes off the star it made. Then the follow is decided again.
func hcFollowDecision(userID, hcID int64, local string, undo bool) error {
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var f hardcover.FollowedAuthor
	var cands string
	err = tx.QueryRow(`SELECT hc_name, follow_id, candidates FROM hardcover_follows WHERE user_id = ? AND hc_id = ?`, userID, hcID).
		Scan(&f.Name, &f.FollowID, &cands)
	if errors.Is(err, sql.ErrNoRows) {
		return errHCNoFollow
	}
	if err != nil {
		return err
	}
	f.AuthorID = hcID
	now := time.Now().Unix()
	switch {
	case undo:
		if _, err := tx.Exec(`DELETE FROM hardcover_aggregate_matches WHERE user_id = ? AND kind = 'author' AND hc_id = ?`, userID, hcID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM aggregate_favorites WHERE user_id = ? AND kind = 'author' AND origin = 'hardcover' AND hc_id = ? AND removed_at IS NULL`, userID, hcID); err != nil {
			return err
		}
	default:
		var offered []string
		_ = json.Unmarshal([]byte(cands), &offered)
		if local != "" && !slices.Contains(offered, local) {
			return errHCNoFollow // only what was offered
		}
		if local != "" {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO hardcover_aggregate_matches (user_id, kind, hc_id, local_name, decision, created_at)
				VALUES (?, 'author', ?, ?, 'match', ?)`, userID, hcID, local, now); err != nil {
				return err
			}
			break
		}
		for _, n := range offered {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO hardcover_aggregate_matches (user_id, kind, hc_id, local_name, decision, created_at)
				VALUES (?, 'author', ?, ?, 'reject', ?)`, userID, hcID, n, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO hardcover_aggregate_matches (user_id, kind, hc_id, local_name, decision, created_at)
			VALUES (?, 'author', ?, NULL, 'reject', ?)`, userID, hcID, now); err != nil {
			return err
		}
	}
	if _, _, err := hcApplyFollow(tx, userID, f, libraryAuthorIndex()); err != nil {
		return err
	}
	if err := touchFavs(tx, userID, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// errHCNoFollow: no such followed author to decide on (or not that choice).
var errHCNoFollow = errors.New("no such followed author")

// hcFollowRow is a followed author on the account page.
type hcFollowRow struct {
	HCID       int64
	Name       string // Hardcover's
	Local      string // its star's name
	Candidates []string
	Decided    bool // a choice the user made (undoable)
}

// hcFollowsView: the last import, for the account page.
type hcFollowsView struct {
	CanRead  bool
	Total    int
	Starred  []hcFollowRow
	Waiting  []hcFollowRow
	Confirm  []hcFollowRow
	Skipped  int
	LastSeen string
}

func hcLoadFollowsView(userID int64) *hcFollowsView {
	v := &hcFollowsView{CanRead: hcCanReadFollows(userID)}
	rows, err := appDB.Query(`SELECT f.hc_id, f.hc_name, f.state, f.local_name, f.candidates, f.seen_at,
			EXISTS (SELECT 1 FROM hardcover_aggregate_matches m WHERE m.user_id = f.user_id AND m.kind = 'author' AND m.hc_id = f.hc_id)
		FROM hardcover_follows f WHERE f.user_id = ? ORDER BY f.hc_name COLLATE NOCASE`, userID)
	if err != nil {
		return v
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var r hcFollowRow
		var state, cands string
		var seen int64
		if rows.Scan(&r.HCID, &r.Name, &state, &r.Local, &cands, &seen, &r.Decided) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(cands), &r.Candidates)
		last = max(last, seen)
		v.Total++
		switch state {
		case "starred":
			v.Starred = append(v.Starred, r)
		case "waiting":
			v.Waiting = append(v.Waiting, r)
		case "confirm":
			v.Confirm = append(v.Confirm, r)
		default:
			v.Skipped++
		}
	}
	if last > 0 {
		v.LastSeen = formatDay(last)
	}
	return v
}

// hcWakeWaitingStars: authors that just got books here (gained), as
// browsed. A star imported from Hardcover under a name the library didn't
// have, matching one of them as an import would (hcNameKey), becomes that
// author's, before anyone is told about the books (notifyFavoriteGains).
func hcWakeWaitingStars(gained []string) error {
	byKey := map[string]string{}
	for _, a := range gained {
		byKey[hcNameKey(a)] = a
	}
	rows, err := appDB.Query(`SELECT user_id, name FROM aggregate_favorites WHERE kind = 'author' AND origin = 'hardcover' AND removed_at IS NULL`)
	if err != nil {
		return err
	}
	type wake struct {
		uid      int64
		from, to string
	}
	var wakes []wake
	for rows.Next() {
		var w wake
		if err := rows.Scan(&w.uid, &w.from); err != nil {
			rows.Close()
			return err
		}
		if to, ok := byKey[hcNameKey(w.from)]; ok && !strings.EqualFold(to, w.from) {
			w.to = to
			wakes = append(wakes, w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(wakes) == 0 {
		return err
	}
	tx, err := appDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, w := range wakes {
		// Starred under the library's name already: the waiting one goes.
		if _, err := tx.Exec(`UPDATE OR IGNORE aggregate_favorites SET name = ? WHERE user_id = ? AND kind = 'author' AND name = ?`, w.to, w.uid, w.from); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM aggregate_favorites WHERE user_id = ? AND kind = 'author' AND name = ? AND origin = 'hardcover'`, w.uid, w.from); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE hardcover_follows SET state = 'starred', local_name = ? WHERE user_id = ? AND state = 'waiting' AND local_name = ?`, w.to, w.uid, w.from); err != nil {
			return err
		}
		if err := touchFavs(tx, w.uid, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
