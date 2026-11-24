package hardcoverseries

import (
	"context"
	"slices"
	"strings"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

// findCandidates: Hardcover series that may be this one, best first,
// grouped by name: Hardcover splits some series into one per author, all of
// the same name and numbered as one, so a library series may be several
// Hardcover series.
//
// They come from a search by name, from the series the library's ISBNs
// are in, and from every series named like the likeliest ones (which finds
// the parts a search's ten results leave out). A candidate has evidence
// when the library's ISBNs are in it, its author wrote the series' books
// here, or the search lists one of their titles. The group of the same
// name as the library's series with evidence, else the group holding the
// most library ISBNs, is ticked (Pick). A batch links it on its own (Sure)
// only when its name is the library's, or when it holds at least 3 of the
// library's ISBNs and more than twice as many as any other group.
func (s *Service) findCandidates(ctx context.Context, si SeriesInfo) ([]Candidate, error) {
	isbns, err := s.host.SeriesISBNs(si)
	if err != nil {
		return nil, err
	}
	byISBN, err := s.client().SeriesByISBNs(ctx, isbns)
	if err != nil {
		return nil, err
	}
	var search []hardcover.Series
	for _, name := range slices.Compact(append([]string{si.Name}, si.Members...)) {
		got, err := s.client().SearchSeries(ctx, name)
		if err != nil {
			return nil, err
		}
		search = append(search, got...)
		if len(search) >= 10 {
			break
		}
	}
	names := map[string]bool{}
	for _, m := range append([]string{si.Name}, si.Members...) {
		names[textkey.Series(m)] = true
	}

	var out []Candidate
	index := map[int64]int{}
	add := func(c Candidate) {
		if i, ok := index[c.ID]; ok {
			o := &out[i]
			o.Hits = max(o.Hits, c.Hits)
			if o.Author == "" {
				o.Author = c.Author
			}
			if len(o.Books) == 0 {
				o.Books = c.Books
			}
			o.BooksCount = max(o.BooksCount, c.BooksCount)
			return
		}
		index[c.ID] = len(out)
		out = append(out, c)
	}
	for _, c := range byISBN {
		add(Candidate{Series: c.Series, Hits: c.Hits})
	}
	for _, c := range search {
		add(Candidate{Series: c})
	}

	titles := map[string]bool{}
	for _, t := range s.host.SeriesTitles(si) {
		titles[textkey.FullTitle(t)] = true
	}
	authors := s.host.SeriesAuthors(si)

	// Every series named like the library's, or like one with evidence.
	var lookup []string
	for _, c := range out {
		if (names[textkey.Series(c.Name)] || evidence(c, authors, titles) != "") && !slices.Contains(lookup, c.Name) && len(lookup) < 5 {
			lookup = append(lookup, c.Name)
		}
	}
	if len(lookup) > 0 {
		same, err := s.client().SeriesByName(ctx, lookup)
		if err != nil {
			return nil, err
		}
		for _, c := range same {
			add(Candidate{Series: c})
		}
	}

	return rankCandidates(out, names, authors, titles), nil
}

// evidence: what ties c to the library's series: its ISBNs ("isbn"), its
// author ("author"), or one of its books' titles ("title"); "" for nothing.
func evidence(c Candidate, authors []string, titles map[string]bool) string {
	switch {
	case c.Hits > 0:
		return "isbn"
	case c.Author != "" && textkey.SharesName([]string{c.Author}, authors):
		return "author"
	case slices.ContainsFunc(c.Books, func(t string) bool { return titles[textkey.FullTitle(t)] }):
		return "title"
	}
	return ""
}

// rankCandidates marks out's evidence, groups, Pick and Sure, and sorts
// them (see findCandidates). names and titles are keys (textkey.Series,
// textkey.FullTitle) of the library series' names and its books' titles.
func rankCandidates(out []Candidate, names map[string]bool, authors []string, titles map[string]bool) []Candidate {
	type group struct {
		hits     int
		evidence bool
		score    int  // members with evidence
		named    bool // the library's name
	}
	groups := map[string]*group{}
	for i := range out {
		c := &out[i]
		c.URL = (&Link{Slug: c.Slug}).URL()
		c.Group = textkey.Series(c.Name)
		c.Evidence = evidence(*c, authors, titles)
		g := groups[c.Group]
		if g == nil {
			g = &group{named: names[c.Group]}
			groups[c.Group] = g
		}
		g.hits += c.Hits
		if c.Evidence != "" {
			g.evidence = true
			g.score++
		}
	}
	pick, sure := "", false
	for k, g := range groups {
		if g.named && g.evidence {
			pick, sure = k, true
		}
	}
	if pick == "" {
		// Else the group with the most library ISBNs, then the most members
		// with evidence (an author or a title). Only ISBNs make it sure.
		rank := func(g *group) int { return g.hits*1000 + g.score }
		best, second := "", 0
		for k, g := range groups {
			if best == "" || rank(g) > rank(groups[best]) || (rank(g) == rank(groups[best]) && k < best) {
				if best != "" {
					second = max(second, groups[best].hits)
				}
				best = k
			} else {
				second = max(second, g.hits)
			}
		}
		if best != "" && groups[best].evidence {
			pick = best
			sure = groups[best].hits >= 3 && groups[best].hits > 2*second
		}
	}
	for i := range out {
		out[i].Pick = pick != "" && out[i].Group == pick
		out[i].Sure = out[i].Pick && sure
	}
	// The picked group first, then by ISBNs found and size.
	slices.SortStableFunc(out, func(a, b Candidate) int {
		if a.Pick != b.Pick {
			return boolInt(b.Pick) - boolInt(a.Pick)
		}
		if ga, gb := groups[a.Group].hits, groups[b.Group].hits; ga != gb {
			return gb - ga
		}
		if a.Group != b.Group {
			return strings.Compare(a.Group, b.Group)
		}
		if a.Hits != b.Hits {
			return b.Hits - a.Hits
		}
		return b.BooksCount - a.BooksCount
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
