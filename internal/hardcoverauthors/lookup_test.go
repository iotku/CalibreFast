package hardcoverauthors

import (
	"slices"
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
	"github.com/iotku/CalibreFast/pkg/textkey"
)

func TestAuthorNameVariants(t *testing.T) {
	for name, want := range map[string][]string{
		"J. R. R. Tolkien":   {"J. R. R. Tolkien", "J.R.R. Tolkien"},
		"J.R.R. Tolkien":     {"J.R.R. Tolkien", "J. R. R. Tolkien"},
		"Le Guin, Ursula K.": {"Le Guin, Ursula K.", "Ursula K. Le Guin"},
		"Terry Pratchett":    {"Terry Pratchett"},
	} {
		got := nameVariants(name)
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("%q: %q lacks %q", name, got, w)
			}
		}
	}
}

func TestPickAuthor(t *testing.T) {
	a := hardcover.Author{ID: 1, Name: "John Smith", UsersCount: 900, Titles: []string{"Famous Book"}}
	b := hardcover.Author{ID: 2, Name: "John Smith", UsersCount: 10, Titles: []string{"The Quiet Garden: A Novel"}}
	c := hardcover.Author{ID: 3, Name: "John Smith", UsersCount: 400, Titles: []string{"Other"}}
	// The one whose books the library has, however little read.
	if p := pickAuthor([]hardcover.Author{a, b}, map[string]bool{textkey.Title("Quiet Garden"): true}); p == nil || p.ID != 2 {
		t.Errorf("by books: %+v", p)
	}
	// None of theirs here: only one far more read than the next.
	if p := pickAuthor([]hardcover.Author{a, b}, nil); p == nil || p.ID != 1 {
		t.Errorf("far more read: %+v", p)
	}
	if p := pickAuthor([]hardcover.Author{a, c}, nil); p != nil {
		t.Errorf("ambiguous taken: %+v", p)
	}
	if p := pickAuthor([]hardcover.Author{c}, nil); p == nil || p.ID != 3 {
		t.Errorf("only one: %+v", p)
	}
	if pickAuthor(nil, nil) != nil {
		t.Error("none")
	}
}
