package main

// "Recommended for you" on the Discover page (docs/notes/Discover.md,
// section 5), worked out in memory from the catalog and the user's marks;
// nothing is read from the library.
//
//   - Seeds: the books the user is reading (weight 3), paused (2),
//     finished (1.5), starred (3) and wants to read (1): what they're
//     reading now counts most.
//   - Tag affinity: each tag on the seeds scores the seeds' weights times
//     its idf (ln(N / books with the tag)), so a rare tag they share beats
//     "Fiction". Tags an admin excluded from the home page don't count
//     (tag_rows.go). The top few are also the user's own tag rows.
//   - Candidates: the books of the top tags and of every favorite tag (a
//     bounded number, in the shuffle order), scored by their tags' scores,
//     plus a small bonus for a seed's author. Books the user has marked in
//     any way, or said they're not interested in, are left out.
//   - Order: a weighted shuffle (Efraimidis–Spirakis, weight score^3), so
//     the best matches are mostly first but weaker ones come up now and
//     then. Its randomness comes from a seed that's the user's own, so the
//     row stays the same until they ask for more
//     (recommendations_state.go).
//   - Wildcards: a couple of books a row from outside the candidates,
//     picked from the shuffle, likelier the more of the user's tags they
//     have ("discover").
//   - Next in series first: for a series the user is reading or finished
//     a book of, the next book after the furthest one they're on.
//   - Favorites (aggregate_favorites.go): a favorite tag scores as if a
//     much-loved book had it, and its books are always candidates; a
//     favorite author's or publisher's books are a few candidates more,
//     with a small bonus (they have their own row, Followed); a favorite
//     series the user hasn't started offers its first book with the
//     next-in-series ones. Favorites alone are enough for a list.
//   - Pages: the whole list, a row at a time, laid out so no author (after
//     author groups) has more than two books in a row unless there's
//     nothing else left. Refresh steps to the next page, and after the last
//     back to the first with a new seed.
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
	recAuthorBonus  = 0.2  // of the best tag's score
	recSharpness    = 3    // the weighted shuffle's weight: score^this
	recSeriesMax    = 6
	recAuthorPerRow = 2 // books by one author in a row, at most

	recWReading  = 3
	recWPaused   = 2
	recWFinished = 1.5
	recWStarred  = 3
	recWWanted   = 1

	recFavTagWeight      = 4   // a favorite tag: as a seed of this weight with it
	recFavAuthorBonus    = 0.3 // of the best tag's score
	recFavPublisherBonus = 0.1
	recPerFav            = 48 // candidates from favorite authors and publishers, in all

	recWildcards  = 2    // per row
	recWildWalk   = 2000 // books of the shuffle looked at for them
	recWildFloor  = 0.05 // the chance of a book with none of the user's tags
	recWildAffine = 0.5  // the chance more for one as good as the best match
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

// favIDs: which catalog ids of kind ("author", "publisher", "series") are
// favorites. Worked out from the favorites, which are few: each one's name
// and, for a group, its members' names, looked up by name (byLower); each
// id found still passes has. Not by asking has of every name in the
// library: each ask goes through every group (groups.resolve), and a big
// library has tens of thousands of authors.
func (ix *recIndex) favIDs(f recFavs, kind string) map[int32]bool {
	out := map[int32]bool{}
	names := map[string]map[int32]string{"author": ix.cat.authorNames, "publisher": ix.cat.publisherNames, "series": ix.cat.seriesNames}[kind]
	for fav := range f[kind] {
		for _, n := range favNames(kind, fav) {
			for _, id := range ix.byLower[kind][strings.ToLower(n)] {
				if f.has(kind, names[id]) {
					out[id] = true
				}
			}
		}
	}
	return out
}

// favTags: which tag entities are favorites (as favIDs).
func (ix *recIndex) favTags(f recFavs) map[int32]bool {
	out := map[int32]bool{}
	for fav := range f["tag"] {
		for _, n := range favNames("tag", fav) {
			if e, ok := ix.entityByLower[strings.ToLower(n)]; ok && f.has("tag", ix.names[e]) {
				out[e] = true
			}
		}
	}
	return out
}

// favNames: the names a favorite (as browsed) stands for: itself, and a
// group's members.
func favNames(kind, fav string) []string {
	out := []string{fav}
	if g, members, grouped := groups.resolve(kind, fav); grouped && strings.EqualFold(g, fav) {
		out = append(out, members...)
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

	// Names to look favorites up by (favIDs, favTags): kind -> lower-cased
	// name -> catalog ids, and lower-cased tag entity -> entity.
	byLower       map[string]map[string][]int32
	entityByLower map[string]int32

	// For favorites (rankRecs, followedBooks): each author's and publisher's
	// books, the books not removed, and each book's place in the shuffle and
	// in Latest (newest added first; -1: removed). Going through a few
	// favorites' books, not every book in the library, on each Home.
	byAuthor    map[int32][]int32
	byPublisher map[int32][]int32
	total       int
	shufflePos  []int32
	latestPos   []int32

	// authorKey: a grouped author's id -> its group (negative), so a
	// group's members count as one author in a row (authorKeys).
	authorKey map[int32]int32
}

// authorKeys: who a book counts as in a row's author cap: its authors,
// a grouped one as its group, each once.
func (ix *recIndex) authorKeys(b *catalogBook) []int32 {
	out := make([]int32, 0, len(b.AuthorIDs))
	for _, a := range b.AuthorIDs {
		k := a
		if g, ok := ix.authorKey[a]; ok {
			k = g
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// rowAuthors counts a row's books by author (authorKeys).
type rowAuthors map[int32]int

func (r rowAuthors) fits(keys []int32) bool {
	for _, k := range keys {
		if r[k] >= recAuthorPerRow {
			return false
		}
	}
	return true
}

func (r rowAuthors) add(keys []int32) {
	for _, k := range keys {
		r[k]++
	}
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
	ix.entityByLower = byName
	ix.byLower = map[string]map[string][]int32{}
	for kind, names := range map[string]map[int32]string{"author": c.authorNames, "publisher": c.publisherNames, "series": c.seriesNames} {
		m := make(map[string][]int32, len(names))
		for id, n := range names {
			k := strings.ToLower(n)
			m[k] = append(m[k], id)
		}
		ix.byLower[kind] = m
	}
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
	// Author groups: members first, then names, as groups.resolve prefers a
	// group's name to another's member.
	ix.authorKey = map[int32]int32{}
	gs := groups.all("author")
	for pass := range 2 {
		for gi, g := range gs {
			names := g.Members
			if pass == 1 {
				names = []string{g.Name}
			}
			for _, n := range names {
				for _, id := range ix.byLower["author"][strings.ToLower(n)] {
					ix.authorKey[id] = int32(-gi - 1)
				}
			}
		}
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

// recItem: a book of the list, and why it's there.
type recItem struct {
	slot    int32
	kind    string  // "series", "tags" or "discover"
	after   string  // a series item: the book it follows
	authors []int32 // who it counts as in a row (authorKeys)
}

// recList: the user's whole list, laid out in rows (layoutRows); a tag
// candidate's "because" is worked out only when asked for (it costs).
type recList struct {
	items []recItem
	cat   *catalog
	seeds []recSeed
	ix    *recIndex
}

// uuid: the book at i, without making its recBook.
func (l recList) uuid(i int) string { return l.cat.books[l.items[i].slot].UUID }

func (l recList) len() int { return len(l.items) }

func (l recList) at(i int) recBook {
	it := l.items[i]
	b := &l.cat.books[it.slot]
	r := recBook{Book: b.Book, Kind: it.kind, Because: it.after}
	if it.kind == "tags" {
		r.Because = because(l.ix, b, l.seeds)
	}
	return r
}

// pick: up to n of the list's books, in order's order, with no author
// over recAuthorPerRow in the row (counts: the books already in it, and
// what's picked is added); if too few fit, the rest of order fills it.
func (l recList) pick(order []int, n int, counts rowAuthors) []int {
	var out, over []int
	for _, i := range order {
		if len(out) >= n {
			break
		}
		if a := l.items[i].authors; counts.fits(a) {
			counts.add(a)
			out = append(out, i)
		} else {
			over = append(over, i)
		}
	}
	for _, i := range over {
		if len(out) >= n {
			break
		}
		counts.add(l.items[i].authors)
		out = append(out, i)
	}
	return out
}

// layoutRows: the list in rows of recRowMax. main (next in series, then
// the candidates in order) fills each row in turn, but a book that would
// put one of its authors over recAuthorPerRow in the row waits for a
// later one, ahead of the books after it; recWildcards wildcards go in at
// random places in each row's back half. Only when nothing else is left
// do the waiting books go in regardless.
func layoutRows(main, wild []recItem, rng *rand.Rand) []recItem {
	out := make([]recItem, 0, len(main)+len(wild))
	var wait []recItem
	next := 0
	for next < len(main) || len(wait) > 0 {
		counts := rowAuthors{}
		fits := func(it recItem) bool { return counts.fits(it.authors) }
		take := func() (recItem, bool) {
			if j := slices.IndexFunc(wait, fits); j >= 0 {
				it := wait[j]
				wait = slices.Delete(wait, j, j+1)
				return it, true
			}
			for next < len(main) {
				it := main[next]
				next++
				if fits(it) {
					return it, true
				}
				wait = append(wait, it)
			}
			if len(wait) > 0 { // nothing else left
				it := wait[0]
				wait = wait[1:]
				return it, true
			}
			return recItem{}, false
		}
		wildAt := map[int]bool{}
		for len(wildAt) < min(recWildcards, len(wild)) {
			wildAt[recRowMax/2+rng.IntN(recRowMax-recRowMax/2)] = true
		}
		for pos := 0; pos < recRowMax; pos++ {
			if wildAt[pos] {
				if j := slices.IndexFunc(wild, fits); j >= 0 {
					counts.add(wild[j].authors)
					out = append(out, wild[j])
					wild = slices.Delete(wild, j, j+1)
					continue
				}
			}
			it, ok := take()
			if !ok {
				break
			}
			counts.add(it.authors)
			out = append(out, it)
		}
	}
	return out
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
	l.cat, l.ix = c, ix

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
	add(reading, recWReading)
	add(m.Paused, recWPaused)
	add(m.Finished, recWFinished)
	add(m.Favorites, recWStarred)
	add(m.Wanted, recWWanted)
	// Dropped: never suggested, but nothing to suggest more like.
	for _, u := range m.Dropped {
		marked[u] = true
	}
	favAuthors := ix.favIDs(favs, "author")
	favPublishers := ix.favIDs(favs, "publisher")
	favSeries := ix.favIDs(favs, "series")
	favTags := ix.favTags(favs)
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
	var series []recItem
	for _, n := range nexts {
		b := &c.books[n.slot]
		if len(series) >= recSeriesMax || hide[b.UUID] || (marked[b.UUID] && !slices.Contains(m.Wanted, b.UUID)) {
			continue
		}
		shown[n.slot] = true
		series = append(series, recItem{n.slot, "series", n.after, ix.authorKeys(b)})
	}
	l.seeds = seeds

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
		l.items = layoutRows(series, nil, rng)
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
	tagScore := func(b *catalogBook) float64 {
		sc := 0.0
		for _, t := range ix.bookTags(b) {
			sc += score[t]
		}
		return sc
	}

	// Candidates, each with its key in the weighted shuffle
	// (Efraimidis–Spirakis: u^(1/w), as its log; w relative to the best tag).
	type cand struct {
		slot int32
		key  float64
	}
	var cands []cand
	seen := map[int32]bool{}
	addCand := func(slot int32, sc float64) {
		seen[slot] = true
		w := math.Pow(max(sc/best, 1e-9), recSharpness)
		cands = append(cands, cand{slot, math.Log(rng.Float64()) / w})
	}
	// The top tags' books, and every favorite tag's.
	for i, e := range top {
		if i >= recTopTags && !favTags[e] {
			continue
		}
		for _, slot := range ix.postings[e][:min(len(ix.postings[e]), recPerTag)] {
			b := &c.books[slot]
			if seen[slot] || shown[slot] || marked[b.UUID] || hide[b.UUID] {
				continue
			}
			addCand(slot, tagScore(b)+bonus(b))
		}
	}
	// A few of favorite authors' and publishers' books.
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
			n++
			addCand(slot, tagScore(b)+bonus(b))
		}
	}
	slices.SortFunc(cands, func(a, b cand) int {
		if a.key != b.key {
			return cmp.Compare(b.key, a.key)
		}
		return cmp.Compare(a.slot, b.slot)
	})
	main := slices.Clip(series)
	for _, cd := range cands {
		main = append(main, recItem{cd.slot, "tags", "", ix.authorKeys(&c.books[cd.slot])})
	}

	// Wildcards: books of the shuffle from a random place, not candidates,
	// each taken with a chance that grows with its share of the user's tags.
	var wild []recItem
	want := (len(main)/(recRowMax-recWildcards) + 1) * recWildcards
	if n := len(c.shuffle); n > 0 && len(main) > 0 {
		start := rng.IntN(n)
		for k := 0; k < min(n, recWildWalk) && len(wild) < want; k++ {
			slot := c.shuffle[(start+k)%n]
			b := &c.books[slot]
			if b.Removed || seen[slot] || shown[slot] || marked[b.UUID] || hide[b.UUID] {
				continue
			}
			if rng.Float64() < recWildFloor+recWildAffine*min(1, tagScore(b)/best) {
				seen[slot] = true
				wild = append(wild, recItem{slot, "discover", "", ix.authorKeys(b)})
			}
		}
	}
	l.items = layoutRows(main, wild, rng)
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
