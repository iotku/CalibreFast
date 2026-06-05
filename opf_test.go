package main

import (
	"slices"
	"testing"
)

func TestLoadOPF(t *testing.T) {
	// Test that we can load an OPF correctly
	actual, err := loadOPF("TestData/metadata.opf")
	if err != nil {
		t.Fatalf("Error on loadOPF: %e", err)
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
	expectedLang := "eng"
	if actual.Metadata.Language != expectedLang {
		t.Errorf("got %#v\nwant %#v", actual.Metadata.Language, expectedLang)
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
	var testOPF = &OPF{
		Metadata: OPFMetadata{
			Title:       "The Go Programming Language",
			Description: "A comprehensive introduction to the Go programming language by its creators.",
			Publisher:   "Addison-Wesley Professional",
			Date:        "2015-10-26",
			Language:    "eng",
			Identifiers: []opfIdentifier{
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
			Meta: []opfMeta{
				{Name: "calibre:series", Content: "Addison-Wesley Professional Computing Series"},
				{Name: "calibre:series_index", Content: "1"},
				{Name: "calibre:rating", Content: "10"},
				{Name: "calibre:timestamp", Content: "2015-10-26T00:00:00+00:00"},
			},
		},
	}

	err := writeOPF(testOPF, "testout.opf")
	if err != nil {
		t.Fatal(err)
	}
}
