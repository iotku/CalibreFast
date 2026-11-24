package main

// Series identified on Hardcover: the server's Hardcover client and the
// library as internal/hardcoverseries sees it (hcSeriesHost). The links,
// the review's matching and the queue of requests to Hardcover are in that
// package; the pages and API in hardcover_http.go.

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/iotku/CalibreFast/internal/hardcoverseries"
	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

var (
	// noHardcover (-no-hardcover) turns off Hardcover only.
	noHardcover bool
	// hcClient asks Hardcover; nil without CF_HARDCOVER_TOKEN.
	hcClient *hardcover.Client
	// hcNow is the clock (tests).
	hcNow = time.Now
)

// hcBatchMax: the most series one batch may hold.
const hcBatchMax = 25

// hcMaxParts: the most Hardcover series one series may be linked to.
const hcMaxParts = 60

func configureHardcover() {
	token := os.Getenv("CF_HARDCOVER_TOKEN")
	hcClient = nil
	hcLinks = newHCLinks()
	switch {
	case noLookups:
		return
	case noHardcover:
		log.Println("-no-hardcover: series won't be identified on Hardcover, nor books looked up there")
		return
	case strings.TrimSpace(token) == "":
		return
	}
	hcClient = hardcover.New(hardcover.Config{
		Token:     token,
		UserAgent: lookupUserAgent,
		Budget:    hardcover.FreePlan(),
	})
}

// hardcoverEnabled: series can be identified on Hardcover, and books looked
// up there (pkg/metalookup/hardcover.go).
func hardcoverEnabled() bool { return !noLookups && !noHardcover && hcClient != nil }

func requireHardcover(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hardcoverEnabled() {
			http.Error(w, "Hardcover isn't set up on this server (CF_HARDCOVER_TOKEN).", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// ---------------------------------------------------------------------------
// The library, as matching sees it
// ---------------------------------------------------------------------------

// hcSeries: a series as browsed: its name (its group's, if any), its
// members, and the member to write onto books and placeholders (the one
// with the most books). ok is false when no library book is in it.
func hcSeries(name string) (hardcoverseries.SeriesInfo, bool) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return hardcoverseries.SeriesInfo{}, false
	}
	g, members, _ := groups.resolve("series", name)
	c := currentCatalog()
	count := map[string]int{}
	for i := range c.books {
		b := &c.books[i]
		if b.Removed || b.Series == 0 {
			continue
		}
		s := c.seriesNames[b.Series]
		if hardcoverseries.InMembers(s, members) {
			count[s]++
		}
	}
	if len(count) == 0 {
		return hardcoverseries.SeriesInfo{}, false
	}
	info := hardcoverseries.SeriesInfo{Name: g, Members: members}
	best := -1
	for s, n := range count {
		if n > best || (n == best && s < info.Write) {
			info.Write, best = s, n
		}
	}
	return info, true
}

// hcTitleIndex: the catalog's books by title key (with and without the
// subtitle), and by book id; built
// again when the catalog changes.
type hcTitleIndex struct {
	cat     *catalog
	byTitle map[string][]int32
	byID    map[int64]int32
	byUUID  map[string]int32
}

var hcTitleIndexCache struct {
	sync.Mutex
	idx *hcTitleIndex
}

func currentHCTitleIndex() *hcTitleIndex {
	cat := currentCatalog()
	hcTitleIndexCache.Lock()
	defer hcTitleIndexCache.Unlock()
	if idx := hcTitleIndexCache.idx; idx != nil && idx.cat == cat {
		return idx
	}
	idx := &hcTitleIndex{cat: cat, byTitle: map[string][]int32{}, byID: map[int64]int32{}, byUUID: map[string]int32{}}
	for i := range cat.books {
		b := &cat.books[i]
		if b.Removed || b.UUID == "" {
			continue
		}
		full, short := textkey.FullTitle(b.Title), textkey.Title(b.Title)
		idx.byTitle[full] = append(idx.byTitle[full], int32(i))
		if short != full {
			idx.byTitle[short] = append(idx.byTitle[short], int32(i))
		}
		idx.byID[b.ID] = int32(i)
		idx.byUUID[b.UUID] = int32(i)
	}
	hcTitleIndexCache.idx = idx
	return idx
}

func (x *hcTitleIndex) libBook(slot int32) hardcoverseries.LibBook {
	b := &x.cat.books[slot]
	lb := hardcoverseries.LibBook{UUID: b.UUID, Title: b.Title}
	for _, id := range b.AuthorIDs {
		lb.Authors = append(lb.Authors, x.cat.authorNames[id])
	}
	if b.Series != 0 {
		lb.Series, lb.Index = x.cat.seriesNames[b.Series], b.SeriesIndex
	}
	return lb
}

// hcLibraryFor: the library for matching d against series s (the
// Service adds its "Not a match"es).
func hcLibraryFor(s hardcoverseries.SeriesInfo, d *hardcover.SeriesDetail) (hardcoverseries.Library, error) {
	x := currentHCTitleIndex()
	lib := hardcoverseries.Library{Members: s.Members, ByISBN: map[string][]hardcoverseries.LibBook{}}
	lib.Book = func(uuid string) (hardcoverseries.LibBook, bool) {
		if slot, ok := x.byUUID[uuid]; ok {
			return x.libBook(slot), true
		}
		return hardcoverseries.LibBook{}, false
	}
	for i := range x.cat.books {
		b := &x.cat.books[i]
		if !b.Removed && b.Series != 0 && hardcoverseries.InMembers(x.cat.seriesNames[b.Series], s.Members) {
			lib.InSeries = append(lib.InSeries, x.libBook(int32(i)))
		}
	}
	lib.ByTitle = func(key string) []hardcoverseries.LibBook {
		var out []hardcoverseries.LibBook
		for _, slot := range x.byTitle[key] {
			out = append(out, x.libBook(slot))
		}
		return out
	}
	if d == nil || calibreDB == nil {
		return lib, nil
	}
	if err := hcLibraryIdentifiers(&lib, x, d); err != nil {
		return lib, err
	}
	var all []string
	for _, e := range d.Entries {
		all = append(all, e.ISBNs...)
	}
	forms := isbnForms(all)
	for chunk := range slices.Chunk(forms, 400) {
		args := make([]any, len(chunk))
		for i, f := range chunk {
			args[i] = f
		}
		rows, err := calibreDB.Query(`SELECT book, val FROM identifiers WHERE type = 'isbn' AND val IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return lib, err
		}
		for rows.Next() {
			var id int64
			var val string
			if err := rows.Scan(&id, &val); err != nil {
				rows.Close()
				return lib, err
			}
			slot, ok := x.byID[id]
			n := isbn.Normalize(val)
			if !ok || n == "" {
				continue
			}
			lib.ByISBN[n] = append(lib.ByISBN[n], x.libBook(slot))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return lib, err
		}
	}
	return lib, nil
}

// hcLibraryIdentifiers fills lib's ByHCSlug and ByHCID: the library books
// whose "hardcover" (slug) or "hardcover-id" identifier names one of d's
// books, as an editor can set in the metadata editor.
func hcLibraryIdentifiers(lib *hardcoverseries.Library, x *hcTitleIndex, d *hardcover.SeriesDetail) error {
	lib.ByHCSlug, lib.ByHCID = map[string][]hardcoverseries.LibBook{}, map[int64][]hardcoverseries.LibBook{}
	var vals []any
	for i := range d.Entries {
		e := &d.Entries[i]
		for _, slug := range e.Slugs() {
			vals = append(vals, strings.ToLower(slug))
		}
		for _, id := range e.IDs() {
			vals = append(vals, strconv.FormatInt(id, 10))
		}
	}
	for chunk := range slices.Chunk(vals, 400) {
		args := append([]any{hardcover.SlugScheme, hardcover.IDScheme}, chunk...)
		rows, err := calibreDB.Query(`SELECT book, lower(type), trim(val) FROM identifiers WHERE lower(type) IN (?, ?) AND lower(trim(val)) IN (?`+
			strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var typ, val string
			if err := rows.Scan(&id, &typ, &val); err != nil {
				rows.Close()
				return err
			}
			slot, ok := x.byID[id]
			if !ok || x.cat.books[slot].Removed {
				continue
			}
			b := x.libBook(slot)
			if typ == hardcover.SlugScheme {
				k := strings.ToLower(val)
				lib.ByHCSlug[k] = append(lib.ByHCSlug[k], b)
			} else if n, err := strconv.ParseInt(val, 10, 64); err == nil && n > 0 {
				lib.ByHCID[n] = append(lib.ByHCID[n], b)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// hcSeriesISBNs: the ISBN-13s of the series' library books (at most 50),
// for finding the series on Hardcover.
func hcSeriesISBNs(s hardcoverseries.SeriesInfo) ([]string, error) {
	if calibreDB == nil {
		return nil, nil
	}
	c := currentCatalog()
	var ids []any
	for i := range c.books {
		b := &c.books[i]
		if !b.Removed && b.Series != 0 && hardcoverseries.InMembers(c.seriesNames[b.Series], s.Members) {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 400 {
		ids = ids[:400]
	}
	rows, err := calibreDB.Query(`SELECT val FROM identifiers WHERE type = 'isbn' AND book IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if n := isbn.Normalize(v); n != "" && !slices.Contains(out, n) && len(out) < 50 {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

// hcSeriesTitles: the titles of the series' books in the library.
func hcSeriesTitles(s hardcoverseries.SeriesInfo) []string {
	c := currentCatalog()
	var out []string
	for i := range c.books {
		b := &c.books[i]
		if !b.Removed && b.Series != 0 && hardcoverseries.InMembers(c.seriesNames[b.Series], s.Members) {
			out = append(out, b.Title)
		}
	}
	return out
}

func hcSeriesAuthors(s hardcoverseries.SeriesInfo) []string {
	c := currentCatalog()
	var out []string
	for i := range c.books {
		b := &c.books[i]
		if !b.Removed && b.Series != 0 && hardcoverseries.InMembers(c.seriesNames[b.Series], s.Members) {
			for _, a := range b.AuthorIDs {
				if n := c.authorNames[a]; !slices.Contains(out, n) {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

func hcReviewURL(name string) string {
	return "/missing/hardcover?series=" + url.QueryEscape(name)
}

var (
	// hcLinks keeps series' links to Hardcover and runs the queue of
	// requests to it; nil without the app database.
	hcLinks *hardcoverseries.Service
)

// newHCLinks makes the Service on the app database; nil without it.
func newHCLinks() *hardcoverseries.Service {
	if appDB == nil {
		return nil
	}
	return hardcoverseries.New(hardcoverseries.Config{
		DB:   appDB,
		Host: hcSeriesHost{},
		Client: func() *hardcover.Client {
			if hardcoverEnabled() {
				return hcClient
			}
			return nil
		},
		Now: func() time.Time { return hcNow() },
		Background: &hardcoverseries.Background{
			Kind: hcJobAuthor, Spacing: hcAuthorSpacing, Run: hcLookupAuthor,
		},
	})
}

// startHardcover takes up the queue left from the last run and starts the
// worker.
func startHardcover() { hcLinks.Start() }

// stopHardcover stops the worker; a job it was running is queued again.
func stopHardcover(deadline time.Time) { hcLinks.Stop(deadline) }

// hcStatuses: every linked series' share of its Hardcover list, by
// lower-cased name (seriesStatuses caches it).
func hcStatuses() map[string]seriesStatus {
	if hcLinks == nil {
		return nil
	}
	revs := hcLinks.LinkedReviews()
	m := make(map[string]seriesStatus, len(revs))
	for k, rev := range revs {
		m[k] = hcReviewStatus(rev)
	}
	return m
}

func hcReviewStatus(r hardcoverseries.Review) seriesStatus {
	return seriesStatus{Numbered: r.Numbered, Have: r.Have, Complete: r.Complete(), Source: statusHardcover}
}

// hcPlaceholderViews: placeholders as matching sees them.
func hcPlaceholderViews(phs []*placeholder) []hardcoverseries.Placeholder {
	out := make([]hardcoverseries.Placeholder, len(phs))
	for i, p := range phs {
		out[i] = hardcoverseries.Placeholder{UUID: p.UUID, Title: p.Meta.Title, Series: p.Meta.Series,
			Authors: p.Meta.Creators, Index: p.index(), HCID: identifierValue(p.Meta, hardcover.IDScheme)}
	}
	return out
}

// hcSeriesHost is hardcoverseries.Host.
type hcSeriesHost struct{}

func (hcSeriesHost) Series(name string) (hardcoverseries.SeriesInfo, bool) { return hcSeries(name) }
func (hcSeriesHost) Library(s hardcoverseries.SeriesInfo, d *hardcover.SeriesDetail) (hardcoverseries.Library, error) {
	return hcLibraryFor(s, d)
}
func (hcSeriesHost) SeriesISBNs(s hardcoverseries.SeriesInfo) ([]string, error) {
	return hcSeriesISBNs(s)
}
func (hcSeriesHost) SeriesTitles(s hardcoverseries.SeriesInfo) []string  { return hcSeriesTitles(s) }
func (hcSeriesHost) SeriesAuthors(s hardcoverseries.SeriesInfo) []string { return hcSeriesAuthors(s) }
func (hcSeriesHost) Placeholders() []hardcoverseries.Placeholder {
	return hcPlaceholderViews(allPlaceholders())
}
func (hcSeriesHost) StatusChanged()                 { seriesStatusChanged() }
func (hcSeriesHost) ReviewURL(series string) string { return hcReviewURL(series) }
