package main

// Gaps in the library's series, for /missing (placeholders.go): the whole
// numbers from 1 to a series' highest (a book's or a placeholder's, or the
// number of books an editor gave it: series_complete.go) that neither a
// book nor a placeholder has.
//
// Nothing runs in the background. The first /missing view after the
// catalog is built (at startup) reads every book's series number once.
// After that, each change to the catalog (catalogFlush: books added,
// edited, recycled) moves only the changed books' numbers, from their old
// entries to their new ones, and a placeholder saved or deleted names its
// series. Either marks just those series; the next /missing view works out
// their gaps again, and keeps the rest as they were.

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"sync"
)

// seriesGap: a series' missing numbers, for /missing.
type seriesGap struct {
	Series  string
	Href    string // the series page
	Numbers []int  // missing, at most gapsShown
	More    int    // missing beyond those
	// BooksMissing: how many books short of the number an editor gave the
	// series (series_complete.go), placeholders in it counting as books.
	BooksMissing int
	Authors      string // of one of its books, to start a placeholder with
}

const (
	gapsShown = 12
	// gapsMaxIndex: series numbered past this (years, issue numbers) are
	// left out.
	gapsMaxIndex = 500
)

// seriesNumbers: the numbers a series' books in the library have.
type seriesNumbers struct {
	name    string
	have    map[float64]int // number -> books with it
	authors string
}

var gaps struct {
	sync.Mutex
	from   *catalog                  // the catalog series reflects; nil until the first /missing view
	series map[string]*seriesNumbers // by lower-case name
	dirty  map[string]string         // series to work out again: lower-case -> name
	found  map[string]seriesGap      // by lower-case name; only series with gaps
}

func gapKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// gapsReset forgets everything: the catalog was built again (startup).
func gapsReset() {
	gaps.Lock()
	defer gaps.Unlock()
	gaps.from, gaps.series, gaps.dirty, gaps.found = nil, nil, nil, nil
}

// gapsMarkLocked marks a series to work out again.
func gapsMarkLocked(name string) {
	if name = strings.TrimSpace(name); name == "" {
		return
	}
	if gaps.dirty == nil {
		gaps.dirty = map[string]string{}
	}
	gaps.dirty[gapKey(name)] = name
}

// gapsSeriesChanged: the number of books an editor gave these series
// changed (series_complete.go).
func gapsSeriesChanged(series ...string) { gapsPlaceholderChanged(series...) }

// gapsPlaceholderChanged: a placeholder in these series was made, edited or
// deleted.
func gapsPlaceholderChanged(series ...string) {
	gaps.Lock()
	defer gaps.Unlock()
	if gaps.from == nil {
		return // nothing worked out yet
	}
	for _, s := range series {
		gapsMarkLocked(s)
	}
}

// gapsBookLocked moves one book's number in or out of its series' counts.
func gapsBookLocked(c *catalog, b catalogBook, delta int) {
	if b.Series == 0 {
		return
	}
	name := c.seriesNames[b.Series]
	k := gapKey(name)
	s := gaps.series[k]
	if s == nil {
		if delta < 0 {
			return
		}
		s = &seriesNumbers{name: name, have: map[float64]int{}}
		gaps.series[k] = s
	}
	if s.have[b.SeriesIndex] += delta; s.have[b.SeriesIndex] <= 0 {
		delete(s.have, b.SeriesIndex)
	}
	if delta > 0 && s.authors == "" {
		s.authors = b.Authors
	}
	if len(s.have) == 0 {
		delete(gaps.series, k)
	}
	gapsMarkLocked(name)
}

// gapsCatalogChanged: catalogFlush made next from old, rereading or
// removing the books in uuids. Only their series change.
func gapsCatalogChanged(old, next *catalog, uuids []string) {
	gaps.Lock()
	defer gaps.Unlock()
	switch gaps.from {
	case nil, next:
		return // nothing worked out yet, or worked out from next already
	case old:
	default:
		gaps.from, gaps.series = nil, nil // lost track: start over at the next view
		return
	}
	for _, u := range uuids {
		if b, ok := old.lookup(u); ok {
			gapsBookLocked(old, b, -1)
		}
		if b, ok := next.lookup(u); ok {
			gapsBookLocked(next, b, +1)
		}
	}
	gaps.from = next
}

// seriesGaps: every series with gaps, by name, working out only what
// changed since the last call (all of them the first time).
func seriesGaps() []seriesGap {
	gaps.Lock()
	defer gaps.Unlock()
	if gaps.from == nil {
		c := currentCatalog()
		gaps.from, gaps.series, gaps.found, gaps.dirty = c, map[string]*seriesNumbers{}, map[string]seriesGap{}, nil
		for i := range c.books {
			if !c.books[i].Removed {
				gapsBookLocked(c, c.books[i], +1)
			}
		}
	}
	if len(gaps.dirty) > 0 {
		phs := allPlaceholders()
		for k := range gaps.dirty {
			if g, ok := gapsOf(gaps.series[k], phs); ok {
				gaps.found[k] = g
			} else {
				delete(gaps.found, k)
			}
		}
		gaps.dirty = nil
	}
	out := make([]seriesGap, 0, len(gaps.found))
	for _, g := range gaps.found {
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b seriesGap) int { return cmp.Compare(gapKey(a.Series), gapKey(b.Series)) })
	return out
}

// gapsOf works out one series' gaps; ok false when it has none (or isn't
// in the library: placeholders alone make no gaps).
func gapsOf(s *seriesNumbers, phs []*placeholder) (seriesGap, bool) {
	if s == nil {
		return seriesGap{}, false
	}
	// What an editor said of the series (its highest number, its number of
	// books). Not for a group's members, each only part of the series.
	_, members, _ := groups.resolve("series", s.name)
	count := seriesCount{}
	if len(members) <= 1 {
		count = seriesVerified(s.name)
	}
	g := seriesGap{Series: s.name, Href: categoryByKey("series").href(s.name), Authors: s.authors}
	if count.Books > 0 {
		n := 0
		for _, k := range s.have {
			n += k
		}
		for _, p := range phs {
			if strings.EqualFold(p.Meta.Series, s.name) {
				n++
			}
		}
		g.BooksMissing = max(0, count.Books-n)
	}
	have := map[int]bool{}
	top := 0.0
	for n := range s.have {
		if n == math.Trunc(n) {
			have[int(n)] = true
		}
		top = max(top, n)
	}
	for _, p := range phs {
		if !strings.EqualFold(p.Meta.Series, s.name) {
			continue
		}
		n := p.index()
		if n == math.Trunc(n) {
			have[int(n)] = true
		}
		top = max(top, n)
	}
	// A series an editor said has more books than its highest here: the
	// rest are missing too.
	top = max(top, float64(count.Total))
	if top > gapsMaxIndex {
		return g, g.BooksMissing > 0
	}
	for n := 1; n <= int(top); n++ {
		if have[n] {
			continue
		}
		if len(g.Numbers) < gapsShown {
			g.Numbers = append(g.Numbers, n)
		} else {
			g.More++
		}
	}
	return g, len(g.Numbers) > 0 || g.BooksMissing > 0
}
