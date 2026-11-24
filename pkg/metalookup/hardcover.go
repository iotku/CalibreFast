package metalookup

// Hardcover as a source: a pkg/hardcover client given in Config.Hardcover.
// That client keeps its own request budget (which holds work rather than
// refusing it) and its own cache of answers and the covers they named, so
// Hardcover's answers aren't cached again here, and it has no Limiter.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/langcodes"
	"github.com/iotku/CalibreFast/pkg/opf"
)

// errNoHardcover: Lookup or FetchCover was asked for Hardcover without a
// client in Config.Hardcover.
var errNoHardcover = errors.New("Hardcover isn't configured")

// hardcoverLookup asks Hardcover for q: two requests either way.
func (c *Client) hardcoverLookup(ctx context.Context, q Query) ([]Match, error) {
	if c.hardcover == nil {
		return nil, errNoHardcover
	}
	books, err := c.hardcover.LookupBooks(ctx, hardcover.BookQuery{ISBN: q.ISBN, Title: q.Title, Author: q.Author})
	var wait *hardcover.WaitError
	switch {
	case errors.As(err, &wait):
		return nil, fmt.Errorf("Hardcover's request limit is used up for now; try again after %s", wait.Until.UTC().Format("15:04 UTC"))
	case err != nil:
		return nil, err
	}
	var out []Match
	for i := range books {
		m := Match{Meta: hardcoverMeta(&books[i])}
		if books[i].HasCover() {
			m.CoverID = books[i].ID
		}
		out = append(out, m)
	}
	return out, nil
}

// hardcoverMeta is a Hardcover book as metadata. Everything in it is
// someone else's data: show it as text, and sanitize the description like
// any other.
func hardcoverMeta(b *hardcover.Book) *opf.Metadata {
	m := &opf.Metadata{
		Title:       b.Title,
		Creators:    b.Authors,
		Publisher:   b.Publisher,
		Description: textToHTML(b.Description),
		Subjects:    opf.UniqueTags(b.Genres),
	}
	if b.Subtitle != "" {
		m.Title += ": " + b.Subtitle
	}
	if t, err := opf.ParseAnyDate(b.Released); err == nil {
		m.Date = t.Format("2006-01-02")
	}
	for _, l := range b.Languages {
		if c := langcodes.Canonical(l); c != "" {
			m.Languages = append(m.Languages, c)
		}
	}
	index := ""
	if b.Position != nil && *b.Position >= 0 {
		index = strconv.FormatFloat(*b.Position, 'f', -1, 64)
	}
	if name, n, err := opf.CleanSeries(b.Series, index); err == nil && name != "" {
		m.Series, m.SeriesIndex = name, strconv.FormatFloat(n, 'f', -1, 64)
	}
	if b.ISBN13 != "" {
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: "isbn", Value: b.ISBN13})
	}
	if b.Slug != "" { // as Calibre's Hardcover plugin names it
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: hardcover.SlugScheme, Value: b.Slug})
	}
	// Its book id too: a series' list may show another Hardcover book at
	// the same position, keeping only this one's id (Entry.AltIDs).
	if b.ID > 0 {
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: hardcover.IDScheme, Value: strconv.FormatInt(b.ID, 10)})
	}
	return m
}

// hardcoverCover fetches the cover of a book a Hardcover lookup found, by
// its book id: the image's URL is the one Hardcover gave.
func (c *Client) hardcoverCover(ctx context.Context, id string) ([]byte, string, error) {
	if c.hardcover == nil {
		return nil, "", ErrBadCoverID
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return nil, "", ErrBadCoverID
	}
	img, ctype, err := c.hardcover.FetchCover(ctx, n)
	switch {
	case errors.Is(err, hardcover.ErrNoCover):
		return nil, "", ErrNoCover
	case err != nil:
		return nil, "", ErrCoverUnreadable
	}
	return img, ctype, nil
}
