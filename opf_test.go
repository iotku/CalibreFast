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
	"testing"

	"github.com/iotku/CalibreFast/pkg/opf"
)

const (
	testBookUUID = "0b6e3f4a-6c1e-4d2b-9a57-3f0c2a9d8e11"          // the book's uuid in metadata.db
	testEPUBUUID = "urn:uuid:5d1c7a2e-90b4-4f1e-8c3a-2b7f6e4d1a09" // an EPUB's own identifier
	testISBN     = "9780306406157"
)

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

// epubWithIdentifiers returns a minimal EPUB whose OPF has the given dc:identifier XML.
func epubWithIdentifiers(t *testing.T, identifiers string) []byte {
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
	fw.Write(epubWithIdentifiers(t, `<dc:identifier id="uid">`+testEPUBUUID+`</dc:identifier>`))
	mw.Close()
	form, err := multipart.NewReader(&body, mw.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer form.RemoveAll()

	res := processUploadGroup(form.File["books"], false)
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
		opf, err := opf.Load(opfPath)
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

	// The editor doesn't see library identifiers (calibre, uuid, urn:uuid:);
	// it sends the ISBN, and the save keeps the file's own.
	apply([]map[string]string{{"scheme": "isbn", "value": testISBN, "id": ""}})
	want := []opfIdentifier{bookID, {Value: testEPUBUUID, ID: "uid"}, {Scheme: "isbn", Value: testISBN}}
	check("after setting an ISBN", want)

	// A file saved before this fix: no uuid of the book's own, and the ISBN
	// holds uuid_id (which the editor sends back). The next save repairs it.
	old := `<?xml version='1.0' encoding='utf-8'?>
<package xmlns="http://www.idpf.org/2007/opf" unique-identifier="uuid_id" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>Uploaded</dc:title>
    <dc:identifier id="uid">` + testEPUBUUID + `</dc:identifier>
    <dc:identifier opf:scheme="isbn" id="uuid_id">` + testISBN + `</dc:identifier>
  </metadata>
</package>`
	if err := os.WriteFile(opfPath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	apply([]map[string]string{{"scheme": "isbn", "value": testISBN, "id": "uuid_id"}})
	check("after saving a file written by the old code", want)
}
