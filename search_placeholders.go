package main

// Placeholders in search results: those that match the query, matched as a
// library book is (search.go) on what they have (title, authors, series,
// tags, publisher, languages, ISBNs, rating; never a format), are listed
// after all of the library's books, in every order.

import (
	"cmp"
	"slices"
	"strings"

	"github.com/iotku/CalibreFast/pkg/textkey"
)

// placeholderHays: a placeholder's names, folded for matching.
type placeholderHays struct {
	p                                       *placeholder
	title, authors, series, tags, publisher string
	isbns                                   []string
	order, sort                             string // as searchIndex's, for ties
}

func newPlaceholderHays(p *placeholder) *placeholderHays {
	h := &placeholderHays{
		p:         p,
		title:     searchHay(p.Meta.Title),
		authors:   searchHay(p.Meta.Creators...),
		tags:      searchHay(p.Meta.Subjects...),
		publisher: searchHay(p.Meta.Publisher),
		sort:      textkey.Fold(titleSort(p.Meta.Title)),
	}
	h.order = h.sort
	if p.Meta.Series != "" {
		h.series = searchHay(p.Meta.Series)
		h.order = textkey.Fold(p.Meta.Series)
	}
	for _, id := range p.Meta.Identifiers {
		if strings.EqualFold(id.Scheme, "isbn") {
			if n := normalizeISBN(id.Value); n != "" {
				h.isbns = append(h.isbns, n)
			}
		}
	}
	return h
}

// score: how well the placeholder matches t, 0 if it doesn't; as
// termMatcher.score for a library book.
func (h *placeholderHays) score(t *searchTerm) int {
	switch t.field {
	case fieldLang:
		if slices.ContainsFunc(h.p.Meta.Languages, t.langMatches) {
			return 1
		}
		return 0
	case fieldFormat:
		return 0 // no files
	case fieldISBN:
		return isbnScore(h.isbns, t.isbn, searchDigits(t.raw))
	case fieldRating:
		if ratingMatches(h.p.Meta.Rating, t.raw) {
			return 1
		}
		return 0
	}
	best := 0
	if t.field == fieldAny && t.isbn != "" && slices.Contains(h.isbns, t.isbn) {
		best = fieldWeight[fieldTitle] * (matchWhole + 1)
	}
	if t.phrase == "" {
		return best
	}
	for _, f := range []struct {
		field searchField
		hay   string
	}{
		{fieldTitle, h.title}, {fieldAuthor, h.authors}, {fieldSeries, h.series},
		{fieldTag, h.tags}, {fieldPublisher, h.publisher},
	} {
		if (t.field == fieldAny || t.field == f.field) && f.hay != "" {
			best = max(best, fieldWeight[f.field]*matchHay(f.hay, t.phrase))
		}
	}
	return best
}

// matchPlaceholders: those of all that match every term (and none of the
// excluded ones), best first, as search orders library books.
func matchPlaceholders(terms []searchTerm, all []*placeholder) []*placeholder {
	if !slices.ContainsFunc(terms, func(t searchTerm) bool { return !t.neg }) {
		return nil
	}
	type hit struct {
		h     *placeholderHays
		score int
	}
	var hits []hit
next:
	for _, p := range all {
		h := newPlaceholderHays(p)
		total := 0
		for i := range terms {
			s := h.score(&terms[i])
			if terms[i].neg {
				if s != 0 {
					continue next
				}
				continue
			}
			if s == 0 {
				continue next
			}
			total += s
		}
		hits = append(hits, hit{h, total})
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		if n := cmp.Compare(b.score, a.score); n != 0 {
			return n
		}
		if n := strings.Compare(a.h.order, b.h.order); n != 0 {
			return n
		}
		if n := cmp.Compare(a.h.p.index(), b.h.p.index()); n != 0 {
			return n
		}
		return strings.Compare(a.h.sort, b.h.sort)
	})
	out := make([]*placeholder, len(hits))
	for i, x := range hits {
		out[i] = x.h.p
	}
	return out
}

// placeholders: the placeholders matching r's query, worked out again once
// they change.
func (r *searchResult) placeholders() []*placeholder {
	all := allPlaceholders()
	from := placeholderCache.Load()
	if from != nil {
		all = *from // the list this answer is for, even if it just changed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if from == nil || r.phsFrom != from {
		r.phs, r.phsFrom = matchPlaceholders(r.terms, all), from
	}
	return r.phs
}

// page is page `page` of the results in order key: the library's books,
// then the placeholders.
func (r *searchResult) page(key string, page int) []Book {
	slots := r.inOrder(key)
	books := pageOfSlots(r.ix.cat, slots, page)
	phs := r.placeholders()
	from, to := (page-1)*booksPerPage, page*booksPerPage
	for i := max(from, len(slots)); i < min(to, len(slots)+len(phs)); i++ {
		books = append(books, phs[i-len(slots)].book())
	}
	return books
}
