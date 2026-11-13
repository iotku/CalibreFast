package main

// "Recommended for you" on the Discover page (docs/notes/Discover.md,
// section 5), worked out in memory from the catalog and the user's marks;
// nothing is read from the library.
//
//   - Seeds: the books the user is reading (weight 2), finished (2),
//     starred (3) and wants to read (1).
//   - Tag affinity: each tag on the seeds scores the seeds' weights times
//     its idf (ln(N / books with the tag)), so a rare tag they share beats
//     "Fiction". Tags an admin excluded from the home page don't count
//     (tag_rows.go). The top few are also the user's own tag rows.
//   - Candidates: the books of the top tags (a bounded number, in the
//     shuffle order), scored by their tags' scores, plus a bonus for a
//     seed's author, with a little randomness so the row changes between
//     visits. Books the user has marked in any way are left out.
//   - Next in series first: for a series the user is reading or finished
//     a book of, the next book after the furthest one they're on.
//
// Tags count after tag groups, as everywhere else.

import (
	"cmp"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	recRowMax       = 24
	recTopTags      = 8    // tags whose books are candidates
	recPerTag       = 1000 // candidates from each
	recAffinityTags = 3    // the user's own tag rows
	recAuthorBonus  = 0.5  // of the best tag's score
	recJitter       = 0.15 // up to this share of a score, at random
	recSeriesMax    = 6
)

// recSeed is one of the user's books recommendations start from.
type recSeed struct {
	b    *catalogBook
	w    float64
	tags []int32
}

// recBook is a book in the Recommended row, and why.
type recBook struct {
	Book    Book   `json:"book"`
	Kind    string `json:"kind"`              // "series" (next in a series) or "tags"
	Because string `json:"because,omitempty"` // the user's book it follows or is like
}

// tagSettingsVersion changes when the admin's tag settings do, so the
// recommendation index is made again without the excluded tags.
var tagSettingsVersion atomic.Int64

// recIndex: the catalog's tags (after groups, without excluded ones) and
// series, in the form scoring needs. Made once per catalog, groups and
// settings.
type recIndex struct {
	cat       *catalog
	groupsVer uint64
	settings  int64

	entity   map[int32]int32 // tag id -> entity
	names    []string        // entity -> name
	postings [][]int32       // entity -> book slots, shuffle order
	series   map[int32][]int32
}

var (
	recIndexMu  sync.Mutex
	recIndexNow atomic.Pointer[recIndex]
)

func currentRecIndex() *recIndex {
	c := currentCatalog()
	gv := groups.currentVersion()
	sv := tagSettingsVersion.Load()
	if ix := recIndexNow.Load(); ix != nil && ix.cat == c && ix.groupsVer == gv && ix.settings == sv {
		return ix
	}
	recIndexMu.Lock()
	defer recIndexMu.Unlock()
	if ix := recIndexNow.Load(); ix != nil && ix.cat == c && ix.groupsVer == gv && ix.settings == sv {
		return ix
	}
	deny := map[string]bool{}
	if appDB != nil {
		if set, err := loadDiscoverTagSettings(); err == nil {
			deny = set.deny
		}
	}
	ix := &recIndex{cat: c, groupsVer: gv, settings: sv, entity: map[int32]int32{}, series: map[int32][]int32{}}
	byName := map[string]int32{}
	for id, name := range c.tagNames {
		e := tagEntity(name)
		key := strings.ToLower(e)
		if deny[key] {
			continue
		}
		n, ok := byName[key]
		if !ok {
			n = int32(len(ix.names))
			byName[key] = n
			ix.names = append(ix.names, e)
		}
		ix.entity[id] = n
	}
	ix.postings = make([][]int32, len(ix.names))
	for _, slot := range c.shuffle {
		b := &c.books[slot]
		if b.Removed {
			continue
		}
		for _, e := range ix.bookTags(b) {
			ix.postings[e] = append(ix.postings[e], slot)
		}
		if b.Series != 0 {
			ix.series[b.Series] = append(ix.series[b.Series], slot)
		}
	}
	for _, slots := range ix.series {
		slices.SortStableFunc(slots, func(a, b int32) int {
			return cmp.Compare(c.books[a].SeriesIndex, c.books[b].SeriesIndex)
		})
	}
	recIndexNow.Store(ix)
	return ix
}

// bookTags: a book's tags as entities, each once.
func (ix *recIndex) bookTags(b *catalogBook) []int32 {
	out := make([]int32, 0, len(b.TagIDs))
	for _, t := range b.TagIDs {
		if e, ok := ix.entity[t]; ok && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// recommend works out the Recommended row and the user's strongest tags.
func recommend(m readingMarks, rng *rand.Rand) (recs []recBook, tags []string) {
	ix := currentRecIndex()
	c := ix.cat
	recs = []recBook{}
	tags = []string{}

	marked := map[string]bool{}
	weight := map[string]float64{}
	add := func(uuids []string, w float64) {
		for _, u := range uuids {
			marked[u] = true
			weight[u] += w
		}
	}
	reading := make([]string, 0, len(m.Reading))
	for u := range m.Reading {
		reading = append(reading, u)
	}
	slices.Sort(reading)
	add(reading, 2)
	add(m.Finished, 2)
	add(m.Favorites, 3)
	add(m.Wanted, 1)
	if len(weight) == 0 {
		return recs, tags
	}

	// Seeds in the catalog, in a fixed order (scores don't depend on map order).
	var seeds []recSeed
	for u, w := range weight {
		i, ok := c.byUUID[u]
		if !ok || c.books[i].Removed {
			continue
		}
		b := &c.books[i]
		seeds = append(seeds, recSeed{b, w, ix.bookTags(b)})
	}
	slices.SortFunc(seeds, func(a, b recSeed) int { return cmp.Compare(a.b.UUID, b.b.UUID) })
	seedAuthors := map[int32]bool{}
	for _, s := range seeds {
		for _, a := range s.b.AuthorIDs {
			seedAuthors[a] = true
		}
	}

	// Next in series: the book after the furthest one read in each series.
	progress := map[string]bool{}
	for _, u := range reading {
		progress[u] = true
	}
	for _, u := range m.Finished {
		progress[u] = true
	}
	type seriesNext struct {
		slot  int32
		after string
	}
	furthest := map[int32]int32{} // series -> slot of the furthest read
	for _, s := range seeds {
		if s.b.Series == 0 || !progress[s.b.UUID] {
			continue
		}
		at := c.byUUID[s.b.UUID]
		if f, ok := furthest[s.b.Series]; !ok || c.books[at].SeriesIndex > c.books[f].SeriesIndex {
			furthest[s.b.Series] = at
		}
	}
	var nexts []seriesNext
	for series, f := range furthest {
		for _, slot := range ix.series[series] {
			b := &c.books[slot]
			if b.SeriesIndex > c.books[f].SeriesIndex && !progress[b.UUID] {
				nexts = append(nexts, seriesNext{slot, c.books[f].Title})
				break
			}
		}
	}
	slices.SortFunc(nexts, func(a, b seriesNext) int { return cmp.Compare(c.books[a.slot].Title, c.books[b.slot].Title) })
	shown := map[int32]bool{}
	for _, n := range nexts[:min(len(nexts), recSeriesMax)] {
		b := &c.books[n.slot]
		if marked[b.UUID] && !slices.Contains(m.Wanted, b.UUID) {
			continue
		}
		shown[n.slot] = true
		recs = append(recs, recBook{Book: b.Book, Kind: "series", Because: n.after})
	}

	// Tag affinity.
	total := 0
	for i := range c.books {
		if !c.books[i].Removed {
			total++
		}
	}
	score := map[int32]float64{}
	for _, s := range seeds {
		for _, e := range s.tags {
			if n := len(ix.postings[e]); n > 0 {
				score[e] += s.w * math.Log(float64(total)/float64(n))
			}
		}
	}
	top := make([]int32, 0, len(score))
	for e, sc := range score {
		if sc > 0 {
			top = append(top, e)
		}
	}
	slices.SortFunc(top, func(a, b int32) int {
		if score[a] != score[b] {
			return cmp.Compare(score[b], score[a])
		}
		return cmp.Compare(ix.names[a], ix.names[b])
	})
	for _, e := range top[:min(len(top), recAffinityTags)] {
		tags = append(tags, ix.names[e])
	}
	if len(top) == 0 {
		return recs, tags
	}
	best := score[top[0]]

	// Candidates: the top tags' books.
	type cand struct {
		slot  int32
		score float64
	}
	var cands []cand
	seen := map[int32]bool{}
	for _, e := range top[:min(len(top), recTopTags)] {
		for _, slot := range ix.postings[e][:min(len(ix.postings[e]), recPerTag)] {
			b := &c.books[slot]
			if seen[slot] || shown[slot] || marked[b.UUID] {
				continue
			}
			seen[slot] = true
			sc := 0.0
			for _, t := range ix.bookTags(b) {
				sc += score[t]
			}
			for _, a := range b.AuthorIDs {
				if seedAuthors[a] {
					sc += recAuthorBonus * best
					break
				}
			}
			sc *= 1 + recJitter*rng.Float64()
			cands = append(cands, cand{slot, sc})
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		return cmp.Compare(a.slot, b.slot)
	})
	for _, cd := range cands {
		if len(recs) >= recRowMax {
			break
		}
		b := &c.books[cd.slot]
		recs = append(recs, recBook{Book: b.Book, Kind: "tags", Because: because(ix, b, seeds)})
	}
	return recs, tags
}

// because: the seed sharing the most tags with b (the heaviest, on a tie).
func because(ix *recIndex, b *catalogBook, seeds []recSeed) string {
	bt := ix.bookTags(b)
	bestN, bestW, title := 0, 0.0, ""
	for _, s := range seeds {
		n := 0
		for _, t := range s.tags {
			if slices.Contains(bt, t) {
				n++
			}
		}
		if n > bestN || (n == bestN && n > 0 && s.w > bestW) {
			bestN, bestW, title = n, s.w, s.b.Title
		}
	}
	return title
}
