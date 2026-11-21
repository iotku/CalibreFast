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
//
// Text is compared without case, accents or punctuation (foldWords), and a
// word matches the start of a word ("tolk" finds Tolkien); words of four
// letters or more also match inside a word ("hobbit" finds "Hobbits'").
// Books are ranked by where they matched (title, then author, series, tag,
// publisher) and how well (the whole name, its start, a word's start, inside
// a word); equal books are listed series by series, in reading order, and
// otherwise by title.

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/langcodes"
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
)

var searchPrefixes = map[string]searchField{
	"title": fieldTitle, "author": fieldAuthor, "authors": fieldAuthor, "by": fieldAuthor,
	"series": fieldSeries, "tag": fieldTag, "tags": fieldTag, "publisher": fieldPublisher,
	"lang": fieldLang, "language": fieldLang, "format": fieldFormat, "isbn": fieldISBN,
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
		t.phrase = foldWords(t.raw)
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
		if f := foldWords(n); f != "" {
			b.WriteString(" " + f + " \x1f")
		}
	}
	return b.String()
}

// matchHay is how well phrase matches hay.
func matchHay(hay, phrase string) int {
	switch {
	case strings.Contains(hay, "\x1f "+phrase+" \x1f"):
		return matchWhole
	case strings.Contains(hay, "\x1f "+phrase):
		return matchStart
	case strings.Contains(hay, " "+phrase):
		return matchWord
	case len(phrase) >= 4 && strings.Contains(hay, phrase):
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
		ix.sorts[i] = foldWords(b.Sort)
		ix.order[i] = ix.sorts[i]
		if name := c.seriesNames[b.Series]; b.Series != 0 && name != "" {
			ix.order[i] = foldWords(name)
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
		want := langcodes.Canonical(t.raw)
		for id, code := range ix.cat.langCodes {
			if strings.EqualFold(code, t.raw) || langcodes.Canonical(code) == want ||
				matchHay(searchHay(languageLabel(code)), t.phrase) >= matchStart {
				m.langs[id] = true
			}
		}
	}
	return m
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
	Name string
	Href string
}

type searchLinkGroup struct {
	Label string
	Links []searchLink
}

const searchLinksPerGroup = 5

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
		if len(ids) > searchLinksPerGroup {
			ids = ids[:searchLinksPerGroup]
		}
		if len(ids) == 0 {
			continue
		}
		c := categoryByKey(k.key)
		g := searchLinkGroup{Label: c.Title}
		for _, id := range ids {
			g.Links = append(g.Links, searchLink{Name: names[k.key][id], Href: c.href(names[k.key][id])})
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
	ix      *searchIndex
	results map[string]*searchResult
	recent  []string // oldest first
}

// runSearch returns q's results in the current catalog, from the cache when
// the same query was just run (the search page, then its grid's pages).
func runSearch(q string) *searchResult {
	c := currentCatalog()
	terms := parseSearch(q)
	key := searchKey(terms)

	searchCache.Lock()
	if searchCache.ix == nil || searchCache.ix.cat != c {
		searchCache.ix = nil
		searchCache.results = map[string]*searchResult{}
		searchCache.recent = nil
	}
	if r, ok := searchCache.results[key]; ok {
		searchCache.Unlock()
		return r
	}
	ix := searchCache.ix
	searchCache.Unlock()

	if ix == nil {
		ix = newSearchIndex(c)
	}
	r := ix.search(terms)

	searchCache.Lock()
	defer searchCache.Unlock()
	if searchCache.ix != nil && searchCache.ix.cat != c {
		return r // the catalog changed meanwhile: don't keep stale results
	}
	searchCache.ix = ix
	if _, ok := searchCache.results[key]; !ok {
		searchCache.results[key] = r
		searchCache.recent = append(searchCache.recent, key)
		if len(searchCache.recent) > searchCacheSize {
			delete(searchCache.results, searchCache.recent[0])
			searchCache.recent = searchCache.recent[1:]
		}
	}
	return r
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
		if t.field == fieldLang || t.field == fieldFormat || t.field == fieldISBN {
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

// searchCount counts the books matching q.
func searchCount(q string) int { return len(runSearch(q).slots) }

// searchHandler: GET /api/search?q=…&page=N, a page of results for the grid.
func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)

	res := runSearch(q)
	books := make([]Book, 0, booksPerPage)
	if from := (page - 1) * booksPerPage; from < len(res.slots) && from >= 0 {
		for _, s := range res.slots[from:min(from+booksPerPage, len(res.slots))] {
			b := res.ix.cat.books[s].Book
			uuidPathIndex.Store(b.UUID, b.Path)
			books = append(books, b)
		}
	}
	writeJSON(w, http.StatusOK, books)
}
