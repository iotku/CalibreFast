package main

// The series an author has books in, for the top of their page
// (filtered.html). Built from the catalog, not metadata.db, and cached per
// catalog: every change to a book swaps in a new *catalog (catalog.go), so
// the next author page after one rebuilds the index, and until then no page
// reads the library.

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// AuthorSeries is one series an author has books in.
type AuthorSeries struct {
	Name  string // series, or its group's name
	Href  string // its filtered page
	Count int    // the author's books in it
	// Status: how much of the series is here (seriesStatuses), nil when
	// it isn't counted; set by the page, not cached.
	Status *seriesStatus
}

type authorSeriesIndex struct {
	cat *catalog
	// byName: lower-cased author name -> series id -> the author's book
	// slots in it. Names, not ids: an author page is addressed by name
	// (authors.name is COLLATE NOCASE).
	byName map[string]map[int32][]int32
}

var (
	authorSeriesMu  sync.Mutex
	authorSeriesNow atomic.Pointer[authorSeriesIndex]
)

func currentAuthorSeriesIndex() *authorSeriesIndex {
	c := currentCatalog()
	if ix := authorSeriesNow.Load(); ix != nil && ix.cat == c {
		return ix
	}
	authorSeriesMu.Lock()
	defer authorSeriesMu.Unlock()
	if ix := authorSeriesNow.Load(); ix != nil && ix.cat == c {
		return ix
	}
	ix := &authorSeriesIndex{cat: c, byName: map[string]map[int32][]int32{}}
	for slot := range c.books {
		b := &c.books[slot]
		if b.Removed || b.Series == 0 {
			continue
		}
		for _, a := range b.AuthorIDs {
			name := strings.ToLower(c.authorNames[a])
			m := ix.byName[name]
			if m == nil {
				m = map[int32][]int32{}
				ix.byName[name] = m
			}
			m[b.Series] = append(m[b.Series], int32(slot))
		}
	}
	authorSeriesNow.Store(ix)
	return ix
}

// authorSeries lists the series the author (or the author group's members)
// have books in, most books first. A series group's members fold into one
// entry; a book shared by two members counts once.
func authorSeries(members []string) []AuthorSeries {
	ix := currentAuthorSeriesIndex()
	series := categoryByKey("series")
	books := map[string]map[int32]bool{} // lower-cased series (group) name -> slots
	var out []AuthorSeries
	for _, m := range members {
		for id, slots := range ix.byName[strings.ToLower(m)] {
			g, _, _ := groups.resolve("series", ix.cat.seriesNames[id])
			key := strings.ToLower(g)
			set := books[key]
			if set == nil {
				set = map[int32]bool{}
				books[key] = set
				out = append(out, AuthorSeries{Name: g, Href: series.href(g)})
			}
			for _, s := range slots {
				set[s] = true
			}
		}
	}
	for i := range out {
		out[i].Count = len(books[strings.ToLower(out[i].Name)])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}
