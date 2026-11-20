package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

// A placeholder made from Hardcover links to the book's page there: by the
// slug it was made with, or, for one made before slugs were kept, by its
// Hardcover id in the series' saved list. Editing it keeps them, and the
// book that replaces it gets them.
func TestHardcoverPlaceholderLink(t *testing.T) {
	db := catalogFixture(t)
	setupTestAppDB(t)
	t.Chdir(t.TempDir())
	placeholdersChanged()
	addTestBook(t, db, "Mort", []string{"Terry Pratchett"}, nil, "Discworld", "epub")
	aaron := addTestBook(t, db, "The Last Hero: A Life of Henry Aaron", []string{"Howard Bryant"}, nil, "", "epub")
	if err := writeLibraryPages(); err != nil {
		t.Fatal(err)
	}
	origClient, origWrites, origData := hcClient, allowWrites, dataDir
	hcClient, allowWrites, dataDir = hardcover.New(hardcover.Config{URL: "https://127.0.0.1:1/", Token: "test-token"}), true, t.TempDir()
	t.Cleanup(func() { hcClient, allowWrites, dataDir = origClient, origWrites, origData; placeholdersChanged() })
	pratchett := []string{"Terry Pratchett"}
	if err := hcSaveLink("Discworld", &hardcover.SeriesDetail{Series: hardcover.Series{ID: 9, Name: "Discworld", Slug: "discworld"},
		Entries: []hardcover.Entry{
			{BookID: 1, Slug: "mort", Title: "Mort", Position: pos(1), Authors: pratchett},
			{BookID: 27, Slug: "the-last-hero", Title: "The Last Hero", Position: pos(27), Authors: pratchett},
			{BookID: 28, Slug: "the-truth", Title: "The Truth", Position: pos(25), Authors: pratchett},
		}}, nil); err != nil {
		t.Fatal(err)
	}
	admin, _ := createUser("boss", "a long enough password", true)
	mux := placeholderMux()
	wrap := func(h http.HandlerFunc) http.HandlerFunc { return requireEditor(requireLookups(requireHardcover(h))) }
	mux.HandleFunc("POST /api/hardcover/notmatch", wrap(hardcoverNotMatchHandler))
	do := func(method, path, body string, want int) string {
		t.Helper()
		rec := call(t, mux, admin, method, path, body)
		if rec.Code != want {
			t.Fatalf("%s %s: %d %s (want %d)", method, path, rec.Code, rec.Body, want)
		}
		return rec.Body.String()
	}
	const heroURL = `href="https://hardcover.app/books/the-last-hero"`

	// Made now: both identifiers.
	var res struct{ Placeholder string }
	_ = json.Unmarshal([]byte(do("POST", "/api/hardcover/notmatch", `{"series":"Discworld","uuid":"`+aaron+`","hcBookId":27}`, http.StatusOK)), &res)
	p := placeholderByUUID(res.Placeholder)
	if p == nil || identifierValue(p.Meta, "hardcover") != "the-last-hero" || identifierValue(p.Meta, "hardcover-id") != "27" {
		t.Fatalf("placeholder %+v", p)
	}
	if page := do("GET", "/missing/"+p.UUID, "", http.StatusOK); !strings.Contains(page, heroURL) {
		t.Error("no Hardcover link on the placeholder page")
	}
	// Editing it (the form has only the ISBN) keeps them.
	do("PUT", "/api/placeholders/"+p.UUID, `{"title":"The Last Hero","series":"Discworld","series_index":"27","creators":"Terry Pratchett","isbn":"9780552152938"}`, http.StatusOK)
	if p = placeholderByUUID(p.UUID); identifierValue(p.Meta, "hardcover") != "the-last-hero" || identifierValue(p.Meta, "hardcover-id") != "27" ||
		identifierValue(p.Meta, "isbn") != "9780552152938" {
		t.Errorf("after editing: %+v", p.Meta.Identifiers)
	}

	// Made before slugs were kept: the series' list has it.
	old := &placeholder{Source: "hardcover", Meta: OPFMetadata{Title: "The Truth", Series: "Discworld", SeriesIndex: "25",
		Identifiers: []opfIdentifier{{Scheme: "hardcover-id", Value: "28"}}}}
	if err := savePlaceholder(old, admin); err != nil {
		t.Fatal(err)
	}
	if page := do("GET", "/missing/"+old.UUID, "", http.StatusOK); !strings.Contains(page, `href="https://hardcover.app/books/the-truth"`) {
		t.Error("no Hardcover link for an id-only placeholder")
	}
	// The book that replaces it gets the slug too.
	m := withPlaceholder(OPFMetadata{Title: "the truth", Identifiers: []opfIdentifier{{Scheme: "hardcover", Value: "wrong"}}}, old)
	if identifierValue(m, "hardcover") != "the-truth" || identifierValue(m, "hardcover-id") != "28" {
		t.Errorf("replacing book: %+v", m.Identifiers)
	}

	// Not on a linked series, or not a slug: no link.
	for _, meta := range []OPFMetadata{
		{Title: "x", Series: "Elsewhere", Identifiers: []opfIdentifier{{Scheme: "hardcover-id", Value: "28"}}},
		{Title: "x", Identifiers: []opfIdentifier{{Scheme: "hardcover", Value: "javascript:alert(1)"}}},
		{Title: "x", Identifiers: []opfIdentifier{{Scheme: "hardcover", Value: "../evil"}}},
	} {
		if u := hcMetaURL(meta); u != "" {
			t.Errorf("%+v: link %q", meta.Identifiers, u)
		}
	}
}
