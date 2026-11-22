package opf

import "testing"

func TestParseRating(t *testing.T) {
	for in, want := range map[string]int{"8": 8, "8.0": 8, " 9 ": 9, "10": 10, "11": 10, "-2": 0, "": 0, "x": 0, "NaN": 0, "4.6": 5} {
		if got := ParseRating(in); got != want {
			t.Errorf("ParseRating(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestRatingMetaRoundTrip(t *testing.T) {
	m := Metadata{Meta: []Meta{{Name: "calibre:rating", Content: "6.0"}, {Name: "calibre:series", Content: "S"}}}
	m.FillRating()
	if m.Rating != 6 {
		t.Fatalf("filled %d", m.Rating)
	}
	m.Rating = 9
	m.SyncRatingMeta()
	if m.GetMeta("calibre:rating") != "9" || m.GetMeta("calibre:series") != "S" || len(m.Meta) != 2 {
		t.Errorf("synced %+v", m.Meta)
	}
	m.Rating = 0
	m.SyncRatingMeta()
	if m.GetMeta("calibre:rating") != "" || len(m.Meta) != 1 {
		t.Errorf("cleared %+v", m.Meta)
	}
	// Set already: the meta doesn't override it.
	m = Metadata{Rating: 4, Meta: []Meta{{Name: "calibre:rating", Content: "10"}}}
	m.FillRating()
	if m.Rating != 4 {
		t.Errorf("FillRating overrode %d", m.Rating)
	}
}
