package hardcoversync

// Importing the authors a user follows on Hardcover as starred authors
// (aggregate_favorites.go), from the account page. Read-only on Hardcover;
// see docs/notes/Hardcover-favorites.md for the API.
//
// Each followed author becomes one of:
//
//   - starred: the library author it is, starred. Matched by the Hardcover
//     id an author lookup stored (hardcover_authors.hc_id), else by name as
//     textkey.Name has it (case, accents, periods, word order and "Last,
//     First" don't count), through groups, so it stars the author as
//     browsed. A choice the user made (below) comes first.
//   - waiting: no such author here: starred under Hardcover's name all the
//     same. Such a star isn't listed while the library has no books by that
//     name; when one arrives (or an author whose name matches the same way:
//     WakeWaitingStars), the star becomes the library author's and the
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
	"github.com/iotku/CalibreFast/pkg/textkey"
)

// ErrFollowScope: the link predates follows: it has to be made again.
var ErrFollowScope = errors.New("link Hardcover again to allow reading the authors you follow")

// followsImportTime bounds an import (each thousand follows is a request).
const followsImportTime = time.Minute

// authorIndex: the library's authors as browsed, by textkey.Name of their
// name and their group members' names, and by surname and first initial.
type authorIndex struct {
	exact map[string][]string
	loose map[string][]string
}

func (s *Service) libraryAuthorIndex() authorIndex {
	ix := authorIndex{exact: map[string][]string{}, loose: map[string][]string{}}
	add := func(m map[string][]string, k, name string) {
		if k != "" && !slices.Contains(m[k], name) {
			m[k] = append(m[k], name)
		}
	}
	for _, it := range s.host.LibraryAuthors() {
		for _, n := range append([]string{it.Name}, it.Members...) {
			add(ix.exact, textkey.Name(n), it.Name)
			add(ix.loose, textkey.LooseName(n), it.Name)
		}
	}
	return ix
}

// followOutcome: what a followed author becomes (see the top).
type followOutcome struct {
	state      string // starred, waiting, confirm (skipped: after starFollow)
	local      string // the name to star
	candidates []string
}

// classifyFollow decides what f becomes for userID: the user's own
// choice, else the author's stored Hardcover id, else its name.
func (s *Service) classifyFollow(tx *sql.Tx, userID int64, f hardcover.FollowedAuthor, ix authorIndex) (followOutcome, error) {
	rows, err := tx.Query(`SELECT COALESCE(local_name, ''), decision FROM hardcover_aggregate_matches
		WHERE user_id = ? AND kind = 'author' AND hc_id = ?`, userID, f.AuthorID)
	if err != nil {
		return followOutcome{}, err
	}
	rejected := map[string]bool{}
	none := false
	for rows.Next() {
		var local, decision string
		if err := rows.Scan(&local, &decision); err != nil {
			rows.Close()
			return followOutcome{}, err
		}
		switch {
		case decision == "match":
			rows.Close()
			return followOutcome{state: "starred", local: local}, nil
		case local == "":
			none = true // "none of these": not in the library
		default:
			rejected[strings.ToLower(local)] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return followOutcome{}, err
	}
	if none {
		return followOutcome{state: "waiting", local: f.Name}, nil
	}
	ok := func(names []string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return rejected[strings.ToLower(n)] })
	}
	// A lookup already tied a library author to this Hardcover author.
	var looked string
	if err := tx.QueryRow(`SELECT name FROM hardcover_authors WHERE hc_id = ? LIMIT 1`, f.AuthorID).Scan(&looked); err == nil {
		if name, found := s.host.AuthorAsBrowsed(looked); found && !rejected[strings.ToLower(name)] {
			return followOutcome{state: "starred", local: name}, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return followOutcome{}, err
	}
	switch exact := ok(ix.exact[textkey.Name(f.Name)]); len(exact) {
	case 1:
		return followOutcome{state: "starred", local: exact[0]}, nil
	case 0:
	default:
		return followOutcome{state: "confirm", candidates: exact}, nil
	}
	if near := ok(ix.loose[textkey.LooseName(f.Name)]); len(near) > 0 {
		return followOutcome{state: "confirm", candidates: near}, nil
	}
	return followOutcome{state: "waiting", local: f.Name}, nil
}

// starFollow stars name for userID as imported from Hardcover author
// hcID. added: a new star; skipped: the user took this star off before.
func (s *Service) starFollow(tx *sql.Tx, userID, hcID int64, name string) (added, skipped bool, err error) {
	g, _ := s.host.ResolveAuthor(name)
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
		if hg, _ := s.host.ResolveAuthor(have); strings.EqualFold(hg, g) {
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
	if total >= s.host.MaxFavorites() {
		return false, true, nil
	}
	_, err = tx.Exec(`INSERT INTO aggregate_favorites (user_id, kind, name, origin, hc_id, created_at) VALUES (?, 'author', ?, 'hardcover', ?, ?)`,
		userID, g, hcID, time.Now().UnixMilli())
	return err == nil, false, err
}

// applyFollow classifies f, stars it if that's what it becomes, and
// keeps the outcome in hardcover_follows. added: a star was added.
func (s *Service) applyFollow(tx *sql.Tx, userID int64, f hardcover.FollowedAuthor, ix authorIndex) (state string, added bool, err error) {
	out, err := s.classifyFollow(tx, userID, f, ix)
	if err != nil {
		return "", false, err
	}
	if out.state == "starred" || out.state == "waiting" {
		var skipped bool
		if added, skipped, err = s.starFollow(tx, userID, f.AuthorID, out.local); err != nil {
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

// FollowsResult: an import's outcome, by what the follows became.
type FollowsResult struct {
	Total, Starred, Waiting, Confirm, Skipped, Added int
}

// canReadFollows: userID's link may read their follows.
func (s *Service) canReadFollows(userID int64) bool {
	var scope string
	_ = s.db.QueryRow(`SELECT scope FROM hardcover_links WHERE user_id = ? AND broken = ''`, userID).Scan(&scope)
	return hardcover.HasScope(scope, "read:social")
}

// ImportFollows reads the authors userID follows on Hardcover and stars
// them here (see the top).
func (s *Service) ImportFollows(ctx context.Context, userID int64) (FollowsResult, error) {
	var res FollowsResult
	acc, err := s.loadAccount(userID)
	if err != nil {
		return res, err
	}
	if acc == nil {
		return res, errors.New("Hardcover isn't linked")
	}
	if !s.canReadFollows(userID) {
		return res, ErrFollowScope
	}
	ctx, cancel := context.WithTimeout(ctx, followsImportTime)
	defer cancel()
	follows, err := s.UserClient(userID).FollowedAuthors(ctx, acc.HCUserID)
	if err != nil {
		return res, err
	}
	ix := s.libraryAuthorIndex()
	tx, err := s.db.Begin()
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
		state, added, err := s.applyFollow(tx, userID, f, ix)
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
	if err := s.host.TouchFavorites(tx, userID, time.Now().UnixMilli()); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// FollowDecision records the user's choice for followed author hcID
// (from the last import): local, a library author offered for it; or ""
// for none of those offered. Undo (undo true) forgets the choices made for
// it, and takes off the star it made. Then the follow is decided again.
func (s *Service) FollowDecision(userID, hcID int64, local string, undo bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var f hardcover.FollowedAuthor
	var cands string
	err = tx.QueryRow(`SELECT hc_name, follow_id, candidates FROM hardcover_follows WHERE user_id = ? AND hc_id = ?`, userID, hcID).
		Scan(&f.Name, &f.FollowID, &cands)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoFollow
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
			return ErrNoFollow // only what was offered
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
	if _, _, err := s.applyFollow(tx, userID, f, s.libraryAuthorIndex()); err != nil {
		return err
	}
	if err := s.host.TouchFavorites(tx, userID, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrNoFollow: no such followed author to decide on (or not that choice).
var ErrNoFollow = errors.New("no such followed author")

// FollowRow is a followed author on the account page.
type FollowRow struct {
	HCID       int64
	Name       string // Hardcover's
	Local      string // its star's name
	Candidates []string
	Decided    bool // a choice the user made (undoable)
}

// FollowsView: the last import, and sending stars there
// (follow_push.go), for the account page.
type FollowsView struct {
	Push         FollowPrefs
	ToSend       int // changes waiting to be sent
	FollowedHere int // stars followed on Hardcover from here
	Problems     []FollowProblem
	CanRead      bool
	Total        int
	Starred      []FollowRow
	Waiting      []FollowRow
	Confirm      []FollowRow
	Skipped      int
	LastSeen     string
}

func (s *Service) Follows(userID int64) *FollowsView {
	v := &FollowsView{CanRead: s.canReadFollows(userID), Push: s.LoadFollowPrefs(userID)}
	v.ToSend, v.Problems, v.FollowedHere = s.followQueueView(userID)
	rows, err := s.db.Query(`SELECT f.hc_id, f.hc_name, f.state, f.local_name, f.candidates, f.seen_at,
			EXISTS (SELECT 1 FROM hardcover_aggregate_matches m WHERE m.user_id = f.user_id AND m.kind = 'author' AND m.hc_id = f.hc_id)
		FROM hardcover_follows f WHERE f.user_id = ? ORDER BY f.hc_name COLLATE NOCASE`, userID)
	if err != nil {
		return v
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		var r FollowRow
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

// WakeWaitingStars: authors that just got books here (gained), as
// browsed. A star imported from Hardcover under a name the library didn't
// have, matching one of them as an import would (textkey.Name), becomes that
// author's, before anyone is told about the books (notifyFavoriteGains).
func (s *Service) WakeWaitingStars(gained []string) error {
	if s == nil {
		return nil
	}
	byKey := map[string]string{}
	for _, a := range gained {
		byKey[textkey.Name(a)] = a
	}
	rows, err := s.db.Query(`SELECT user_id, name FROM aggregate_favorites WHERE kind = 'author' AND origin = 'hardcover' AND removed_at IS NULL`)
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
		if to, ok := byKey[textkey.Name(w.from)]; ok && !strings.EqualFold(to, w.from) {
			w.to = to
			wakes = append(wakes, w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(wakes) == 0 {
		return err
	}
	tx, err := s.db.Begin()
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
		if err := s.host.TouchFavorites(tx, w.uid, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
