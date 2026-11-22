package main

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Which placeholders a book being added replaces (placeholdersToReplace),
// and the list of removed ones on /missing, restored from there.
func TestPlaceholdersToReplace(t *testing.T) {
	seriesFixture(t) // the library has Other #1 and #2
	origData, origWrites := dataDir, allowWrites
	dataDir, allowWrites = t.TempDir(), true
	placeholdersChanged()
	t.Cleanup(func() { dataDir, allowWrites = origData, origWrites; placeholdersChanged() })

	mk := func(title, author, series, index string) *placeholder {
		t.Helper()
		p := &placeholder{Meta: OPFMetadata{Title: title, Series: series, SeriesIndex: index}}
		if author != "" {
			p.Meta.Creators = []string{author}
		}
		m, err := cleanPlaceholderMeta(p.Meta)
		if err != nil {
			t.Fatal(err)
		}
		p.Meta = m
		if err := savePlaceholder(p, nil); err != nil {
			t.Fatal(err)
		}
		return p
	}
	titles := func(ms []placeholderMatch) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.P.Meta.Title+" ("+m.Why+")")
		}
		slices.Sort(out)
		return out
	}
	replaces := func(m OPFMetadata, forID, choice string) ([]string, error) {
		t.Helper()
		ms, err := placeholdersToReplace(&m, forID, choice)
		return titles(ms), err
	}

	// A series whose books aren't numbered: both placeholders are "#1".
	alpha := mk("Alpha", "Ann", "Loose", "")
	beta := mk("Beta", "Bob", "Loose", "")
	if got, _ := replaces(OPFMetadata{Title: "Alpha", Creators: []string{"Ann"}, Series: "Loose", SeriesIndex: "1"}, "", placeholdersAsk); !slices.Equal(got, []string{"Alpha (" + whyTitle + ")"}) {
		t.Errorf("Alpha added replaces %q, want Alpha alone", got)
	}
	if got, _ := replaces(OPFMetadata{Title: "Gamma", Creators: []string{"Ann"}, Series: "Loose", SeriesIndex: "1"}, "", placeholdersAsk); len(got) != 0 {
		t.Errorf("another unnumbered book replaces %q", got)
	}
	// Files added from Beta's page: Beta only, whatever they say.
	if got, _ := replaces(OPFMetadata{Title: "Alpha", Creators: []string{"Ann"}, Series: "Loose", SeriesIndex: "1"}, beta.UUID, placeholdersAsk); !slices.Equal(got, []string{"Beta (" + whyFilesAdded + ")"}) {
		t.Errorf("files for Beta replace %q", got)
	}

	// A numbered series: the number tells, unless the authors differ or a
	// library book has the number already.
	mk("Fifth", "Writer", "Other", "5")
	mk("First Again", "Writer", "Other", "1")
	if got, _ := replaces(OPFMetadata{Title: "Book Five", Creators: []string{"Writer"}, Series: "Other", SeriesIndex: "5"}, "", placeholdersAsk); !slices.Equal(got, []string{"Fifth (" + whySeriesIndex + ")"}) {
		t.Errorf("Other #5 replaces %q", got)
	}
	if got, _ := replaces(OPFMetadata{Title: "Book Five", Creators: []string{"Somebody"}, Series: "Other", SeriesIndex: "5"}, "", placeholdersAsk); len(got) != 0 {
		t.Errorf("Other #5 by another author replaces %q", got)
	}
	if got, _ := replaces(OPFMetadata{Title: "Another One", Creators: []string{"Writer"}, Series: "Other", SeriesIndex: "1"}, "", placeholdersAsk); len(got) != 0 {
		t.Errorf("Other #1, which the library has, replaces %q", got)
	}

	// Two placeholders for one book: asked first.
	mk("Twice", "Tess", "", "")
	mk("Twice", "Tess", "Elsewhere", "2")
	twice := OPFMetadata{Title: "Twice", Creators: []string{"Tess"}}
	_, err := replaces(twice, "", placeholdersAsk)
	var many *tooManyPlaceholdersError
	if !errors.As(err, &many) || len(many.Rows()) != 2 {
		t.Fatalf("two matches: %v, want tooManyPlaceholdersError", err)
	}
	if got, _ := replaces(twice, "", placeholdersAll); len(got) != 2 {
		t.Errorf("all: %q", got)
	}
	if got, _ := replaces(twice, "", placeholdersNone); len(got) != 0 {
		t.Errorf("none: %q", got)
	}

	// Replaced: on /missing's list of removed ones, how and by whom; restored
	// from there.
	admin, _ := createUser("boss", "a long enough password", true)
	ms, _ := placeholdersToReplace(&OPFMetadata{Title: "Alpha", Creators: []string{"Ann"}}, "", placeholdersAsk)
	resolvePlaceholders(ms, "some-book", admin)
	if placeholderByUUID(alpha.UUID) != nil {
		t.Fatal("Alpha not replaced")
	}
	removed := removedPlaceholders(10)
	if len(removed) != 1 || removed[0].UUID != alpha.UUID || removed[0].Why != whyTitle || removed[0].By != "boss" || removed[0].BookUUID != "some-book" {
		t.Fatalf("removed %+v", removed)
	}
	var n int
	appDB.QueryRow(`SELECT COUNT(*) FROM edit_log WHERE target = ? AND action LIKE 'placeholder in library%'`, alpha.UUID).Scan(&n)
	if n != 1 {
		t.Errorf("%d edit log entries for the replacement", n)
	}
	mux := placeholderMux()
	mux.HandleFunc("POST /api/placeholders/{uuid}/restore", requireWrites(placeholderRestoreHandler))
	if body := call(t, mux, admin, "GET", "/missing", "").Body.String(); !strings.Contains(body, "Recently removed") || !strings.Contains(body, whyTitle) {
		t.Error("/missing doesn't list the removed placeholder")
	}
	if rec := call(t, mux, admin, "POST", "/api/placeholders/"+alpha.UUID+"/restore", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if p := placeholderByUUID(alpha.UUID); p == nil || p.Meta.Title != "Alpha" {
		t.Fatal("not restored")
	}
	if len(removedPlaceholders(10)) != 0 {
		t.Error("restored, still listed as removed")
	}
	if rec := call(t, mux, admin, "POST", "/api/placeholders/no-such/restore", ""); rec.Code != http.StatusNotFound {
		t.Errorf("restoring an unknown one: %d", rec.Code)
	}
}
