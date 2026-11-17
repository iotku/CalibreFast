package opf

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadOPF(t *testing.T) {
	// Test that we can load an OPF correctly
	actual, err := Load("testdata/metadata.opf")
	if err != nil {
		t.Fatalf("Error on Load: %e", err)
	}

	// Test Title
	expectedTitle := "Contact"
	if actual.Metadata.Title != expectedTitle {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Title, expectedTitle)
	}

	// Test "Creators"/authors
	expectedAuthors := []string{"Carl Sagan"}
	if !slices.Equal(actual.Metadata.Creators, expectedAuthors) {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Creators, expectedAuthors)
	}

	// Test Publisher
	expectedPublisher := "Pocket Books"
	if actual.Metadata.Publisher != expectedPublisher {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Publisher, expectedPublisher)
	}

	// Test Language
	expectedLangs := []string{"eng"}
	if !slices.Equal(actual.Metadata.Languages, expectedLangs) {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Languages, expectedLangs)
	}

	// Test "Subjects"/tags
	expectedSubjects := []string{"Fiction", "Science Fiction", "Alien Contact", "General"}
	if !slices.Equal(actual.Metadata.Subjects, expectedSubjects) {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Subjects, expectedSubjects)
	}

	// Test Description
	expected := `<p class="description">Pulitzer Prize-winning author and astronomer Carl Sagan imagines the greatest adventure of all—the discovery of an advanced civilization in the depths of space.In December of 1999, a multinational team journeys out to the stars, to the most awesome encounter in human history. Who—or what—is out there? In Cosmos, Carl Sagan explained the universe. In Contact, he predicts its future—and our own.</p>`
	if actual.Metadata.Description != expected {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Description, expected)
	}

	// Test Identifiers
	expectedIdentifiers := map[string]string{
		"calibre": "27417", // NOTE: Should match Calibre database ID
		"uuid":    "c477d1cb-1ea6-48b9-811b-38819bd20732",
		"GOOGLE":  "pO6mDQAAQBAJ",
		"ISBN":    "9780099469506",
		"AMAZON":  "0671004107",
	}

	if len(actual.Metadata.Identifiers) != len(expectedIdentifiers) {
		t.Fatalf(
			"expected %d identifiers, got %d",
			len(expectedIdentifiers),
			len(actual.Metadata.Identifiers),
		)
	}

	for _, id := range actual.Metadata.Identifiers {
		want, ok := expectedIdentifiers[id.Scheme]
		if !ok {
			t.Errorf("unexpected scheme %q", id.Scheme)
			continue
		}

		if id.Value != want {
			t.Errorf(
				"scheme %q: got %q, want %q",
				id.Scheme,
				id.Value,
				want,
			)
		}
	}

	// Test Meta
	for _, meta := range actual.Metadata.Meta {
		switch meta.Name {
		case "calibre:rating":
			expected = "8"
		case "calibre:timestamp":
			expected = "2017-05-01T04:35:17+00:00"
		case "calibre:title_sort":
			expected = "Contact"
		default: // "calibre:user_metadata" is untested
			continue
		}

		if meta.Content != expected {
			t.Errorf("%v got %v, want %v", meta.Name, meta.Content, expected)
		}
	}
}

func TestSaveOPF(t *testing.T) {
	var testOPF = &Package{
		Metadata: Metadata{
			Title:       "The Go Programming Language",
			Description: "A comprehensive introduction to the Go programming language by its creators.",
			Publisher:   "Addison-Wesley Professional",
			Date:        "2015-10-26",
			Languages:   []string{"eng"},
			Identifiers: []Identifier{
				{Scheme: "UUID", Value: "7a1b2c3d-4e5f-6789-abcd-ef0123456789"},
				{Scheme: "ISBN", Value: "978-0134190440"},
			},
			Subjects: []string{
				"Computer Programming",
				"Go (Programming language)",
			},
			Creators: []string{
				"Alan A. A. Donovan",
				"Brian W. Kernighan",
			},
			Meta: []Meta{
				{Name: "calibre:series", Content: "Addison-Wesley Professional Computing Series"},
				{Name: "calibre:series_index", Content: "1"},
				{Name: "calibre:rating", Content: "10"},
				{Name: "calibre:timestamp", Content: "2015-10-26T00:00:00+00:00"},
			},
		},
	}

	err := Write(testOPF, filepath.Join(t.TempDir(), "testout.opf"), "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestOPFMultipleLanguagesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.opf")
	in := &Package{Metadata: Metadata{Title: "T", Languages: []string{"eng", "fra", "deu"}}}
	if err := Write(in, path, ""); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Metadata.Languages, in.Metadata.Languages) {
		t.Errorf("got %v, want %v", out.Metadata.Languages, in.Metadata.Languages)
	}
}

// package@unique-identifier points at id="uuid_id". It must land on the
// book's UUID identifier and never on an ISBN or other typed identifier.
func TestIdentifiersForWrite(t *testing.T) {
	isbn := Identifier{Scheme: "isbn", Value: testISBN}
	bookID := Identifier{Scheme: "uuid", Value: testBookUUID, ID: "uuid_id"}

	tests := []struct {
		name     string
		in       []Identifier
		bookUUID string
		want     []Identifier
	}{
		{
			name:     "EPUB's own uuid plus a new ISBN: the book's uuid is added and gets uuid_id",
			in:       []Identifier{{Value: testEPUBUUID, ID: "uid"}, isbn},
			bookUUID: testBookUUID,
			want:     []Identifier{bookID, {Value: testEPUBUUID, ID: "uid"}, isbn},
		},
		{
			name: "without the book's uuid, the urn:uuid identifier gets it, not the ISBN",
			in:   []Identifier{{Value: testEPUBUUID, ID: "uid"}, isbn},
			want: []Identifier{{Value: testEPUBUUID, ID: "uuid_id"}, isbn},
		},
		{
			name: "an ISBN listed first still doesn't get it",
			in:   []Identifier{isbn, {Value: testEPUBUUID}},
			want: []Identifier{isbn, {Value: testEPUBUUID, ID: "uuid_id"}},
		},
		{
			name:     "a file where an ISBN was given uuid_id is repaired",
			in:       []Identifier{{Value: testEPUBUUID, ID: "uid"}, {Scheme: "isbn", Value: testISBN, ID: "uuid_id"}},
			bookUUID: testBookUUID,
			want:     []Identifier{bookID, {Value: testEPUBUUID, ID: "uid"}, isbn},
		},
		{
			name: "same repair without the book's uuid",
			in:   []Identifier{{Value: testEPUBUUID, ID: "uid"}, {Scheme: "isbn", Value: testISBN, ID: "uuid_id"}},
			want: []Identifier{{Value: testEPUBUUID, ID: "uuid_id"}, isbn},
		},
		{
			name:     "Calibre's own layout is left alone",
			in:       []Identifier{{Scheme: "calibre", Value: "7", ID: "calibre_id"}, bookID, isbn},
			bookUUID: testBookUUID,
			want:     []Identifier{{Scheme: "calibre", Value: "7", ID: "calibre_id"}, bookID, isbn},
		},
		{
			name:     "the book's uuid as a scheme-less urn, in another case, counts",
			in:       []Identifier{isbn, {Value: "URN:UUID:" + strings.ToUpper(testBookUUID), ID: "uid"}},
			bookUUID: testBookUUID,
			want:     []Identifier{isbn, {Value: "URN:UUID:" + strings.ToUpper(testBookUUID), ID: "uuid_id"}},
		},
		{
			name:     "the book's uuid typed as something else doesn't count",
			in:       []Identifier{{Scheme: "isbn", Value: testBookUUID}},
			bookUUID: testBookUUID,
			want:     []Identifier{bookID, {Scheme: "isbn", Value: testBookUUID}},
		},
		{
			name: "an EPUB from another Calibre library: its uuid_id gives way to this library's uuid",
			in: []Identifier{
				{Scheme: "uuid", Value: "11111111-2222-4333-8444-555555555555", ID: "uuid_id"},
				{Scheme: "calibre", Value: "5", ID: "calibre_id"},
				isbn,
			},
			bookUUID: testBookUUID,
			want: []Identifier{
				bookID,
				{Scheme: "uuid", Value: "11111111-2222-4333-8444-555555555555"},
				{Scheme: "calibre", Value: "5", ID: "calibre_id"},
				isbn,
			},
		},
		{
			name:     "of two holding the book's uuid, the one already called uuid_id keeps it",
			in:       []Identifier{{Value: "urn:uuid:" + testBookUUID, ID: "uid"}, bookID},
			bookUUID: testBookUUID,
			want:     []Identifier{{Value: "urn:uuid:" + testBookUUID, ID: "uid"}, bookID},
		},
		{
			name:     "of two holding the book's uuid, the uuid-typed one is preferred",
			in:       []Identifier{{Value: "urn:uuid:" + testBookUUID}, {Scheme: "uuid", Value: testBookUUID}},
			bookUUID: testBookUUID,
			want:     []Identifier{{Value: "urn:uuid:" + testBookUUID}, bookID},
		},
		{
			name: "only typed identifiers and no book uuid: none gets uuid_id",
			in:   []Identifier{isbn, {Value: "urn:isbn:" + testISBN}, {Scheme: "calibre", Value: "7"}},
			want: []Identifier{isbn, {Value: "urn:isbn:" + testISBN}, {Scheme: "calibre", Value: "7"}},
		},
		{
			name: "a uuid-typed identifier gets it when nothing better is there",
			in:   []Identifier{isbn, {Scheme: "UUID", Value: "x"}},
			want: []Identifier{isbn, {Scheme: "UUID", Value: "x", ID: "uuid_id"}},
		},
		{
			name:     "no identifiers: the book's uuid is added",
			bookUUID: testBookUUID,
			want:     []Identifier{bookID},
		},
		{
			name: "no identifiers and no book uuid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(tt.in)
			got := identifiersForWrite(in, tt.bookUUID)
			if !slices.Equal(got, tt.want) {
				t.Errorf("got  %#v\nwant %#v", got, tt.want)
			}
			if !slices.Equal(in, tt.in) {
				t.Errorf("input was modified: %#v", in)
			}
		})
	}
}

func TestWriteOPFUniqueIdentifierIsBookUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.opf")
	in := &Package{Metadata: Metadata{Title: "T", Identifiers: []Identifier{
		{Value: testEPUBUUID, ID: "uid"},
		{Scheme: "isbn", Value: testISBN},
	}}}
	if err := Write(in, path, testBookUUID); err != nil {
		t.Fatal(err)
	}
	want := Identifier{Scheme: "uuid", Value: testBookUUID, ID: "uuid_id"}
	if got := uniqueIdentifier(t, path); got != want {
		t.Errorf("unique identifier: got %#v, want %#v", got, want)
	}
	if in.Metadata.Identifiers[0].ID != "uid" || len(in.Metadata.Identifiers) != 2 {
		t.Errorf("caller's identifiers were modified: %#v", in.Metadata.Identifiers)
	}
}

func TestFillSeries(t *testing.T) {
	calibre := Metadata{Meta: []Meta{{Name: "calibre:series", Content: "Dune"}, {Name: "calibre:series_index", Content: "2.0"}}}
	calibre.FillSeries()
	if calibre.Series != "Dune" || calibre.SeriesIndex != "2" {
		t.Fatalf("calibre meta: %q %q", calibre.Series, calibre.SeriesIndex)
	}
	epub3 := Metadata{Meta: []Meta{
		{Property: "belongs-to-collection", ID: "c1", Value: "Box Set"},
		{Property: "collection-type", Refines: "#c1", Value: "set"},
		{Property: "belongs-to-collection", ID: "c2", Value: "The Expanse"},
		{Property: "collection-type", Refines: "#c2", Value: "series"},
		{Property: "group-position", Refines: "#c2", Value: "3"},
	}}
	epub3.FillSeries()
	if epub3.Series != "The Expanse" || epub3.SeriesIndex != "3" {
		t.Fatalf("EPUB 3 meta: %q %q", epub3.Series, epub3.SeriesIndex)
	}

	// Writing back replaces both kinds with Calibre's.
	epub3.Series, epub3.SeriesIndex = "Expanse", "3.5"
	epub3.SyncSeriesMeta()
	if got := epub3.GetMeta("calibre:series"); got != "Expanse" || epub3.GetMeta("calibre:series_index") != "3.5" || len(epub3.Meta) != 2 {
		t.Fatalf("after sync: %+v", epub3.Meta)
	}
	epub3.Series = ""
	epub3.SyncSeriesMeta()
	if len(epub3.Meta) != 0 {
		t.Fatalf("after removing: %+v", epub3.Meta)
	}

	for index, want := range map[string]float64{"": 1, "2": 2, " 2.5 ": 2.5} {
		if _, f, err := CleanSeries("Dune", index); err != nil || f != want {
			t.Fatalf("CleanSeries(%q): %v %v", index, f, err)
		}
	}
	for _, bad := range []string{"two", "-1", "NaN"} {
		if _, _, err := CleanSeries("Dune", bad); err == nil {
			t.Fatalf("CleanSeries(%q) accepted", bad)
		}
	}
}

const (
	testBookUUID = "0b6e3f4a-6c1e-4d2b-9a57-3f0c2a9d8e11"          // the book's uuid in metadata.db
	testEPUBUUID = "urn:uuid:5d1c7a2e-90b4-4f1e-8c3a-2b7f6e4d1a09" // an EPUB's own identifier
	testISBN     = "9780306406157"
)

// uniqueIdentifier returns the dc:identifier that the file's
// package@unique-identifier points at, failing unless exactly one does.
func uniqueIdentifier(t *testing.T, path string) Identifier {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		UniqueID    string       `xml:"unique-identifier,attr"`
		Identifiers []Identifier `xml:"metadata>identifier"`
	}
	if err := xml.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	var found []Identifier
	for _, id := range pkg.Identifiers {
		if id.ID == pkg.UniqueID {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("unique-identifier %q matches %d identifiers in:\n%s", pkg.UniqueID, len(found), data)
	}
	return found[0]
}
