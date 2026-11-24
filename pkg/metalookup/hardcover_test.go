package metalookup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// fakeHardcover answers the two lookup queries for Mort (ISBN 9780552152938).
func fakeHardcover(t *testing.T) *hardcover.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "EditionByISBN"):
			_, _ = io.WriteString(w, `{"data":{"editions":[{"id":9,"book_id":42,"title":"Mort","isbn_13":"9780552152938","release_date":"1987-11-12",
				"publisher":{"name":"Corgi"},"language":{"code2":"en","code3":"eng"},"cached_image":{"url":"https://assets.hardcover.app/e/9.jpg"}}]}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"books":[{"id":42,"slug":"mort","title":"Mort","description":"Death takes <b>an</b> apprentice.",
				"cached_tags":{"Genre":[{"tag":"Fantasy"}]},"contributions":[{"author":{"name":"Terry Pratchett"}}],
				"book_series":[{"position":4,"series":{"name":"Discworld"}}]}]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return hardcover.New(hardcover.Config{URL: srv.URL, Token: "test-token", UserAgent: "test"})
}

func TestHardcoverLookup(t *testing.T) {
	ctx := context.Background()
	if res := New(Config{}).Lookup(ctx, Hardcover, Query{ISBN: "9780552152938"}); res.Error == "" {
		t.Fatalf("without a client: %+v", res)
	}

	c := New(Config{Hardcover: fakeHardcover(t)})
	res := c.Lookup(ctx, Hardcover, Query{ISBN: "9780552152938"})
	if res.Source != Hardcover || res.Error != "" || len(res.Matches) != 1 {
		t.Fatalf("result %+v", res)
	}
	m := res.Matches[0]
	o := m.Meta
	if o.Title != "Mort" || o.Publisher != "Corgi" || o.Date != "1987-11-12" ||
		strings.Join(o.Creators, ",") != "Terry Pratchett" || o.Series != "Discworld" || o.SeriesIndex != "4" ||
		strings.Join(o.Subjects, ",") != "Fantasy" || strings.Join(o.Languages, ",") != "eng" {
		t.Errorf("metadata %+v", o)
	}
	// Plain text, escaped into HTML.
	if o.Description != "<p>Death takes &lt;b&gt;an&lt;/b&gt; apprentice.</p>" {
		t.Errorf("description %q", o.Description)
	}
	ids := map[string]string{}
	for _, id := range o.Identifiers {
		ids[id.Scheme] = id.Value
	}
	if ids["isbn"] != "9780552152938" || ids[hardcover.SlugScheme] != "mort" || ids[hardcover.IDScheme] != "42" {
		t.Errorf("identifiers %v", o.Identifiers)
	}
	if m.CoverID != 42 {
		t.Errorf("cover id %d", m.CoverID)
	}
	if _, ok := c.Usage(Hardcover, time.Now()); ok {
		t.Error("Hardcover has no Limiter")
	}

	// Only a book id is taken, and only one a lookup named.
	for _, id := range []string{"x", "-1", "0", "https://evil.example/"} {
		if _, _, err := c.FetchCover(ctx, Hardcover, id); !errors.Is(err, ErrBadCoverID) {
			t.Errorf("FetchCover(%q): %v", id, err)
		}
	}
	if _, _, err := c.FetchCover(ctx, Hardcover, "7"); !errors.Is(err, ErrNoCover) {
		t.Errorf("FetchCover(7): %v", err)
	}
	if _, _, err := c.FetchCover(ctx, "Elsewhere", "42"); !errors.Is(err, ErrBadCoverID) {
		t.Errorf("unknown source: %v", err)
	}
}
