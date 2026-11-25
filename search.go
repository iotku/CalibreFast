package main

// Search: /search and /api/search, answered from the in-memory catalog
// (catalog.go), so searching never reads metadata.db.
//
// A query is a list of terms, all of which a book must match:
//
//	tolkien hobbit          each word on its own, in any field
//	"lord of the rings"     words together, in this order
//	-abridged               leaves out books that match
//	author:pratchett        one field: title, author, series, tag, publisher,
//	                        lang (a code or name: "eng", "en", "english"),
//	                        format ("epub"), isbn
//	978-0-261-10221-7       an ISBN (10 or 13 digits, with or without dashes)
//	rating:4                Calibre's rating: 4 stars or more; rating:=4.5
//	                        exactly 4½, rating:0 unrated
//
// Text is compared without case, accents or punctuation (textkey.Fold), and a
// word matches the start of a word ("tolk" finds Tolkien); words of four
// letters or more also match inside a word ("hobbit" finds "Hobbits'").
// Books are ranked by where they matched (title, then author, series, tag,
// publisher) and how well (the whole name, its start, a word's start, inside
// a word); equal books are listed series by series, in reading order, and
// otherwise by title.
//
// Placeholders (books not in the library yet, placeholders.go) that match
// are listed too, after every library book whatever the order: see
// search_placeholders.go.

import (
	"cmp"
	"context"
	"math"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/langcodes"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

const (
	searchMaxQuery = 200 // runes; longer queries are cut
	searchMaxTerms = 12  // terms after that are ignored
)

type searchField uint8

const (
	fieldAny searchField = iota
	fieldTitle
	fieldAuthor
	fieldSeries
	fieldTag
	fieldPublisher
	fieldLang
	fieldFormat
	fieldISBN
	fieldRating
)

var searchPrefixes = map[string]searchField{
	"title": fieldTitle, "author": fieldAuthor, "authors": fieldAuthor, "by": fieldAuthor,
	"series": fieldSeries, "tag": fieldTag, "tags": fieldTag, "publisher": fieldPublisher,
	"lang": fieldLang, "language": fieldLang, "format": fieldFormat, "isbn": fieldISBN,
	"rating": fieldRating, "stars": fieldRating,
}

type searchTerm struct {
	field  searchField
	phrase string // folded words, space separated ("lord of the rings")
	raw    string // as typed, without the prefix and quotes (lang, format, isbn)
	isbn   string // the ISBN-13 the term is, if it is one
	neg    bool
}

// parseSearch splits a query into terms. Terms that fold to nothing (only
// punctuation) are dropped, unless they're an ISBN.
func parseSearch(q string) []searchTerm {
	if utf8.RuneCountInString(q) > searchMaxQuery {
		q = string([]rune(q)[:searchMaxQuery])
	}
	var terms []searchTerm
	rest := strings.TrimSpace(q)
	for rest != "" && len(terms) < searchMaxTerms {
		var t searchTerm
		if rest[0] == '-' && len(rest) > 1 && rest[1] != ' ' {
			t.neg = true
			rest = rest[1:]
		}
		// field:
		if i := strings.IndexByte(rest, ':'); i > 0 && !strings.ContainsAny(rest[:i], " \t\"") {
			if f, ok := searchPrefixes[strings.ToLower(rest[:i])]; ok {
				t.field = f
				rest = rest[i+1:]
			}
		}
		var tok string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				tok, rest = rest[1:], ""
			} else {
				tok, rest = rest[1:1+end], rest[2+end:]
			}
		} else {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				tok, rest = rest, ""
			} else {
				tok, rest = rest[:end], rest[end:]
			}
		}
		rest = strings.TrimSpace(rest)
		t.raw = strings.TrimSpace(tok)
		t.phrase = textkey.Fold(t.raw)
		if t.field == fieldAny || t.field == fieldISBN {
			t.isbn = isbn.Normalize(t.raw)
		}
		switch {
		case t.field == fieldISBN && searchDigits(t.raw) == "":
			continue
		case t.field != fieldISBN && t.phrase == "" && t.isbn == "":
			continue
		}
		terms = append(terms, t)
	}
	return terms
}

// searchDigits keeps an ISBN's digits and X.
func searchDigits(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case r == 'x' || r == 'X':
			return 'X'
		}
		return -1
	}, s)
}

// normalizeISBN is how the catalog keeps a book's ISBN: the ISBN-13, or, for
// one with a wrong check digit, its digits as written.
func normalizeISBN(v string) string {
	if n := isbn.Normalize(v); n != "" {
		return n
	}
	if d := searchDigits(v); len(d) >= 10 {
		return d
	}
	return ""
}

// ---------------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------------

// Match quality, best last.
const (
	matchNone   = 0
	matchInside = 1 // inside a word
	matchWord   = 2 // at the start of a word
	matchStart  = 3 // at the start of the name
	matchWhole  = 4 // the whole name
)

// Weight of each field: a title match ranks above an author match, and so on.
var fieldWeight = [...]int{fieldTitle: 5, fieldAuthor: 4, fieldSeries: 3, fieldTag: 2, fieldPublisher: 1}

// searchHay is a folded name ready for matching: "\x1f lord of the rings \x1f".
// Several names (a book's tags) are kept as "\x1f a b \x1f c \x1f", so a
// phrase never runs from one into the next.
func searchHay(names ...string) string {
	var b strings.Builder
	b.WriteByte('\x1f')
	for _, n := range names {
		if f := textkey.Fold(n); f != "" {
			b.WriteString(" " + f + " \x1f")
		}
	}
	return b.String()
}

// matchHay is how well phrase matches hay.
func matchHay(hay, phrase string) int {
	if !strings.Contains(hay, phrase) {
		return matchNone // most names: one scan, not four
	}
	switch {
	case strings.Contains(hay, "\x1f "+phrase+" \x1f"):
		return matchWhole
	case strings.Contains(hay, "\x1f "+phrase):
		return matchStart
	case strings.Contains(hay, " "+phrase):
		return matchWord
	case len(phrase) >= 4:
		return matchInside
	}
	return matchNone
}

// searchIndex: the catalog's text, folded, made once per catalog.
type searchIndex struct {
	cat        *catalog
	titles     []string // by slot
	order      []string // by slot: series name, or else title, folded (ties)
	sorts      []string // by slot: books.sort, folded (ties within a series)
	authors    map[int32]string
	series     map[int32]string
	tags       map[int32]string
	publishers map[int32]string
}

func newSearchIndex(c *catalog) *searchIndex {
	ix := &searchIndex{
		cat:        c,
		titles:     make([]string, len(c.books)),
		order:      make([]string, len(c.books)),
		sorts:      make([]string, len(c.books)),
		authors:    hayMap(c.authorNames),
		series:     hayMap(c.seriesNames),
		tags:       hayMap(c.tagNames),
		publishers: hayMap(c.publisherNames),
	}
	for i := range c.books {
		b := &c.books[i]
		ix.titles[i] = searchHay(b.Title)
		ix.sorts[i] = textkey.Fold(b.Sort)
		ix.order[i] = ix.sorts[i]
		if name := c.seriesNames[b.Series]; b.Series != 0 && name != "" {
			ix.order[i] = textkey.Fold(name)
		}
	}
	return ix
}

func hayMap(names map[int32]string) map[int32]string {
	m := make(map[int32]string, len(names))
	for id, n := range names {
		m[id] = searchHay(n)
	}
	return m
}

// nameMatches is how well each name matches phrase (only those that do).
func nameMatches(hays map[int32]string, phrase string) map[int32]int {
	m := map[int32]int{}
	for id, hay := range hays {
		if q := matchHay(hay, phrase); q != matchNone {
			m[id] = q
		}
	}
	return m
}

// termMatcher scores one term against a book: 0 if it doesn't match.
type termMatcher struct {
	searchTerm
	authors, series, tags, publishers map[int32]int
	langs                             map[int32]bool
}

func (ix *searchIndex) matcher(t searchTerm) *termMatcher {
	m := &termMatcher{searchTerm: t}
	if t.phrase != "" {
		if t.field == fieldAny || t.field == fieldAuthor {
			m.authors = nameMatches(ix.authors, t.phrase)
		}
		if t.field == fieldAny || t.field == fieldSeries {
			m.series = nameMatches(ix.series, t.phrase)
		}
		if t.field == fieldAny || t.field == fieldTag {
			m.tags = nameMatches(ix.tags, t.phrase)
		}
		if t.field == fieldAny || t.field == fieldPublisher {
			m.publishers = nameMatches(ix.publishers, t.phrase)
		}
	}
	if t.field == fieldLang {
		m.langs = map[int32]bool{}
		for id, code := range ix.cat.langCodes {
			if t.langMatches(code) {
				m.langs[id] = true
			}
		}
	}
	return m
}

// langMatches: a lang: term names language code ("eng", "en", "english").
func (t *searchTerm) langMatches(code string) bool {
	return strings.EqualFold(code, t.raw) || langcodes.Canonical(code) == langcodes.Canonical(t.raw) ||
		matchHay(searchHay(languageLabel(code)), t.phrase) >= matchStart
}

func bestOf(ids []int32, m map[int32]int) int {
	best := matchNone
	for _, id := range ids {
		best = max(best, m[id])
	}
	return best
}

// score: how well book slot i matches the term, 0 if it doesn't.
func (m *termMatcher) score(ix *searchIndex, i int) int {
	b := &ix.cat.books[i]
	switch m.field {
	case fieldLang:
		for _, id := range b.LangIDs {
			if m.langs[id] {
				return 1
			}
		}
		return 0
	case fieldFormat:
		if slices.Contains(b.Formats, strings.ToLower(strings.TrimPrefix(m.raw, "."))) {
			return 1
		}
		return 0
	case fieldISBN:
		return isbnScore(b.ISBNs, m.isbn, searchDigits(m.raw))
	case fieldRating:
		if ratingMatches(int(b.Rating), m.raw) {
			return 1
		}
		return 0
	}
	best := 0
	if m.field == fieldAny && m.isbn != "" && slices.Contains(b.ISBNs, m.isbn) {
		best = fieldWeight[fieldTitle] * (matchWhole + 1) // an ISBN beats everything
	}
	if m.phrase == "" {
		return best
	}
	if m.field == fieldAny || m.field == fieldTitle {
		best = max(best, fieldWeight[fieldTitle]*matchHay(ix.titles[i], m.phrase))
	}
	if m.authors != nil {
		best = max(best, fieldWeight[fieldAuthor]*bestOf(b.AuthorIDs, m.authors))
	}
	if m.series != nil && b.Series != 0 {
		best = max(best, fieldWeight[fieldSeries]*m.series[b.Series])
	}
	if m.tags != nil {
		best = max(best, fieldWeight[fieldTag]*bestOf(b.TagIDs, m.tags))
	}
	if m.publishers != nil && b.Publisher != 0 {
		best = max(best, fieldWeight[fieldPublisher]*m.publishers[b.Publisher])
	}
	return best
}

// ratingMatches: rating:4 is 4 stars or more, rating:=4.5 exactly 4½,
// rating:0 unrated. rating (half-stars) is the book's, 0 for none.
func ratingMatches(rating int, q string) bool {
	exact := strings.HasPrefix(q, "=")
	q = strings.TrimSuffix(strings.TrimLeft(q, "=>"), "*")
	stars, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
	if err != nil || stars < 0 || stars > 5 {
		return false
	}
	want := int(math.Round(stars * 2))
	switch {
	case want == 0:
		return rating == 0
	case exact:
		return rating == want
	}
	return rating >= want
}

// isbnScore: an exact ISBN, or for a partial one, any ISBN containing its digits.
func isbnScore(have []string, want13, digits string) int {
	for _, h := range have {
		if (want13 != "" && h == want13) || (want13 == "" && len(digits) >= 4 && strings.Contains(h, digits)) {
			return 1
		}
	}
	return 0
}

// searchResult is a query's books, best first, as catalog slots.
type searchResult struct {
	ix    *searchIndex
	terms []searchTerm
	slots []int32

	mu     sync.Mutex
	sorted map[string][]int32 // slots in another order (catalog_sort.go), made once

	// Matching placeholders, best first, and the list they're from
	// (placeholderCache): placeholders change apart from the catalog.
	phs     []*placeholder
	phsFrom *[]*placeholder
}

// inOrder is the results sorted by key: by relevance (slots) by default.
func (r *searchResult) inOrder(key string) []int32 {
	if key == sortByBest {
		return r.slots
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if o, ok := r.sorted[key]; ok {
		return o
	}
	o := slices.Clone(r.slots)
	sortSlots(r.ix.cat, o, key)
	if r.sorted == nil {
		r.sorted = map[string][]int32{}
	}
	r.sorted[key] = o
	return o
}

func (ix *searchIndex) search(terms []searchTerm) *searchResult {
	res := &searchResult{ix: ix, terms: terms}
	var pos, neg []*termMatcher
	for _, t := range terms {
		if t.neg {
			neg = append(neg, ix.matcher(t))
		} else {
			pos = append(pos, ix.matcher(t))
		}
	}
	if len(pos) == 0 {
		return res // only exclusions: nothing to look for
	}
	scores := make([]int, len(ix.cat.books))
books:
	for i := range ix.cat.books {
		if ix.cat.books[i].Removed {
			continue
		}
		total := 0
		for _, m := range pos {
			s := m.score(ix, i)
			if s == 0 {
				continue books
			}
			total += s
		}
		for _, m := range neg {
			if m.score(ix, i) != 0 {
				continue books
			}
		}
		res.slots = append(res.slots, int32(i))
		scores[i] = total
	}
	books := ix.cat.books
	slices.SortFunc(res.slots, func(a, b int32) int {
		if n := cmp.Compare(scores[b], scores[a]); n != 0 {
			return n
		}
		if n := strings.Compare(ix.order[a], ix.order[b]); n != 0 {
			return n
		}
		if n := cmp.Compare(books[a].SeriesIndex, books[b].SeriesIndex); n != 0 {
			return n
		}
		if n := strings.Compare(ix.sorts[a], ix.sorts[b]); n != 0 {
			return n
		}
		return cmp.Compare(a, b)
	})
	return res
}

// ---------------------------------------------------------------------------
// Matching authors, series and tags: links above the results
// ---------------------------------------------------------------------------

type searchLink struct {
	Name  string
	Href  string
	Count int // books found that have it
}

type searchLinkGroup struct {
	Kind  string // "author", "series" or "tag"
	Label string
	Links []searchLink
	More  int // matches past searchLinksMax, not listed
}

// Shown at first: searchLinksShown of each kind; the rest, up to
// searchLinksMax (which bounds the page's size), behind "Show all".
const (
	searchLinksShown = 20
	searchLinksMax   = 200
)

// Shown are the links listed at first, Hidden those behind "Show all".
func (g searchLinkGroup) Shown() []searchLink  { return g.Links[:min(len(g.Links), searchLinksShown)] }
func (g searchLinkGroup) Hidden() []searchLink { return g.Links[min(len(g.Links), searchLinksShown):] }

// links are the authors, series and tags that match every word of the query
// (at a word's start or better) and are on books found, most books first.
func (r *searchResult) links() []searchLinkGroup {
	var phrases []string
	for _, t := range r.terms {
		if t.neg {
			continue
		}
		if t.field != fieldAny || t.phrase == "" {
			return nil // a field or ISBN query: the books are what's wanted
		}
		phrases = append(phrases, t.phrase)
	}
	if len(phrases) == 0 || len(r.slots) == 0 {
		return nil
	}
	type kind struct {
		key  string
		hays map[int32]string
		ids  func(b *catalogBook) []int32
	}
	kinds := []kind{
		{"author", r.ix.authors, func(b *catalogBook) []int32 { return b.AuthorIDs }},
		{"series", r.ix.series, func(b *catalogBook) []int32 {
			if b.Series == 0 {
				return nil
			}
			return []int32{b.Series}
		}},
		{"tag", r.ix.tags, func(b *catalogBook) []int32 { return b.TagIDs }},
	}
	names := map[string]map[int32]string{"author": r.ix.cat.authorNames, "series": r.ix.cat.seriesNames, "tag": r.ix.cat.tagNames}
	var groups []searchLinkGroup
	for _, k := range kinds {
		matching := map[int32]bool{}
		for id, hay := range k.hays {
			ok := true
			for _, p := range phrases {
				if matchHay(hay, p) < matchWord {
					ok = false
					break
				}
			}
			if ok {
				matching[id] = true
			}
		}
		if len(matching) == 0 {
			continue
		}
		count := map[int32]int{}
		for _, s := range r.slots {
			for _, id := range k.ids(&r.ix.cat.books[s]) {
				if matching[id] {
					count[id]++
				}
			}
		}
		ids := make([]int32, 0, len(count))
		for id := range count {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b int32) int {
			if n := cmp.Compare(count[b], count[a]); n != 0 {
				return n
			}
			return strings.Compare(names[k.key][a], names[k.key][b])
		})
		if len(ids) == 0 {
			continue
		}
		c := categoryByKey(k.key)
		g := searchLinkGroup{Kind: k.key, Label: c.Title}
		if len(ids) > searchLinksMax {
			g.More = len(ids) - searchLinksMax
			ids = ids[:searchLinksMax]
		}
		for _, id := range ids {
			g.Links = append(g.Links, searchLink{Name: names[k.key][id], Href: c.href(names[k.key][id]), Count: count[id]})
		}
		groups = append(groups, g)
	}
	return groups
}

// ---------------------------------------------------------------------------
// Caches
// ---------------------------------------------------------------------------

const searchCacheSize = 32

var searchCache struct {
	sync.Mutex
	ix       *searchIndex // the current catalog's, once built
	building *indexBuild  // being built, for the catalog it names
	results  map[string]*searchResult
	recent   []string // oldest first
}

type indexBuild struct {
	cat  *catalog
	ix   *searchIndex
	done chan struct{}
}

// searchSlots bounds how many searches are worked out at once (cached
// answers don't take one), so a burst of new queries can't take every CPU
// from the rest of the server.
var searchSlots = make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/2))

// searchIndexFor returns c's index, building it once however many searches
// ask for it at the same time.
func searchIndexFor(c *catalog) *searchIndex {
	searchCache.Lock()
	if ix := searchCache.ix; ix != nil && ix.cat == c {
		searchCache.Unlock()
		return ix
	}
	if b := searchCache.building; b != nil && b.cat == c {
		searchCache.Unlock()
		<-b.done
		return b.ix
	}
	b := &indexBuild{cat: c, done: make(chan struct{})}
	searchCache.building = b
	searchCache.Unlock()

	defer close(b.done)
	b.ix = newSearchIndex(c)
	searchCache.Lock()
	defer searchCache.Unlock()
	if searchCache.building == b {
		searchCache.building = nil
	}
	if currentCatalog() == c {
		searchCache.ix = b.ix
		searchCache.results = map[string]*searchResult{}
		searchCache.recent = nil
	}
	return b.ix
}

// runSearch returns q's results in the current catalog, from the cache when
// the same query was just run (the search page, then its grid's pages). It
// fails only if ctx ends while waiting for a slot.
func runSearch(ctx context.Context, q string) (*searchResult, error) {
	c := currentCatalog()
	terms := parseSearch(q)
	key := searchKey(terms)

	searchCache.Lock()
	if ix := searchCache.ix; ix != nil && ix.cat == c {
		if r, ok := searchCache.results[key]; ok {
			searchCache.Unlock()
			return r, nil
		}
	}
	searchCache.Unlock()

	select {
	case searchSlots <- struct{}{}:
		defer func() { <-searchSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r := searchIndexFor(c).search(terms)

	searchCache.Lock()
	defer searchCache.Unlock()
	if ix := searchCache.ix; ix == nil || ix.cat != c {
		return r, nil // the catalog changed meanwhile: don't keep stale results
	}
	if _, ok := searchCache.results[key]; !ok {
		searchCache.results[key] = r
		searchCache.recent = append(searchCache.recent, key)
		if len(searchCache.recent) > searchCacheSize {
			delete(searchCache.results, searchCache.recent[0])
			searchCache.recent = searchCache.recent[1:]
		}
	}
	return r, nil
}

// searchKey is the same for queries that mean the same ("Tolkien" and "tolkien ").
func searchKey(terms []searchTerm) string {
	var b strings.Builder
	for _, t := range terms {
		if t.neg {
			b.WriteByte('-')
		}
		b.WriteString(strconv.Itoa(int(t.field)))
		b.WriteByte(':')
		if t.field == fieldLang || t.field == fieldFormat || t.field == fieldISBN || t.field == fieldRating {
			b.WriteString(strings.ToLower(t.raw))
		} else {
			b.WriteString(t.phrase)
			b.WriteByte('|')
			b.WriteString(t.isbn)
		}
		b.WriteByte('\x1f')
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// searchPageHandler: GET /search?q=…, the page; its grid asks /api/search.
func searchPageHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := SearchPageData{Title: SiteTitle, Query: q, Sort: newGridSort("search", pickSort(r, searchSorts), searchSorts)}
	if q != "" {
		res, err := runSearch(r.Context(), q)
		if err != nil {
			return // the visitor left
		}
		data.TotalBooks = len(res.slots) + len(res.placeholders())
		data.Links = res.links()
	}
	if err := templates.ExecuteTemplate(w, "search.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// searchHandler: GET /api/search?q=…&page=N, a page of results for the grid.
func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)

	res, err := runSearch(r.Context(), q)
	if err != nil {
		http.Error(w, "search cancelled", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, res.page(pickSort(r, searchSorts), page))
}

// The top bar's suggestions while typing (web/search-suggest.ts): the best
// few books, and the authors, series and tags that match.
const (
	suggestBooks = 8
	suggestLinks = 6
)

type suggestBook struct {
	UUID        string  `json:"uuid"`
	Title       string  `json:"title"`
	Authors     string  `json:"authors"` // "A & B", names in comma form (bookAuthorsSQL)
	Series      string  `json:"series"`
	SeriesIndex float64 `json:"series_index"`
}

type suggestLink struct {
	Kind  string `json:"kind"` // author, series or tag
	Name  string `json:"name"`
	Href  string `json:"href"`
	Count int    `json:"count"` // books found that have it
}

type suggestResponse struct {
	Books []suggestBook `json:"books"`
	Links []suggestLink `json:"links"`
}

// suggestLinksOf picks up to limit links, each kind's best in turn so that
// one kind (a common tag) doesn't push out the others; kinds keep their order.
func suggestLinksOf(groups []searchLinkGroup, limit int) []suggestLink {
	take := make([]int, len(groups))
	for n, more := 0, true; n < limit && more; {
		more = false
		for i, g := range groups {
			if take[i] < len(g.Links) && n < limit {
				take[i]++
				n++
				more = true
			}
		}
	}
	out := []suggestLink{}
	for i, g := range groups {
		for _, l := range g.Links[:take[i]] {
			out = append(out, suggestLink{Kind: g.Kind, Name: l.Name, Href: l.Href, Count: l.Count})
		}
	}
	return out
}

// searchSuggestHandler: GET /api/search/suggest?q=…, what the top bar's
// search box offers while typing: the search /search runs, cut short.
func searchSuggestHandler(w http.ResponseWriter, r *http.Request) {
	out := suggestResponse{Books: []suggestBook{}, Links: []suggestLink{}}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	res, err := runSearch(r.Context(), q)
	if err != nil {
		http.Error(w, "search cancelled", http.StatusServiceUnavailable)
		return
	}
	cat := res.ix.cat
	for _, s := range res.slots[:min(len(res.slots), suggestBooks)] {
		b := &cat.books[s]
		out.Books = append(out.Books, suggestBook{
			UUID:        b.UUID,
			Title:       b.Title,
			Authors:     b.Authors,
			Series:      cat.seriesNames[b.Series],
			SeriesIndex: b.SeriesIndex,
		})
	}
	out.Links = suggestLinksOf(res.links(), suggestLinks)
	writeJSON(w, http.StatusOK, out)
}
