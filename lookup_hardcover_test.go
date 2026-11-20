package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

func TestHardcoverLookup(t *testing.T) {
	origClient, origNo := hcClient, noHardcover
	t.Cleanup(func() { hcClient, noHardcover = origClient, origNo })
	h := requireLookups(lookupHandler)
	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/metadata/lookup?"+query, nil))
		return rec
	}

	hcClient = nil // no CF_HARDCOVER_TOKEN
	if rec := get("source=hardcover&isbn=9780552152938"); rec.Code != http.StatusForbidden {
		t.Fatalf("without a token: %d", rec.Code)
	}

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
	hcClient = hardcover.New(hardcover.Config{URL: srv.URL, Token: "test-token", UserAgent: "test"})

	noHardcover = true
	if rec := get("source=hardcover&isbn=9780552152938"); rec.Code != http.StatusForbidden {
		t.Fatalf("-no-hardcover: %d", rec.Code)
	}
	noHardcover = false

	rec := get("source=hardcover&isbn=9780552152938")
	if rec.Code != http.StatusOK {
		t.Fatalf("lookup: %d %s", rec.Code, rec.Body)
	}
	var res lookupResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || len(res.Matches) != 1 {
		t.Fatalf("result %s, err %v", rec.Body, err)
	}
	m := res.Matches[0]
	o := m.Option
	if res.Source != "Hardcover" || o.Format != "Hardcover" || o.Title != "Mort" || o.Publisher != "Corgi" || o.Date != "1987-11-12" ||
		strings.Join(o.Creators, ",") != "Terry Pratchett" || o.Series != "Discworld" || o.SeriesIndex != "4" ||
		strings.Join(o.Subjects, ",") != "Fantasy" || strings.Join(o.Languages, ",") != "eng" {
		t.Errorf("option %+v", o.OPFMetadata)
	}
	// Plain text, escaped into HTML.
	if o.Description != "<p>Death takes &lt;b&gt;an&lt;/b&gt; apprentice.</p>" {
		t.Errorf("description %q", o.Description)
	}
	ids := map[string]string{}
	for _, id := range o.Identifiers {
		ids[id.Scheme] = id.Value
	}
	if ids["isbn"] != "9780552152938" || ids["hardcover"] != "mort" {
		t.Errorf("identifiers %v", o.Identifiers)
	}
	if m.Cover != "/metadata/lookup/cover?src=hardcover&id=42" {
		t.Errorf("cover %q", m.Cover)
	}

	// The cover route takes only a book id.
	for _, q := range []string{"src=hardcover&id=x", "src=hardcover&id=-1", "src=hardcover&id=https://evil.example/"} {
		rec := httptest.NewRecorder()
		lookupCoverHandler(rec, httptest.NewRequest("GET", "/metadata/lookup/cover?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}
}
