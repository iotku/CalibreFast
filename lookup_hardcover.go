package main

// Hardcover as a source in the metadata editor (lookup.go): offered, like
// Google Books, only when the admin asks for it ("Try Hardcover"), and only
// with CF_HARDCOVER_TOKEN (hardcover.go). It uses the same client and
// request budget as series on Hardcover: a lookup is two requests.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/langcodes"
	"github.com/iotku/CalibreFast/pkg/metalookup"
	"github.com/iotku/CalibreFast/pkg/opf"
)

// hardcoverLookup asks Hardcover for q, in the editor's form.
func hardcoverLookup(ctx context.Context, q metalookup.Query) lookupResult {
	out := lookupResult{Source: hardcoverName}
	books, err := hcClient.LookupBooks(ctx, hardcover.BookQuery{ISBN: q.ISBN, Title: q.Title, Author: q.Author})
	var wait *hardcover.WaitError
	switch {
	case errors.As(err, &wait):
		out.Error = fmt.Sprintf("Hardcover's request limit is used up for now; try again after %s", wait.Until.UTC().Format("15:04 UTC"))
		return out
	case err != nil:
		out.Error = err.Error()
		return out
	}
	for i := range books {
		m := hardcoverMeta(&books[i])
		lm := lookupMatch{Option: &UploadedBookMeta{OPFMetadata: *m, Format: hardcoverName}}
		if books[i].HasCover() {
			lm.Cover = fmt.Sprintf("/metadata/lookup/cover?src=hardcover&id=%d", books[i].ID)
		}
		out.Matches = append(out.Matches, lm)
	}
	return out
}

// hardcoverMeta is a Hardcover book as metadata. Everything in it is
// someone else's data: the editor shows it as text, and the description
// is sanitized like any other.
func hardcoverMeta(b *hardcover.Book) *opf.Metadata {
	m := &opf.Metadata{
		Title:       b.Title,
		Creators:    b.Authors,
		Publisher:   b.Publisher,
		Description: metalookup.TextToHTML(b.Description),
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
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: "hardcover", Value: b.Slug})
	}
	// Its book id too: a series' list may show another Hardcover book at
	// the same position, keeping only this one's id (Entry.AltIDs).
	if b.ID > 0 {
		m.Identifiers = append(m.Identifiers, opf.Identifier{Scheme: hcIdentifierScheme, Value: strconv.FormatInt(b.ID, 10)})
	}
	return m
}

// hardcoverCoverHandler: GET /metadata/lookup/cover?src=hardcover&id=…, a
// cover of a book a lookup just found. Only the book id is taken; the
// image's URL is the one Hardcover gave.
func hardcoverCoverHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "unknown cover", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	img, ctype, err := hcClient.FetchCover(ctx, id)
	switch {
	case errors.Is(err, hardcover.ErrNoCover):
		http.Error(w, "no cover", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "cover unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(img)
}
