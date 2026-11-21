package main

// Author profiles from Hardcover: a picture, a bio and the years they
// lived, shown at the top of an author's page when an admin hasn't given
// the entry its own (aggregate_meta.go wins, field by field).
//
// Looked up only on demand: the first time a signed-in user opens an
// author's page, the page asks for it (POST /api/authors/profile, from
// web/author-profile.ts). What Hardcover answers, found or not, is kept
// for good in hardcover_authors and never asked again, unless an editor
// asks for it again. Pictures are fetched once and served from our own
// origin (<datadir>/author-images), never hot-linked.
//
// The lookups go through the Hardcover queue (hardcover.go) as "author"
// jobs, behind every editor's series request, and are spread out: one at
// most every hcAuthorSpacing, so a queue of them (say, held overnight
// while the plan's daily limit was used up) trickles out rather than
// firing at once. They also leave the last hcAuthorReserve of the day's
// limit to the editors: past it they're held until the limit resets.
//
// Only the author's name (and, failing an exact match, a search for it)
// is sent. Everything Hardcover answers is untrusted: the bio is plain
// text shown through html/template, the picture is re-encoded as a JPEG.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/imageutil"
)

const (
	hcJobAuthor = "author" // look an author up for their page's profile

	// hcAuthorSpacing: the least time between two author lookups.
	hcAuthorSpacing = 20 * time.Second
	// hcAuthorReserve: the share of the day's limit author lookups leave
	// to editors' series requests.
	hcAuthorReserve = 0.2
	// hcAuthorRetryAfter: a lookup that failed (not one that found
	// nothing) may be asked for again after this.
	hcAuthorRetryAfter = 24 * time.Hour

	hcAuthorImagesDir      = "author-images"
	hcAuthorImageMaxWidth  = 600
	hcAuthorImageKeepBytes = 400 << 10
)

// hcAuthor is a row of hardcover_authors. HCID 0: not found on
// Hardcover; -1: the lookup failed (retried after hcAuthorRetryAfter).
type hcAuthor struct {
	Name      string
	HCID      int64
	Slug      string
	HCName    string
	Bio       string
	Image     string
	Born      int
	Died      int
	CheckedAt int64
}

func (a *hcAuthor) found() bool { return a != nil && a.HCID > 0 }

func (a *hcAuthor) imageURL() string {
	if a == nil || a.Image == "" || !aggregateImageRe.MatchString(a.Image) {
		return ""
	}
	return "/" + hcAuthorImagesDir + "/" + a.Image
}

func (a *hcAuthor) hardcoverURL() string {
	if a == nil || !hardcover.ValidSlug(a.Slug) {
		return ""
	}
	return "https://hardcover.app/authors/" + a.Slug
}

// years: "1948–2015", "born 1948", "died 1616", or "".
func (a *hcAuthor) years() string {
	switch {
	case a == nil:
		return ""
	case a.Born > 0 && a.Died > 0:
		return fmt.Sprintf("%d–%d", a.Born, a.Died)
	case a.Born > 0:
		return fmt.Sprintf("born %d", a.Born)
	case a.Died > 0:
		return fmt.Sprintf("died %d", a.Died)
	}
	return ""
}

// needsLookup: the author was never looked up, or the last lookup failed
// long enough ago.
func (a *hcAuthor) needsLookup() bool {
	return a == nil || (a.HCID < 0 && hcNow().Sub(time.Unix(a.CheckedAt, 0)) > hcAuthorRetryAfter)
}

// hcAuthorFor is the stored lookup of the author (as browsed); nil when
// there's none.
func hcAuthorFor(name string) *hcAuthor {
	if appDB == nil {
		return nil
	}
	var a hcAuthor
	err := appDB.QueryRow(`SELECT name, hc_id, hc_slug, hc_name, bio, image, born_year, death_year, checked_at
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

func hcSaveAuthor(a *hcAuthor) error {
	old := hcAuthorFor(a.Name)
	_, err := appDB.Exec(`INSERT INTO hardcover_authors (name, hc_id, hc_slug, hc_name, bio, image, born_year, death_year, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET hc_id = excluded.hc_id, hc_slug = excluded.hc_slug, hc_name = excluded.hc_name,
			bio = excluded.bio, image = excluded.image, born_year = excluded.born_year, death_year = excluded.death_year,
			checked_at = excluded.checked_at`,
		a.Name, a.HCID, a.Slug, a.HCName, a.Bio, a.Image, a.Born, a.Died, a.CheckedAt)
	if err == nil && old != nil && old.Image != a.Image {
		hcRemoveUnusedAuthorImage(old.Image)
	}
	return err
}

// hcForgetAuthor drops the stored lookup, so the next is asked of Hardcover.
func hcForgetAuthor(name string) error {
	old := hcAuthorFor(name)
	if old == nil {
		return nil
	}
	if _, err := appDB.Exec(`DELETE FROM hardcover_authors WHERE name = ?`, name); err != nil {
		return err
	}
	hcRemoveUnusedAuthorImage(old.Image)
	return nil
}

func hcRemoveUnusedAuthorImage(name string) {
	if name == "" || !aggregateImageRe.MatchString(name) {
		return
	}
	var n int
	if err := appDB.QueryRow(`SELECT COUNT(*) FROM hardcover_authors WHERE image = ?`, name).Scan(&n); err != nil || n > 0 {
		return
	}
	if err := os.Remove(filepath.Join(dataDir, hcAuthorImagesDir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("author image %s: %v", name, err)
	}
}

// hcRequestAuthor queues a lookup of the author (as browsed), unless one
// is stored or already waiting. id 0: nothing was queued.
func hcRequestAuthor(name string, by *User) (int64, error) {
	if !hcAuthorFor(name).needsLookup() {
		return 0, nil
	}
	return hcEnqueue(hcJobAuthor, name, "", hcPayload{}, by)
}

// ---------------------------------------------------------------------------
// The lookup (run by the queue)
// ---------------------------------------------------------------------------

// hcAuthorResult is an author job's result.
type hcAuthorResult struct {
	Found  bool   `json:"found"`
	HCName string `json:"hcName,omitempty"`
}

// hcAuthorHeld: author lookups wait for the day's limit to reset, leaving
// what's left of it to editors. Unlike a hardcover.WaitError it holds
// only author jobs.
type hcAuthorHeld struct{ until time.Time }

func (e *hcAuthorHeld) Error() string {
	return "most of today's Hardcover limit is used; author lookups wait until " + e.until.UTC().Format("15:04 UTC")
}

// hcAuthorBudgetLeft: whether an author lookup may spend from today's
// limit now; else when it may.
func hcAuthorBudgetLeft() (bool, time.Time) {
	u := hcClient.Budget.Usage(hcNow())
	left := u.Limit - u.Used
	if u.Remote >= 0 && u.Remote < left {
		left = u.Remote
	}
	// Three requests at most per lookup.
	if float64(left-3) < hcAuthorReserve*float64(u.Limit) {
		return false, u.ResetsAt
	}
	return true, time.Time{}
}

func hcLookupAuthor(ctx context.Context, name string) (any, error) {
	if ok, until := hcAuthorBudgetLeft(); !ok {
		return nil, &hcAuthorHeld{until}
	}
	_, members, _ := groups.resolve("author", name)
	if len(members) == 0 {
		members = []string{name}
	}
	a, err := hcFindAuthor(ctx, members)
	var wait *hardcover.WaitError
	switch {
	case errors.As(err, &wait) || ctx.Err() != nil:
		return nil, err
	case err != nil:
		// Not asked again for a while, whoever opens the page.
		_ = hcSaveAuthor(&hcAuthor{Name: name, HCID: -1, CheckedAt: hcNow().Unix()})
		return nil, err
	}
	row := &hcAuthor{Name: name, CheckedAt: hcNow().Unix()}
	if a != nil {
		row.HCID, row.Slug, row.HCName, row.Bio, row.Born, row.Died = a.ID, a.Slug, a.Name, a.Bio, a.BornYear, a.DeathYear
		if a.ImageURL != "" {
			row.Image = hcSaveAuthorImage(ctx, a.ImageURL)
		}
	}
	if err := hcSaveAuthor(row); err != nil {
		return nil, err
	}
	return hcAuthorResult{Found: a != nil, HCName: row.HCName}, nil
}

// hcSaveAuthorImage fetches and keeps an author's picture; "" when it
// can't be had (the profile is kept without it).
func hcSaveAuthorImage(ctx context.Context, u string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, _, err := hcClient.FetchImage(ctx, u)
	if err != nil {
		return ""
	}
	jpg, err := imageutil.CoverJPEG(data, imageutil.CoverOptions{
		MaxWidth:     hcAuthorImageMaxWidth,
		KeepMaxBytes: hcAuthorImageKeepBytes,
		Quality:      aggregateImageQuality,
	})
	if err != nil {
		return ""
	}
	name, err := saveHashedImage(hcAuthorImagesDir, &aggImage{jpg, "jpg"})
	if err != nil {
		log.Println("author image:", err)
		return ""
	}
	return name
}

// hcFindAuthor finds the Hardcover author who is members (an author, or
// the names an author group holds): by exact name first, then by search,
// keeping only those with the same name. Of several, the one who wrote
// the most of the library's books, then the most read. nil: none, or
// none sure.
func hcFindAuthor(ctx context.Context, members []string) (*hardcover.Author, error) {
	keys := map[string]bool{}
	var names []string
	for _, m := range members {
		keys[hcNameKey(m)] = true
		for _, v := range hcAuthorNameVariants(m) {
			if !slices.Contains(names, v) {
				names = append(names, v)
			}
		}
	}
	same := func(a hardcover.Author) bool { return keys[hcNameKey(a.Name)] }

	cands, err := hcClient.AuthorsByName(ctx, names)
	if err != nil {
		return nil, err
	}
	cands = slices.DeleteFunc(cands, func(a hardcover.Author) bool { return !same(a) })
	if len(cands) == 0 {
		hits, err := hcClient.SearchAuthors(ctx, members[0])
		if err != nil {
			return nil, err
		}
		var ids []int64
		for _, h := range hits {
			ok := keys[hcNameKey(h.Name)]
			for _, alt := range h.Alternates {
				ok = ok || keys[hcNameKey(alt)]
			}
			if ok && len(ids) < 5 {
				ids = append(ids, h.ID)
			}
		}
		if len(ids) == 0 {
			return nil, nil
		}
		if cands, err = hcClient.AuthorsByID(ctx, ids); err != nil {
			return nil, err
		}
	}
	return hcPickAuthor(cands, hcAuthorTitleKeys(members)), nil
}

// hcPickAuthor: of authors of the same name, the one whose books the
// library has most of. When the library has none of anyone's (as far as
// their most read books show), only an author who is the only one, or far
// more read than the next, is taken.
func hcPickAuthor(cands []hardcover.Author, titles map[string]bool) *hardcover.Author {
	if len(cands) == 0 {
		return nil
	}
	overlap := func(a hardcover.Author) int {
		n := 0
		for _, t := range a.Titles {
			if titles[hcTitleKey(t)] {
				n++
			}
		}
		return n
	}
	sorted := slices.Clone(cands)
	slices.SortStableFunc(sorted, func(a, b hardcover.Author) int {
		if d := overlap(b) - overlap(a); d != 0 {
			return d
		}
		return b.UsersCount - a.UsersCount
	})
	best := sorted[0]
	switch {
	case overlap(best) > 0, len(sorted) == 1:
	case best.UsersCount >= 5*max(sorted[1].UsersCount, 1):
	default:
		return nil
	}
	return &best
}

// hcAuthorTitleKeys: the library's titles by these authors (hcTitleKey).
func hcAuthorTitleKeys(members []string) map[string]bool {
	c := currentCatalog()
	want := map[string]bool{}
	for _, m := range members {
		want[strings.ToLower(m)] = true
	}
	out := map[string]bool{}
	for i := range c.books {
		b := &c.books[i]
		if b.Removed {
			continue
		}
		for _, a := range b.AuthorIDs {
			if want[strings.ToLower(c.authorNames[a])] {
				out[hcTitleKey(b.Title)] = true
				break
			}
		}
	}
	return out
}

var (
	hcInitialsRe   = regexp.MustCompile(`\b(\p{Lu})\.\s*`)
	hcInitialEndRe = regexp.MustCompile(`(\p{Lu}\.)(\p{L}{2,})`)
)

// hcAuthorNameVariants: the spellings of a name Hardcover may have it
// under: as given, "Last, First" turned round, and initials with and
// without spaces ("J. R. R. Tolkien", "J.R.R. Tolkien").
func hcAuthorNameVariants(name string) []string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return nil
	}
	bases := []string{name}
	if last, first, ok := strings.Cut(name, ", "); ok && !strings.Contains(first, ",") && first != "" {
		bases = append(bases, first+" "+last)
	}
	var out []string
	add := func(s string) {
		if s = strings.Join(strings.Fields(s), " "); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	for _, b := range bases {
		add(b)
		// Spaced: "J.R.R. Tolkien" -> "J. R. R. Tolkien".
		add(hcInitialsRe.ReplaceAllString(b, "$1. "))
		// Packed: "J. R. R. Tolkien" -> "J.R.R. Tolkien".
		packed := hcInitialsRe.ReplaceAllString(b, "$1.")
		packed = hcInitialEndRe.ReplaceAllString(packed, "$1 $2")
		add(packed)
	}
	return out
}

// ---------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------

// AuthorProfile is what the author's page shows of Hardcover's lookup.
type AuthorProfile struct {
	Years        string // "1948–2015"
	HardcoverURL string // their Hardcover page, "" for none
	// FromHardcover: the bio or the picture shown is Hardcover's.
	FromHardcover bool
	// Lookup: the page asks for a lookup (web/author-profile.ts).
	Lookup string
	// Refresh: an editor may have it looked up again.
	Refresh string
}

// hcFillAuthorProfile adds Hardcover's profile of the author to their page:
// the bio and picture where the page has none of its own.
func hcFillAuthorProfile(r *http.Request, data *FilteredPageData) {
	if data.Category != "author" || appDB == nil {
		return
	}
	a := hcAuthorFor(data.Name)
	p := &AuthorProfile{}
	if a.found() {
		if data.Description == "" && a.Bio != "" {
			data.Description, p.FromHardcover = a.Bio, true
		}
		if data.ImageURL == "" && a.imageURL() != "" {
			data.ImageURL, p.FromHardcover = a.imageURL(), true
		}
		p.Years, p.HardcoverURL = a.years(), a.hardcoverURL()
	}
	q := "?" + url.Values{"name": {data.Name}}.Encode()
	if hardcoverEnabled() {
		if currentUser(r) != nil && a.needsLookup() && data.TotalBooks > data.Missing {
			p.Lookup = "/api/authors/profile" + q
		}
		if a != nil && canEditLibrary(r) {
			p.Refresh = "/api/authors/profile/refresh" + q
		}
	}
	if p.Years != "" || p.HardcoverURL != "" || p.FromHardcover || p.Lookup != "" || p.Refresh != "" {
		data.Profile = p
	}
}

// hcFillSeriesDescription adds the description Hardcover gave a linked
// series (fetched with its books, hardcover.go) where the page has none of
// its own. Plain text, shown escaped like an author's bio.
func hcFillSeriesDescription(data *FilteredPageData) {
	if data.Category != "series" || data.Description != "" || appDB == nil {
		return
	}
	l, err := hcLinkFor(data.Name)
	if err != nil || l == nil || l.Snapshot == nil || l.Snapshot.Description == "" {
		return
	}
	data.Description = l.Snapshot.Description
	data.Profile = &AuthorProfile{FromHardcover: true, HardcoverURL: l.hardcoverURL()}
}

// authorHeaderData is what the top of the author's page shows, for
// rendering it again once Hardcover has answered.
func authorHeaderData(r *http.Request, value string) (FilteredPageData, bool) {
	c := categoryByKey("author")
	name, members, grouped := groups.resolve(c.Key, value)
	if !grouped {
		// As the library spells it: the queue and the store key on it.
		for _, n := range currentCatalog().authorNames {
			if strings.EqualFold(n, name) {
				name, members = n, []string{n}
				break
			}
		}
	}
	data := FilteredPageData{Name: name, Display: name, Category: c.Key, Members: members,
		TotalBooks: getFilteredTotal(c, name, members)}
	if data.TotalBooks == 0 {
		return data, false
	}
	if m, ok := aggregateMetaFor(c.Key, name); ok {
		data.Description, data.ImageURL = m.Description, m.imageURL()
	}
	data.Series = authorSeries(members)
	if st := seriesStatuses(); len(st) > 0 {
		for i := range data.Series {
			data.Series[i].Status = statusPtr(st, data.Series[i].Name)
		}
	}
	hcFillAuthorProfile(r, &data)
	return data, true
}

// POST /api/authors/profile?name=: look the author up on Hardcover, unless
// done already, and answer the top of their page (HTML) once it's there:
// 200 with it, or 202 while the lookup waits in the queue.
func authorProfileHandler(refresh bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, ok := authorHeaderData(r, r.URL.Query().Get("name"))
		if !ok {
			http.NotFound(w, r) // only authors with books here are looked up
			return
		}
		if refresh {
			if err := hcForgetAuthor(data.Name); err != nil {
				log.Println("hardcover author:", err)
				http.Error(w, "couldn't forget the lookup", http.StatusInternalServerError)
				return
			}
		}
		id, err := hcRequestAuthor(data.Name, currentUser(r))
		if err != nil {
			log.Println("hardcover author:", err)
			http.Error(w, "couldn't queue the lookup", http.StatusInternalServerError)
			return
		}
		if id != 0 {
			if j, err := hcAwait(r.Context(), id, hcAwaitTime); err != nil || j.State != "done" && j.State != "failed" {
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusAccepted)
				return
			}
			data, _ = authorHeaderData(r, data.Name)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		logErr(templates.ExecuteTemplate(w, "agg-header", data), "author profile")
	}
}

// GET /author-images/{file}: a picture from Hardcover, kept under its hash.
func authorImageHandler(w http.ResponseWriter, r *http.Request) {
	serveHashedImage(w, r, hcAuthorImagesDir)
}

func registerAuthorProfileRoutes() {
	http.HandleFunc("POST /api/authors/profile", requireUser(requireLookups(requireHardcover(authorProfileHandler(false)))))
	http.HandleFunc("POST /api/authors/profile/refresh", requireEditor(requireLookups(requireHardcover(authorProfileHandler(true)))))
	http.HandleFunc("GET /"+hcAuthorImagesDir+"/{file}", authorImageHandler)
}

// Shows: the profile has something to show in the header.
func (p *AuthorProfile) Shows() bool {
	return p != nil && (p.Years != "" || p.HardcoverURL != "" || p.FromHardcover || p.Refresh != "")
}

// descLeadMax: a description longer than this shows its first part, the
// rest behind "More".
const descLeadMax = 600

// splitDescription: a long description's first paragraphs (about
// descLeadMax/2 at least, cut at a sentence when one paragraph is long),
// and the rest.
func splitDescription(s string) (lead, more string) {
	if len(s) <= descLeadMax {
		return s, ""
	}
	paras := strings.Split(s, "\n\n")
	n := 0
	for n < len(paras) && (n == 0 || len(strings.Join(paras[:n], "\n\n")) < descLeadMax/2) {
		n++
	}
	lead = strings.Join(paras[:n], "\n\n")
	if len(lead) > descLeadMax*3/2 {
		if i := strings.LastIndex(lead[:descLeadMax], ". "); i > descLeadMax/3 {
			return lead[:i+1], strings.TrimSpace(s[i+1:])
		}
	}
	if n == len(paras) {
		return s, ""
	}
	return lead, strings.Join(paras[n:], "\n\n")
}

// DescLead and DescMore: the description, split for showing (filtered.html).
func (d FilteredPageData) DescLead() string { l, _ := splitDescription(d.Description); return l }
func (d FilteredPageData) DescMore() string { _, m := splitDescription(d.Description); return m }

// descNarrowMax: a lead longer than this (or with this many line breaks) may
// pass the four lines a phone shows of it (style.css), so it gets a "More"
// toggle there even when there is no more to show.
const descNarrowMax = 200

// DescClamps: the lead may be cut short on a phone.
func (d FilteredPageData) DescClamps() bool {
	l := d.DescLead()
	return len(l) > descNarrowMax || strings.Count(l, "\n") >= 3
}
