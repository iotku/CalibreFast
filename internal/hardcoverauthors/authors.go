package hardcoverauthors

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// Author is what Hardcover said of an author (as browsed). HCID 0: not
// found there; -1: the lookup failed (retried after RetryAfter).
type Author struct {
	Name      string
	HCID      int64
	Slug      string
	HCName    string
	Bio       string
	Image     string // as StoreImage named it, "" for none
	Born      int
	Died      int
	CheckedAt int64
}

// Found: Hardcover has the author.
func (a *Author) Found() bool { return a != nil && a.HCID > 0 }

// HardcoverURL is their page on Hardcover, or "".
func (a *Author) HardcoverURL() string {
	if a == nil || !hardcover.ValidSlug(a.Slug) {
		return ""
	}
	return "https://hardcover.app/authors/" + a.Slug
}

// Years: "1948–2015", "born 1948", "died 1616", or "".
func (a *Author) Years() string {
	if a == nil {
		return ""
	}
	return LifeYears(a.Born, a.Died)
}

// NeedsLookup: the author was never looked up, or the last lookup failed
// long enough before now.
func (a *Author) NeedsLookup(now time.Time) bool {
	return a == nil || (a.HCID < 0 && now.Sub(time.Unix(a.CheckedAt, 0)) > RetryAfter)
}

// LifeYears: "1948–2015", "born 1948", "died 1616", or "" (0: not known).
func LifeYears(born, died int) string {
	switch {
	case born > 0 && died > 0:
		return fmt.Sprintf("%d–%d", born, died)
	case born > 0:
		return fmt.Sprintf("born %d", born)
	case died > 0:
		return fmt.Sprintf("died %d", died)
	}
	return ""
}

// LifeAge: "Age 77", or "Died at 41"; "" without a year of birth. From the
// years alone, so it may be one more than the true age.
func LifeAge(born, died int, now time.Time) string {
	switch {
	case born <= 0:
		return ""
	case died > 0:
		return fmt.Sprintf("Died at %d", died-born)
	}
	return fmt.Sprintf("Age %d", now.Year()-born)
}

// For is the stored lookup of the author (as browsed); nil when there's
// none.
func (s *Service) For(name string) *Author {
	if s == nil || s.db == nil {
		return nil
	}
	var a Author
	err := s.db.QueryRow(`SELECT name, hc_id, hc_slug, hc_name, bio, image, born_year, death_year, checked_at
		FROM hardcover_authors WHERE name = ?`, name).
		Scan(&a.Name, &a.HCID, &a.Slug, &a.HCName, &a.Bio, &a.Image, &a.Born, &a.Died, &a.CheckedAt)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("hardcover author %q: %v", name, err)
		}
		return nil
	}
	return &a
}

// Save stores a lookup, replacing the author's last; a picture no author
// has any more is removed.
func (s *Service) Save(a *Author) error {
	old := s.For(a.Name)
	_, err := s.db.Exec(`INSERT INTO hardcover_authors (name, hc_id, hc_slug, hc_name, bio, image, born_year, death_year, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET hc_id = excluded.hc_id, hc_slug = excluded.hc_slug, hc_name = excluded.hc_name,
			bio = excluded.bio, image = excluded.image, born_year = excluded.born_year, death_year = excluded.death_year,
			checked_at = excluded.checked_at`,
		a.Name, a.HCID, a.Slug, a.HCName, a.Bio, a.Image, a.Born, a.Died, a.CheckedAt)
	if err == nil && old != nil && old.Image != a.Image {
		s.removeUnusedImage(old.Image)
	}
	return err
}

// Forget drops the stored lookup, so the next is asked of Hardcover.
func (s *Service) Forget(name string) error {
	if s == nil {
		return nil
	}
	old := s.For(name)
	if old == nil {
		return nil
	}
	if _, err := s.db.Exec(`DELETE FROM hardcover_authors WHERE name = ?`, name); err != nil {
		return err
	}
	s.removeUnusedImage(old.Image)
	return nil
}

func (s *Service) removeUnusedImage(name string) {
	if name == "" || s.cfg.RemoveImage == nil {
		return
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hardcover_authors WHERE image = ?`, name).Scan(&n); err != nil || n > 0 {
		return
	}
	s.cfg.RemoveImage(name)
}
