package main

// Merging a placeholder (a book not in the library yet, placeholders.go)
// with the library book(s) that are it: an editor selects the placeholder
// and one or more books in a grid (web/select-books.ts) and Merge opens
// /book/merge?ids=a,b&placeholder=p. The books are merged as any merge is
// (merge.go; a single book is only edited), and the placeholder is then
// replaced by the kept book, as an add or an edit replaces one
// (removePlaceholder): readers who saved it have the book saved instead,
// its Hardcover sharing moves to the book, and its page leads there. The
// editor starts from the kept book's metadata with the placeholder's series,
// number and Hardcover identifiers (mergedPlaceholderMeta), and offers the
// placeholder's metadata as a source of its own ("Placeholder").

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
)

// whyMerged: a placeholder an editor merged with library books.
const whyMerged = "merged with a library book"

// errNoSuchPlaceholder: a merge named a placeholder that isn't there
// (removed meanwhile, or never was).
var errNoSuchPlaceholder = errors.New("no such placeholder")

// mergePlaceholderView is the placeholder on the merge page.
type mergePlaceholderView struct {
	UUID    string
	Title   string
	Authors string
	Series  string // "Dune #2", "" for none
	Href    string
	Cover   string // its picture's URL, "" for none
}

func newMergePlaceholderView(p *placeholder) *mergePlaceholderView {
	v := &mergePlaceholderView{UUID: p.UUID, Title: p.Meta.Title, Authors: p.authors(), Href: p.href(), Cover: p.coverURL()}
	if s := strings.TrimSpace(p.Meta.Series); s != "" {
		v.Series = s + " #" + strings.TrimSpace(p.Meta.SeriesIndex)
		if strings.TrimSpace(p.Meta.SeriesIndex) == "" {
			v.Series = s
		}
	}
	return v
}

// mergePlaceholder finds the placeholder a merge names ("": none).
func mergePlaceholder(id string) (*placeholder, error) {
	if id == "" {
		return nil, nil
	}
	if p := placeholderByUUID(id); p != nil {
		return p, nil
	}
	return nil, errNoSuchPlaceholder
}

// mergedPlaceholderMeta: the kept book's metadata m as the merge with
// placeholder p starts it: the placeholder's series and number and its
// Hardcover identifiers (what it was made for), its ISBN where the book has
// none, and its description, publisher and date where the book's are empty.
// Title and authors stay the book's.
func mergedPlaceholderMeta(m OPFMetadata, p *placeholder) OPFMetadata {
	pm := p.Meta
	m.Identifiers = slices.Clone(m.Identifiers)
	if strings.TrimSpace(pm.Series) != "" {
		m.Series, m.SeriesIndex = pm.Series, pm.SeriesIndex
		m.SyncSeriesMeta()
	}
	if ids := hcIdentifiers(pm); len(ids) > 0 {
		m.Identifiers = withIdentifiers(m.Identifiers, ids)
	}
	if isbn := placeholderISBN(pm); isbn != "" && placeholderISBN(m) == "" {
		m.Identifiers = append(m.Identifiers, opfIdentifier{Scheme: "ISBN", Value: isbn})
	}
	if strings.TrimSpace(m.Description) == "" {
		m.Description = pm.Description
	}
	if strings.TrimSpace(m.Publisher) == "" {
		m.Publisher = pm.Publisher
	}
	if strings.TrimSpace(m.Date) == "" {
		m.Date = pm.Date
	}
	return m
}

// resolveMergedPlaceholder: library book bookUUID, just merged with p,
// replaces it. One already gone (an edit's match got there first) is fine.
func resolveMergedPlaceholder(p *placeholder, bookUUID string, by *User) error {
	if err := replacePlaceholderLogged(p, bookUUID, whyMerged, by); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}
