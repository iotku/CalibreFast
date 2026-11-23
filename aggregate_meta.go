package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/imageutil"
	"github.com/iotku/CalibreFast/pkg/langcodes"
)

// What the aggregate pages keep beyond Calibre's values and the groups
// (groups.go): entries featured at the top of a category's page, and an
// entry's description and picture. An entry is what a filtered page shows:
// a group, by its name, or a single value. All of it lives in the app DB
// and <datadir>/aggregate-images, never in the library folder.

const aggregateImagesDir = "aggregate-images"

const (
	// aggregateImageMaxWidth: a wider picture is scaled down to this.
	aggregateImageMaxWidth = 1200
	// aggregateImageKeepMaxBytes: a JPEG up to this size (and width) is
	// kept byte for byte.
	aggregateImageKeepMaxBytes = 1 << 20
	aggregateImageQuality      = 88
	// aggregateDescriptionMax bounds a description, in bytes.
	aggregateDescriptionMax = 10000
)

// aggregateImageRe: the names aggregate pictures are saved under (the
// content's hash), the only ones served.
var aggregateImageRe = regexp.MustCompile(`^[0-9a-f]{32}\.(jpg|svg)$`)

// aggImage is a picture ready to save: a JPEG, or a cleaned SVG.
type aggImage struct {
	data []byte
	ext  string // "jpg" or "svg"
}

// AggregateMeta is an entry's description and picture, and an author's
// years when an editor set them (YearsSet; 0: not known).
type AggregateMeta struct {
	Description string
	Image       string // file name in aggregateImagesDir, "" for none
	YearsSet    bool
	Born, Died  int
}

func (m AggregateMeta) empty() bool { return m.Description == "" && m.Image == "" && !m.YearsSet }

func (m AggregateMeta) imageURL() string {
	if m.Image == "" {
		return ""
	}
	return "/" + aggregateImagesDir + "/" + m.Image
}

// aggregateMetaFor returns the meta of the entry cat/name (ok false: none).
func aggregateMetaFor(cat, name string) (AggregateMeta, bool) {
	var m AggregateMeta
	if appDB == nil {
		return m, false
	}
	err := appDB.QueryRow(`SELECT description, image, years_set, born_year, death_year FROM aggregate_meta WHERE category = ? AND name = ?`, cat, name).
		Scan(&m.Description, &m.Image, &m.YearsSet, &m.Born, &m.Died)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("aggregate meta %s/%s: %v", cat, name, err)
		}
		return m, false
	}
	return m, !m.empty()
}

// aggregateImage is the picture to save for an uploaded image's bytes: an
// SVG cleaned (sanitizeSVG), anything else as a JPEG.
func aggregateImage(data []byte) (*aggImage, error) {
	if looksLikeSVG(data) {
		svg, err := sanitizeSVG(data)
		if err != nil {
			return nil, err
		}
		return &aggImage{svg, "svg"}, nil
	}
	jpg, err := imageutil.CoverJPEG(data, imageutil.CoverOptions{
		MaxWidth:     aggregateImageMaxWidth,
		KeepMaxBytes: aggregateImageKeepMaxBytes,
		Quality:      aggregateImageQuality,
	})
	if err != nil {
		return nil, err
	}
	return &aggImage{jpg, "jpg"}, nil
}

// saveAggregateImage writes a picture (from aggregateImage) under its
// content's hash and returns the file name.
func saveAggregateImage(img *aggImage) (string, error) {
	return saveHashedImage(aggregateImagesDir, img)
}

// saveHashedImage writes a picture into <datadir>/<dirName> under its
// content's hash and returns the file name (aggregate pictures, placeholder
// covers).
func saveHashedImage(dirName string, img *aggImage) (string, error) {
	data := img.data
	dir := filepath.Join(dataDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:16]) + "." + img.ext
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return name, nil // the same picture is already there
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return name, os.Rename(tmp.Name(), path)
}

// removeUnusedAggregateImage deletes a picture no entry uses any more.
func removeUnusedAggregateImage(name string) {
	if name == "" || !aggregateImageRe.MatchString(name) {
		return
	}
	var n int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM aggregate_meta WHERE image = ?`, name).Scan(&n); err != nil || n > 0 {
		return
	}
	if err := os.Remove(filepath.Join(dataDir, aggregateImagesDir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("aggregate image %s: %v", name, err)
	}
}

// metaEdit is a change to an entry's meta: nil Description leaves it as
// it is, Image replaces the picture, RemoveImage drops it. Years (an
// author's), when not nil, are what the page shows from now on; nil
// Years.Set goes back to Hardcover's.
type metaEdit struct {
	Description *string
	Image       *aggImage
	RemoveImage bool
	Years       *metaYears
}

type metaYears struct {
	Set        bool
	Born, Died int
}

func (e metaEdit) empty() bool {
	return e.Description == nil && e.Image == nil && !e.RemoveImage && e.Years == nil
}

// checkYears: a year of birth and of death that may be shown (0: not known).
func checkYears(born, died int) error {
	this := time.Now().Year()
	switch {
	case born < 0 || born > this:
		return fmt.Errorf("the year of birth must be between 1 and %d", this)
	case died < 0 || died > this:
		return fmt.Errorf("the year of death must be between 1 and %d", this)
	case born > 0 && died > 0 && died < born:
		return errors.New("the year of death is before the year of birth")
	case born > 0 && died > 0 && died-born > 130:
		return errors.New("that's more than 130 years")
	}
	return nil
}

// setAggregateMeta applies e to the entry cat/name. A row left with no
// description and no picture is deleted.
func setAggregateMeta(cat, name string, e metaEdit) error {
	if e.empty() {
		return nil
	}
	old, _ := aggregateMetaFor(cat, name)
	m := old
	if e.Description != nil {
		m.Description = strings.TrimSpace(*e.Description)
		if len(m.Description) > aggregateDescriptionMax {
			return fmt.Errorf("the description is too long (at most %d characters)", aggregateDescriptionMax)
		}
	}
	if y := e.Years; y != nil {
		if err := checkYears(y.Born, y.Died); err != nil {
			return err
		}
		m.YearsSet, m.Born, m.Died = y.Set, y.Born, y.Died
		if !y.Set {
			m.Born, m.Died = 0, 0
		}
	}
	if e.RemoveImage {
		m.Image = ""
	}
	if e.Image != nil {
		img, err := saveAggregateImage(e.Image)
		if err != nil {
			return err
		}
		m.Image = img
	}
	var err error
	if m.empty() {
		_, err = appDB.Exec(`DELETE FROM aggregate_meta WHERE category = ? AND name = ?`, cat, name)
	} else {
		_, err = appDB.Exec(`
			INSERT INTO aggregate_meta (category, name, description, image, years_set, born_year, death_year, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (category, name) DO UPDATE SET
				name = excluded.name, description = excluded.description, image = excluded.image,
				years_set = excluded.years_set, born_year = excluded.born_year, death_year = excluded.death_year,
				updated_at = excluded.updated_at`,
			cat, name, m.Description, m.Image, m.YearsSet, m.Born, m.Died, nowUnix())
	}
	if err != nil {
		return err
	}
	if old.Image != m.Image {
		removeUnusedAggregateImage(old.Image)
	}
	return nil
}

// renameAggregate carries an entry's meta and Featured place over to its
// new name (a group renamed, made or dissolved). Where the new name has
// its own already, that is kept.
func renameAggregate(cat, from, to string) {
	if appDB == nil || from == to || from == "" || to == "" {
		return
	}
	for _, q := range []string{
		`UPDATE OR IGNORE aggregate_meta SET name = ? WHERE category = ? AND name = ?`,
		`UPDATE OR IGNORE featured_aggregates SET name = ? WHERE category = ? AND name = ?`,
	} {
		if _, err := appDB.Exec(q, to, cat, from); err != nil {
			log.Printf("rename aggregate %s/%s: %v", cat, from, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Featured
// ---------------------------------------------------------------------------

// FeaturedCard is one entry of an aggregate page's Featured section.
type FeaturedCard struct {
	Name        string // label shown
	Sub         string // raw value when Name is a display name
	Href        string
	Count       int
	Grouped     bool
	Description string
	ImageURL    string
}

func isFeaturedAggregate(cat, name string) bool {
	if appDB == nil {
		return false
	}
	var n int
	err := appDB.QueryRow(`SELECT COUNT(*) FROM featured_aggregates WHERE category = ? AND name = ?`, cat, name).Scan(&n)
	return err == nil && n > 0
}

// featuredCards lists c's featured entries in the order they were
// featured, as they are browsed now (a value since grouped shows as its
// group), leaving out any with no books.
func featuredCards(c *category) []FeaturedCard {
	if appDB == nil {
		return nil
	}
	rows, err := appDB.Query(`SELECT name FROM featured_aggregates WHERE category = ? ORDER BY position, created_at`, c.Key)
	if err != nil {
		log.Printf("featured %s: %v", c.Key, err)
		return nil
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()

	seen := map[string]bool{}
	var out []FeaturedCard
	for _, v := range names {
		name, members, grouped := groups.resolve(c.Key, v)
		if seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		count := getFilteredTotal(c, name, members)
		if count == 0 {
			continue
		}
		card := FeaturedCard{Name: name, Href: c.href(name), Count: count, Grouped: grouped}
		if c.Key == "language" && !grouped {
			if dn, ok := langcodes.DisplayName(name); ok && dn != name {
				card.Name, card.Sub = dn, name
			}
		}
		if m, ok := aggregateMetaFor(c.Key, name); ok {
			card.Description, card.ImageURL = m.Description, m.imageURL()
		}
		out = append(out, card)
	}
	return out
}

// featuredHandler: PUT /api/aggregates/{cat}/featured/{value...}
// features the entry value is browsed as; DELETE takes it off.
func featuredHandler(w http.ResponseWriter, r *http.Request) {
	c := categoryByKey(r.PathValue("cat"))
	value := r.PathValue("value")
	if c == nil || value == "" {
		http.NotFound(w, r)
		return
	}
	name, _, _ := groups.resolve(c.Key, value)
	var err error
	if r.Method == http.MethodDelete {
		_, err = appDB.Exec(`DELETE FROM featured_aggregates WHERE category = ? AND (name = ? OR name = ?)`, c.Key, name, value)
	} else {
		_, err = appDB.Exec(`
			INSERT OR IGNORE INTO featured_aggregates (category, name, position, created_at)
			SELECT ?, ?, COALESCE(MAX(position), 0) + 1, ? FROM featured_aggregates WHERE category = ?`,
			c.Key, name, nowUnix(), c.Key)
	}
	if err != nil {
		log.Println("featured:", err)
		http.Error(w, "failed to save", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(map[string]any{"featured": r.Method != http.MethodDelete, "name": name}), "encode featured")
}

// aggregateImageHandler: GET /aggregate-images/{file}. A file's name is its
// content's hash, so it never changes. An SVG (cleaned when saved) is also
// sent with a CSP that runs no script and loads nothing, should it be
// opened as a page.
func aggregateImageHandler(w http.ResponseWriter, r *http.Request) {
	serveHashedImage(w, r, aggregateImagesDir)
}

// serveHashedImage serves the {file} of <datadir>/<dirName> (saveHashedImage).
func serveHashedImage(w http.ResponseWriter, r *http.Request, dirName string) {
	name := r.PathValue("file")
	if !aggregateImageRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.HasSuffix(name, ".svg") {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
	}
	http.ServeFile(w, r, filepath.Join(dataDir, dirName, name))
}

// groupAlias is the name a value is shown under when it has an alias: a
// group whose one member it is ("COMPUTERS" shown as "Computers").
// Otherwise the value itself. For templates ({{ alias "tag" . }}).
func groupAlias(cat, v string) string {
	if name, members, grouped := groups.resolve(cat, v); grouped && len(members) == 1 {
		return name
	}
	return v
}
