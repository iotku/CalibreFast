package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	var testOPF = &OPF{
		Metadata: OPFMetadata{
			Title:       "The Go Programming Language",
			Description: "A comprehensive introduction to the Go programming language by its creators.",
			Publisher:   "Addison-Wesley Professional",
			Date:        "2015-10-26",
			Languages:   []string{"eng"},
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

	err := writeOPF(testOPF, "testout.opf", "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestOPFMultipleLanguagesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.opf")
	in := &OPF{Metadata: OPFMetadata{Title: "T", Languages: []string{"eng", "fra", "deu"}}}
	if err := writeOPF(in, path, ""); err != nil {
		t.Fatal(err)
	}
	out, err := loadOPF(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Metadata.Languages, in.Metadata.Languages) {
		t.Errorf("got %v, want %v", out.Metadata.Languages, in.Metadata.Languages)
	}
}

const (
	testBookUUID = "0b6e3f4a-6c1e-4d2b-9a57-3f0c2a9d8e11"          // the book's uuid in metadata.db
	testEPUBUUID = "urn:uuid:5d1c7a2e-90b4-4f1e-8c3a-2b7f6e4d1a09" // an EPUB's own identifier
	testISBN     = "9780306406157"
)

// package@unique-identifier points at id="uuid_id". It must land on the
// book's UUID identifier and never on an ISBN or other typed identifier.
func TestIdentifiersForWrite(t *testing.T) {
	isbn := opfIdentifier{Scheme: "isbn", Value: testISBN}
	bookID := opfIdentifier{Scheme: "uuid", Value: testBookUUID, ID: "uuid_id"}

	tests := []struct {
		name     string
		in       []opfIdentifier
		bookUUID string
		want     []opfIdentifier
	}{
		{
			name:     "EPUB's own uuid plus a new ISBN: the book's uuid is added and gets uuid_id",
			in:       []opfIdentifier{{Value: testEPUBUUID, ID: "uid"}, isbn},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{bookID, {Value: testEPUBUUID, ID: "uid"}, isbn},
		},
		{
			name: "without the book's uuid, the urn:uuid identifier gets it, not the ISBN",
			in:   []opfIdentifier{{Value: testEPUBUUID, ID: "uid"}, isbn},
			want: []opfIdentifier{{Value: testEPUBUUID, ID: "uuid_id"}, isbn},
		},
		{
			name: "an ISBN listed first still doesn't get it",
			in:   []opfIdentifier{isbn, {Value: testEPUBUUID}},
			want: []opfIdentifier{isbn, {Value: testEPUBUUID, ID: "uuid_id"}},
		},
		{
			name:     "a file where an ISBN was given uuid_id is repaired",
			in:       []opfIdentifier{{Value: testEPUBUUID, ID: "uid"}, {Scheme: "isbn", Value: testISBN, ID: "uuid_id"}},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{bookID, {Value: testEPUBUUID, ID: "uid"}, isbn},
		},
		{
			name: "same repair without the book's uuid",
			in:   []opfIdentifier{{Value: testEPUBUUID, ID: "uid"}, {Scheme: "isbn", Value: testISBN, ID: "uuid_id"}},
			want: []opfIdentifier{{Value: testEPUBUUID, ID: "uuid_id"}, isbn},
		},
		{
			name:     "Calibre's own layout is left alone",
			in:       []opfIdentifier{{Scheme: "calibre", Value: "7", ID: "calibre_id"}, bookID, isbn},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{{Scheme: "calibre", Value: "7", ID: "calibre_id"}, bookID, isbn},
		},
		{
			name:     "the book's uuid as a scheme-less urn, in another case, counts",
			in:       []opfIdentifier{isbn, {Value: "URN:UUID:" + strings.ToUpper(testBookUUID), ID: "uid"}},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{isbn, {Value: "URN:UUID:" + strings.ToUpper(testBookUUID), ID: "uuid_id"}},
		},
		{
			name:     "the book's uuid typed as something else doesn't count",
			in:       []opfIdentifier{{Scheme: "isbn", Value: testBookUUID}},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{bookID, {Scheme: "isbn", Value: testBookUUID}},
		},
		{
			name: "an EPUB from another Calibre library: its uuid_id gives way to this library's uuid",
			in: []opfIdentifier{
				{Scheme: "uuid", Value: "11111111-2222-4333-8444-555555555555", ID: "uuid_id"},
				{Scheme: "calibre", Value: "5", ID: "calibre_id"},
				isbn,
			},
			bookUUID: testBookUUID,
			want: []opfIdentifier{
				bookID,
				{Scheme: "uuid", Value: "11111111-2222-4333-8444-555555555555"},
				{Scheme: "calibre", Value: "5", ID: "calibre_id"},
				isbn,
			},
		},
		{
			name:     "of two holding the book's uuid, the one already called uuid_id keeps it",
			in:       []opfIdentifier{{Value: "urn:uuid:" + testBookUUID, ID: "uid"}, bookID},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{{Value: "urn:uuid:" + testBookUUID, ID: "uid"}, bookID},
		},
		{
			name:     "of two holding the book's uuid, the uuid-typed one is preferred",
			in:       []opfIdentifier{{Value: "urn:uuid:" + testBookUUID}, {Scheme: "uuid", Value: testBookUUID}},
			bookUUID: testBookUUID,
			want:     []opfIdentifier{{Value: "urn:uuid:" + testBookUUID}, bookID},
		},
		{
			name: "only typed identifiers and no book uuid: none gets uuid_id",
			in:   []opfIdentifier{isbn, {Value: "urn:isbn:" + testISBN}, {Scheme: "calibre", Value: "7"}},
			want: []opfIdentifier{isbn, {Value: "urn:isbn:" + testISBN}, {Scheme: "calibre", Value: "7"}},
		},
		{
			name: "a uuid-typed identifier gets it when nothing better is there",
			in:   []opfIdentifier{isbn, {Scheme: "UUID", Value: "x"}},
			want: []opfIdentifier{isbn, {Scheme: "UUID", Value: "x", ID: "uuid_id"}},
		},
		{
			name:     "no identifiers: the book's uuid is added",
			bookUUID: testBookUUID,
			want:     []opfIdentifier{bookID},
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

// uniqueIdentifier returns the dc:identifier that the file's
// package@unique-identifier points at, failing unless exactly one does.
func uniqueIdentifier(t *testing.T, path string) opfIdentifier {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pkg struct {
		UniqueID    string          `xml:"unique-identifier,attr"`
		Identifiers []opfIdentifier `xml:"metadata>identifier"`
	}
	if err := xml.Unmarshal(data, &pkg); err != nil {
		t.Fatal(err)
	}
	var found []opfIdentifier
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

func TestWriteOPFUniqueIdentifierIsBookUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.opf")
	in := &OPF{Metadata: OPFMetadata{Title: "T", Identifiers: []opfIdentifier{
		{Value: testEPUBUUID, ID: "uid"},
		{Scheme: "isbn", Value: testISBN},
	}}}
	if err := writeOPF(in, path, testBookUUID); err != nil {
		t.Fatal(err)
	}
	want := opfIdentifier{Scheme: "uuid", Value: testBookUUID, ID: "uuid_id"}
	if got := uniqueIdentifier(t, path); got != want {
		t.Errorf("unique identifier: got %#v, want %#v", got, want)
	}
	if in.Metadata.Identifiers[0].ID != "uid" || len(in.Metadata.Identifiers) != 2 {
		t.Errorf("caller's identifiers were modified: %#v", in.Metadata.Identifiers)
	}
}

// testEPUB returns a minimal EPUB whose OPF has the given dc:identifier XML.
func testEPUB(t *testing.T, identifiers string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"OEBPS/content.opf", `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" unique-identifier="uid" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>Uploaded</dc:title>
    <dc:creator>Some Author</dc:creator>
    <dc:language>en</dc:language>
    ` + identifiers + `
  </metadata>
</package>`},
	} {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The reported bug, end to end: upload an EPUB whose own identifier is
// <dc:identifier id="uid">urn:uuid:…</dc:identifier>, then set an ISBN in the
// editor. uuid_id used to end up on the ISBN.
func TestUploadThenSetISBN_UUIDIDStaysOnBookUUID(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()

	origDB, origBaseDir := calibreDB, baseDir
	calibreDB, baseDir = db, tempDir
	defer func() { calibreDB, baseDir = origDB, origBaseDir }()

	// Upload.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("books", "Uploaded.epub")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(testEPUB(t, `<dc:identifier id="uid">`+testEPUBUUID+`</dc:identifier>`))
	mw.Close()
	form, err := multipart.NewReader(&body, mw.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()

	res := processUploadGroup(form.File["books"])
	if res.Error != "" {
		t.Fatalf("upload: %s", res.Error)
	}
	var bookPath string
	if err := db.QueryRow(`SELECT path FROM books WHERE uuid = ?`, res.UUID).Scan(&bookPath); err != nil {
		t.Fatal(err)
	}
	opfPath := filepath.Join(tempDir, bookPath, "metadata.opf")
	bookID := opfIdentifier{Scheme: "uuid", Value: res.UUID, ID: "uuid_id"}

	check := func(stage string, want []opfIdentifier) {
		t.Helper()
		opf, err := loadOPF(opfPath)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if !slices.Equal(opf.Metadata.Identifiers, want) {
			t.Errorf("%s: identifiers\n got  %#v\nwant %#v", stage, opf.Metadata.Identifiers, want)
		}
		if got := uniqueIdentifier(t, opfPath); got != bookID {
			t.Errorf("%s: unique identifier is %#v, want %#v", stage, got, bookID)
		}
	}
	check("after upload", []opfIdentifier{bookID, {Value: testEPUBUUID, ID: "uid"}})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /metadata/{uuid}/apply", applyMetadataHandler)
	apply := func(ids []map[string]string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"title": "Uploaded", "creators": "Some Author", "identifiers": ids})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metadata/"+res.UUID+"/apply", bytes.NewReader(payload)))
		if rec.Code != http.StatusOK {
			t.Fatalf("apply: %d %s", rec.Code, rec.Body)
		}
	}

	// The editor sends back the identifiers it loaded, with their ids, plus
	// the new ISBN.
	apply([]map[string]string{
		{"scheme": "uuid", "value": res.UUID, "id": "uuid_id"},
		{"scheme": "", "value": testEPUBUUID, "id": "uid"},
		{"scheme": "isbn", "value": testISBN, "id": ""},
	})
	want := []opfIdentifier{bookID, {Value: testEPUBUUID, ID: "uid"}, {Scheme: "isbn", Value: testISBN}}
	check("after setting an ISBN", want)

	// A file saved before this fix (the ISBN holds uuid_id, no uuid of the
	// book's own) is repaired by the next save, and removing the book's uuid
	// in the editor doesn't remove it from the file.
	apply([]map[string]string{
		{"scheme": "", "value": testEPUBUUID, "id": "uid"},
		{"scheme": "isbn", "value": testISBN, "id": "uuid_id"},
	})
	check("after saving a file written by the old code", want)
}
