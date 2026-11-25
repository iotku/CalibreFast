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
//     seed; the seed is the user's own, so the row stays the same until
//     they ask for more (recommendations_state.go). Books the user has
//     marked in any way, or said they're not interested in, are left out.
//   - Next in series first: for a series the user is reading or finished
//     a book of, the next book after the furthest one they're on.
//   - Favorites (aggregate_favorites.go): a favorite tag scores as if a
//     starred book had it; a favorite author's or publisher's books are
//     candidates too, with a bonus (an author's beats a seed author's); a
//     favorite series the user hasn't started offers its first book with
//     the next-in-series ones. Favorites alone are enough for a list.
//   - Pages: the whole ranked list, a row at a time. Refresh steps to the
//     next page, and after the last back to the first with a new seed.
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
	recJitter       = 0.15 // up to this share of a score, at random (the seed's)
	recSeriesMax    = 6

	recFavTagWeight      = 3   // a favorite tag: as a starred seed with it
	recFavAuthorBonus    = 1.0 // of the best tag's score
	recFavPublisherBonus = 0.25
	recPerFav            = 1000 // candidates from favorite authors and publishers, in all
)

// recFavs: a user's favorites, by kind, as browsed (lower case).
type recFavs map[string]map[string]bool

func (f recFavs) has(kind, name string) bool {
	if f[kind] == nil {
		return false
	}
	g, _, _ := groups.resolve(kind, name)
	return f[kind][strings.ToLower(g)]
}

// ids: which of names (catalog ids to raw names) are favorites of kind.
func (f recFavs) ids(kind string, names map[int32]string) map[int32]bool {
	out := map[int32]bool{}
	if len(f[kind]) == 0 {
		return out
	}
	for id, n := range names {
		if f.has(kind, n) {
			out[id] = true
		}
	}
	return out
}

// userRecFavs: u's favorites for ranking.
func userRecFavs(u *User) (recFavs, error) {
	rows, err := appDB.Query(`SELECT kind, name FROM aggregate_favorites WHERE user_id = ? AND removed_at IS NULL`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	f := recFavs{}
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, err
		}
		g, _, _ := groups.resolve(kind, name)
		if f[kind] == nil {
			f[kind] = map[string]bool{}
		}
		f[kind][strings.ToLower(g)] = true
	}
	return f, rows.Err()
}

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

	// For favorites (rankRecs, followedBooks): each author's and publisher's
	// books, the books not removed, and each book's place in the shuffle and
	// in Latest (newest added first; -1: removed). Going through a few
	// favorites' books, not every book in the library, on each Home.
	byAuthor    map[int32][]int32
	byPublisher map[int32][]int32
	total       int
	shufflePos  []int32
	latestPos   []int32
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
	ix.byAuthor, ix.byPublisher = map[int32][]int32{}, map[int32][]int32{}
	ix.shufflePos, ix.latestPos = make([]int32, len(c.books)), make([]int32, len(c.books))
	for i := range ix.latestPos {
		ix.latestPos[i] = -1
	}
	for i, slot := range c.latest() {
		ix.latestPos[slot] = int32(i)
	}
	for i, slot := range c.shuffle {
		ix.shufflePos[slot] = int32(i)
		b := &c.books[slot]
		if b.Removed {
			continue
		}
		ix.total++
		for _, a := range b.AuthorIDs {
			ix.byAuthor[a] = append(ix.byAuthor[a], slot)
		}
		if b.Publisher != 0 {
			ix.byPublisher[b.Publisher] = append(ix.byPublisher[b.Publisher], slot)
		}
		for _, e := range ix.bookTags(b) {
			ix.postings[e] = append(ix.postings[e], slot)
		}
		if b.Series != 0 {
			ix.series[b.Series] = append(ix.series[b.Series], slot)
		}
	}
	for _, slots := range ix.series {
		sortSeries(c, slots) // series.go
	}
	recIndexNow.Store(ix)
	return ix
}

// favBooks: the books by these authors or from these publishers (removed
// ones aren't indexed), each once, in pos's order (shufflePos, latestPos).
func (ix *recIndex) favBooks(authors, publishers map[int32]bool, pos []int32) []int32 {
	seen := map[int32]bool{}
	var out []int32
	add := func(slots []int32) {
		for _, s := range slots {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	for a := range authors {
		add(ix.byAuthor[a])
	}
	for p := range publishers {
		add(ix.byPublisher[p])
	}
	slices.SortFunc(out, func(a, b int32) int { return cmp.Compare(pos[a], pos[b]) })
	return out
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

// recList: the user's whole ranked list, next in series first; a tag
// candidate's recBook is made only when asked for (its "because" costs).
type recList struct {
	series []recBook
	n      int // tag candidates
	cand   func(i int) recBook
	slot   func(i int) int32 // a tag candidate's book, cheaply
	cat    *catalog
}

// uuid: the book at i, without making its recBook.
func (l recList) uuid(i int) string {
	if i < len(l.series) {
		return l.series[i].Book.UUID
	}
	return l.cat.books[l.slot(i-len(l.series))].UUID
}

func (l recList) len() int { return len(l.series) + l.n }

func (l recList) at(i int) recBook {
	if i < len(l.series) {
		return l.series[i]
	}
	return l.cand(i - len(l.series))
}

// pages: how many rows of recRowMax the list makes.
func (l recList) pages() int { return (l.len() + recRowMax - 1) / recRowMax }

// pageUUIDs: the books of a row page, cheaply (no recBooks made); none for a
// page past the last (Next starts over then, ranked with new randomness).
func (l recList) pageUUIDs(page int) []string {
	var out []string
	if page < 0 || page >= l.pages() {
		return out
	}
	for i := page * recRowMax; i < min(l.len(), (page+1)*recRowMax); i++ {
		out = append(out, l.uuid(i))
	}
	return out
}

// page: the list's row page (a page past the last is the first).
func (l recList) page(page int) []recBook {
	if page < 0 || page >= l.pages() {
		page = 0
	}
	recs := []recBook{}
	for i := page * recRowMax; i < min(l.len(), (page+1)*recRowMax); i++ {
		recs = append(recs, l.at(i))
	}
	return recs
}

// recommend works out a page of the Recommended row (recRowMax books), how
// many pages there are, and the user's strongest tags. hide: books the user
// isn't interested in. A page past the last is the first.
func recommend(m readingMarks, hide map[string]bool, rng *rand.Rand, page int) (recs []recBook, pages int, tags []string) {
	l, tags := rankRecs(m, nil, hide, rng)
	return l.page(page), l.pages(), tags
}

// rankRecs works out the user's whole ranked list, and their strongest tags.
// favs: the user's favorites (nil: none).
func rankRecs(m readingMarks, favs recFavs, hide map[string]bool, rng *rand.Rand) (l recList, tags []string) {
	ix := currentRecIndex()
	c := ix.cat
	tags = []string{}
	l.cat = c

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
	add(m.Paused, 2)
	add(m.Finished, 2)
	add(m.Favorites, 3)
	add(m.Wanted, 1)
	// Dropped: never suggested, but nothing to suggest more like.
	for _, u := range m.Dropped {
		marked[u] = true
	}
	favAuthors := favs.ids("author", c.authorNames)
	favPublishers := favs.ids("publisher", c.publisherNames)
	favSeries := favs.ids("series", c.seriesNames)
	favTags := map[int32]bool{}
	for e, n := range ix.names {
		if favs.has("tag", n) {
			favTags[int32(e)] = true
		}
	}
	if len(weight) == 0 && len(favAuthors)+len(favPublishers)+len(favSeries)+len(favTags) == 0 {
		return l, tags
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
	for _, u := range m.Paused {
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
		if f, ok := furthest[s.b.Series]; !ok || seriesLess(&c.books[f], &c.books[at]) {
			furthest[s.b.Series] = at
		}
	}
	var nexts []seriesNext
	for series, f := range furthest {
		for _, slot := range ix.series[series] {
			b := &c.books[slot]
			if seriesLess(&c.books[f], b) && !progress[b.UUID] {
				nexts = append(nexts, seriesNext{slot, c.books[f].Title})
				break
			}
		}
	}
	// A favorite series not started: its first book not read.
	for series := range favSeries {
		if _, started := furthest[series]; started {
			continue
		}
		for _, slot := range ix.series[series] {
			if !progress[c.books[slot].UUID] {
				nexts = append(nexts, seriesNext{slot, ""})
				break
			}
		}
	}
	slices.SortFunc(nexts, func(a, b seriesNext) int { return cmp.Compare(c.books[a.slot].Title, c.books[b.slot].Title) })
	shown := map[int32]bool{}
	var series []recBook
	for _, n := range nexts {
		b := &c.books[n.slot]
		if len(series) >= recSeriesMax || hide[b.UUID] || (marked[b.UUID] && !slices.Contains(m.Wanted, b.UUID)) {
			continue
		}
		shown[n.slot] = true
		series = append(series, recBook{Book: b.Book, Kind: "series", Because: n.after})
	}
	l.series = series

	// Tag affinity.
	total := ix.total
	score := map[int32]float64{}
	idf := func(e int32) float64 {
		if n := len(ix.postings[e]); n > 0 {
			return math.Log(float64(total) / float64(n))
		}
		return 0
	}
	for _, s := range seeds {
		for _, e := range s.tags {
			score[e] += s.w * idf(e)
		}
	}
	for e := range favTags {
		score[e] += recFavTagWeight * idf(e)
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
	if len(top) == 0 && len(favAuthors)+len(favPublishers) == 0 {
		return l, tags
	}
	best := 1.0 // with only favorite authors or publishers, their bonus alone
	if len(top) > 0 {
		best = score[top[0]]
	}
	// bonus: what b's authors and publisher add to its score.
	bonus := func(b *catalogBook) float64 {
		bn := 0.0
		for _, a := range b.AuthorIDs {
			if favAuthors[a] {
				bn = recFavAuthorBonus
				break
			}
			if seedAuthors[a] {
				bn = recAuthorBonus
			}
		}
		if favPublishers[b.Publisher] {
			bn += recFavPublisherBonus
		}
		return bn * best
	}

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
			if seen[slot] || shown[slot] || marked[b.UUID] || hide[b.UUID] {
				continue
			}
			seen[slot] = true
			sc := 0.0
			for _, t := range ix.bookTags(b) {
				sc += score[t]
			}
			sc += bonus(b)
			sc *= 1 + recJitter*rng.Float64()
			cands = append(cands, cand{slot, sc})
		}
	}
	// Candidates: favorite authors' and publishers' books.
	if len(favAuthors)+len(favPublishers) > 0 {
		n := 0
		for _, slot := range ix.favBooks(favAuthors, favPublishers, ix.shufflePos) { // in shuffle order
			if n >= recPerFav {
				break
			}
			b := &c.books[slot]
			if b.Removed || seen[slot] || shown[slot] || marked[b.UUID] || hide[b.UUID] {
				continue
			}
			fav := favPublishers[b.Publisher]
			for _, a := range b.AuthorIDs {
				fav = fav || favAuthors[a]
			}
			if !fav {
				continue
			}
			seen[slot] = true
			n++
			sc := bonus(b)
			for _, t := range ix.bookTags(b) {
				sc += score[t]
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
	l.n = len(cands)
	l.slot = func(i int) int32 { return cands[i].slot }
	l.cand = func(i int) recBook {
		b := &c.books[cands[i].slot]
		return recBook{Book: b.Book, Kind: "tags", Because: because(ix, b, seeds)}
	}
	return l, tags
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
