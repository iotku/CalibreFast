package hardcoverseries

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// staleAfter: a series' list is shown as stale after this.
const staleAfter = 30 * 24 * time.Hour

// Link is a series linked to Hardcover: to one series there, or to
// several parts of one (hardcover_series_parts).
type Link struct {
	Name         string
	HCID         int64 // the first part
	Slug, HCName string
	IdentifiedAt int64
	CheckedAt    int64
	Snapshot     *hardcover.SeriesDetail
	Parts        []Part
}

// Part is one Hardcover series a library series is linked to.
type Part struct {
	ID     int64
	Slug   string
	Name   string
	Author string
	Books  int
}

// URL is its page on Hardcover, or "".
func (p Part) URL() string {
	if !hardcover.ValidSlug(p.Slug) {
		return ""
	}
	return "https://hardcover.app/series/" + p.Slug
}

// ids: the Hardcover series to fetch.
func (l *Link) ids() []int64 {
	var ids []int64
	for _, p := range l.Parts {
		ids = append(ids, p.ID)
	}
	if len(ids) == 0 && l.HCID > 0 {
		ids = []int64{l.HCID}
	}
	return ids
}

// loadParts reads the link's parts.
func (s *Service) loadParts(l *Link) error {
	rows, err := s.db.Query(`SELECT hc_id, hc_slug, hc_name, author, books FROM hardcover_series_parts WHERE name = ? ORDER BY books DESC, hc_id`, l.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	l.Parts = nil
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Author, &p.Books); err != nil {
			return err
		}
		l.Parts = append(l.Parts, p)
	}
	return rows.Err()
}

// URL is its page on Hardcover, or "".
func (l *Link) URL() string {
	if !hardcover.ValidSlug(l.Slug) {
		return ""
	}
	return "https://hardcover.app/series/" + l.Slug
}

// Stale: its list was fetched too long before now to trust.
func (l *Link) Stale(now time.Time) bool {
	return l.CheckedAt == 0 || now.Sub(time.Unix(l.CheckedAt, 0)) > staleAfter
}

func scanLink(sc interface{ Scan(...any) error }) (*Link, error) {
	var l Link
	var snap string
	if err := sc.Scan(&l.Name, &l.HCID, &l.Slug, &l.HCName, &l.IdentifiedAt, &l.CheckedAt, &snap); err != nil {
		return nil, err
	}
	if snap != "" {
		var d hardcover.SeriesDetail
		if err := json.Unmarshal([]byte(snap), &d); err == nil {
			l.Snapshot = &d
		}
	}
	return &l, nil
}

const linkCols = `name, hc_id, hc_slug, hc_name, identified_at, checked_at, snapshot`

// LinkFor: the series' link, or nil.
func (s *Service) LinkFor(name string) (*Link, error) {
	if s == nil {
		return nil, nil
	}
	l, err := scanLink(s.db.QueryRow(`SELECT `+linkCols+` FROM hardcover_series WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l, s.loadParts(l)
}

func (s *Service) AllLinks() ([]*Link, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT ` + linkCols + ` FROM hardcover_series ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SaveLink links name to d's series (all its parts) with d as its list.
// What was dismissed stays dismissed: Hardcover's book ids are its own,
// whichever of its series lists them.
func (s *Service) SaveLink(name string, d *hardcover.SeriesDetail, byUserID int64) error {
	snap, err := json.Marshal(d)
	if err != nil {
		return err
	}
	var uid any
	if byUserID > 0 {
		uid = byUserID
	}
	now := s.now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parts := d.AllParts()
	var oldIDs []int64
	if rows, qerr := tx.Query(`SELECT hc_id FROM hardcover_series_parts WHERE name = ?`, name); qerr == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				oldIDs = append(oldIDs, id)
			}
		}
		rows.Close()
	}
	same := len(oldIDs) > 0 && slices.ContainsFunc(parts, func(p hardcover.Series) bool { return slices.Contains(oldIDs, p.ID) })
	var old int64
	err = tx.QueryRow(`SELECT hc_id FROM hardcover_series WHERE name = ?`, name).Scan(&old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.Exec(`INSERT INTO hardcover_series (name, hc_id, hc_slug, hc_name, identified_by, identified_at, checked_at, snapshot)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, name, d.ID, d.Slug, d.Name, uid, now, now, string(snap))
	case err != nil:
	case !same && old != d.ID:
		_, err = tx.Exec(`UPDATE hardcover_series SET hc_id = ?, hc_slug = ?, hc_name = ?, identified_by = ?, identified_at = ?, checked_at = ?, snapshot = ? WHERE name = ?`,
			d.ID, d.Slug, d.Name, uid, now, now, string(snap), name)
	default:
		_, err = tx.Exec(`UPDATE hardcover_series SET hc_id = ?, hc_slug = ?, hc_name = ?, checked_at = ?, snapshot = ? WHERE name = ?`,
			d.ID, d.Slug, d.Name, now, string(snap), name)
	}
	if err == nil {
		_, err = tx.Exec(`DELETE FROM hardcover_series_parts WHERE name = ?`, name)
	}
	for _, p := range parts {
		if err != nil {
			break
		}
		_, err = tx.Exec(`INSERT INTO hardcover_series_parts (name, hc_id, hc_slug, hc_name, author, books) VALUES (?, ?, ?, ?, ?, ?)`,
			name, p.ID, p.Slug, p.Name, p.Author, p.BooksCount)
	}
	if err != nil {
		return err
	}
	defer s.host.StatusChanged()
	return tx.Commit()
}

// Unlink forgets the series' link. Its dismissals stay, for when it's
// linked again.
func (s *Service) Unlink(name string) error {
	defer s.host.StatusChanged()
	if _, err := s.db.Exec(`DELETE FROM hardcover_series_parts WHERE name = ?`, name); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM hardcover_series WHERE name = ?`, name)
	return err
}

// Dismiss turns down the list's book ids for the series, for good (until
// an editor restores them); Restore brings them back, all when ids is
// empty.
func (s *Service) Dismiss(name string, ids []int64) (int, error) {
	defer s.host.StatusChanged()
	n := 0
	for _, id := range ids {
		res, err := s.db.Exec(`INSERT OR IGNORE INTO hardcover_dismissed (name, hc_book_id) VALUES (?, ?)`, name, id)
		if err != nil {
			return n, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			n++
		}
	}
	return n, nil
}

func (s *Service) Restore(name string, ids []int64) error {
	defer s.host.StatusChanged()
	if len(ids) == 0 {
		if _, err := s.db.Exec(`DELETE FROM hardcover_not_match WHERE name = ?`, name); err != nil {
			return err
		}
		_, err := s.db.Exec(`DELETE FROM hardcover_dismissed WHERE name = ?`, name)
		return err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM hardcover_dismissed WHERE name = ? AND hc_book_id = ?`, name, id); err != nil {
			return err
		}
	}
	return nil
}

// RestoreMatch undoes a "Not a match": the library book may be matched
// to the Hardcover book (ids: it and the books merged with it) again.
func (s *Service) RestoreMatch(name, uuid string, ids []int64) error {
	defer s.host.StatusChanged()
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM hardcover_not_match WHERE name = ? AND hc_book_id = ? AND uuid = ?`, name, id, uuid); err != nil {
			return err
		}
	}
	return nil
}

// LinkedReviews: every linked series' review (without placeholders: only
// its counts are meant), by lower-cased name.
func (s *Service) LinkedReviews() map[string]Review {
	if s == nil {
		return nil
	}
	links, err := s.AllLinks()
	if err != nil {
		log.Println("hardcover status:", err)
		return nil
	}
	m := make(map[string]Review, len(links))
	for _, l := range links {
		si, ok := s.host.Series(l.Name)
		if !ok || l.Snapshot == nil {
			continue
		}
		lib, err := s.library(si, l.Snapshot)
		if err != nil {
			log.Println("hardcover status:", err)
			continue
		}
		rev := Match(l.Snapshot, lib, nil, s.dismissed(si.Name))
		m[strings.ToLower(si.Name)] = rev
	}
	return m
}

func (s *Service) dismissed(name string) map[int64]bool {
	out := map[int64]bool{}
	rows, err := s.db.Query(`SELECT hc_book_id FROM hardcover_dismissed WHERE name = ?`, name)
	if err != nil {
		log.Println("hardcover:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// refused: the series' "Not a match"es, Hardcover book id -> uuids.
func (s *Service) refused(name string) map[int64]map[string]bool {
	out := map[int64]map[string]bool{}
	if s.db == nil {
		return out
	}
	rows, err := s.db.Query(`SELECT hc_book_id, uuid FROM hardcover_not_match WHERE name = ?`, name)
	if err != nil {
		log.Println("hardcover:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var uuid string
		if rows.Scan(&id, &uuid) == nil {
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			out[id][uuid] = true
		}
	}
	return out
}

// library is the host's library for matching d against si, with the
// series' "Not a match"es.
func (s *Service) library(si SeriesInfo, d *hardcover.SeriesDetail) (Library, error) {
	lib, err := s.host.Library(si, d)
	lib.Refused = s.refused(si.Name)
	return lib, err
}

// ReviewFor matches the link's list against the library now.
func (s *Service) ReviewFor(si SeriesInfo, l *Link) (Review, error) {
	if l == nil || l.Snapshot == nil {
		return Review{}, nil
	}
	lib, err := s.library(si, l.Snapshot)
	if err != nil {
		return Review{}, err
	}
	return Match(l.Snapshot, lib, s.host.Placeholders(), s.dismissed(si.Name)), nil
}

// NotMatch records an editor's "Not a match": library book uuid isn't
// Hardcover book hcBookID, in the series name.
func (s *Service) NotMatch(name string, hcBookID int64, uuid string) error {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO hardcover_not_match (name, hc_book_id, uuid) VALUES (?, ?, ?)`, name, hcBookID, uuid); err != nil {
		return err
	}
	s.host.StatusChanged()
	return nil
}
