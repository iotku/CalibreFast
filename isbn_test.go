package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/isbn"
)

// writeISBNTestEPUB writes an EPUB whose package declares ids and whose spine
// has the given documents (path relative to OEBPS → body XHTML).
func writeISBNTestEPUB(t *testing.T, path, metadata string, docs []string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	add("mimetype", "application/epub+zip")
	add("META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`)
	var manifest, spine strings.Builder
	for i, body := range docs {
		id := "d" + string(rune('a'+i))
		name := "Text/doc " + id + ".xhtml"
		manifest.WriteString(`<item id="` + id + `" href="` + strings.ReplaceAll(name, " ", "%20") + `" media-type="application/xhtml+xml"/>`)
		spine.WriteString(`<itemref idref="` + id + `"/>`)
		add("OEBPS/"+name, `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>t</title></head><body>`+body+`</body></html>`)
	}
	add("OEBPS/content.opf", `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="uid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">`+metadata+`</metadata>
  <manifest>`+manifest.String()+`</manifest>
  <spine>`+spine.String()+`</spine>
</package>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testMOBI builds a MOBI file whose first record has an EXTH block with
// these records (when exth is non-nil).
func testMOBI(exth map[uint32]string) []byte {
	be := binary.BigEndian
	const mobiLen = 232
	rec0 := make([]byte, 16+mobiLen)
	copy(rec0[16:], "MOBI")
	be.PutUint32(rec0[20:], mobiLen)
	if exth != nil {
		be.PutUint32(rec0[0x80:], 0x40)
		var recs []byte
		for _, typ := range []uint32{100, 104, 113} {
			v, ok := exth[typ]
			if !ok {
				continue
			}
			r := make([]byte, 8+len(v))
			be.PutUint32(r, typ)
			be.PutUint32(r[4:], uint32(len(r)))
			copy(r[8:], v)
			recs = append(recs, r...)
		}
		h := make([]byte, 12)
		copy(h, "EXTH")
		be.PutUint32(h[4:], uint32(12+len(recs)))
		be.PutUint32(h[8:], uint32(len(exth)))
		rec0 = append(rec0, append(h, recs...)...)
	}
	hdr := make([]byte, 78+16+2)
	copy(hdr[60:], "BOOKMOBI")
	be.PutUint16(hdr[76:], 2)
	be.PutUint32(hdr[78:], uint32(len(hdr)))
	be.PutUint32(hdr[86:], uint32(len(hdr)+len(rec0)))
	return append(append(hdr, rec0...), []byte("text record")...)
}

func TestISBNCandidatesHandler(t *testing.T) {
	db, tempDir := setupTestDB(t)
	defer os.RemoveAll(tempDir)
	defer db.Close()
	origDB, origBase := calibreDB, baseDir
	calibreDB, baseDir = db, tempDir
	defer func() { calibreDB, baseDir = origDB, origBase }()
	const uuid = "isbn-test-uuid"
	dir := filepath.Join(baseDir, "Author", "Book (1)")
	os.MkdirAll(dir, 0o755)
	writeISBNTestEPUB(t, filepath.Join(dir, "Book.epub"), "", []string{"<p>ISBN 978-0-13-419044-0 (hardcover)</p>"})
	os.WriteFile(filepath.Join(dir, "Book.azw3"), testMOBI(map[uint32]string{104: "9780306406157"}), 0o644)
	os.WriteFile(filepath.Join(dir, "Book.pdf"), []byte("%PDF-1.4"), 0o644)
	uuidPathIndex.Store(uuid, "Author/Book (1)")
	formatCache.Delete(uuid)
	defer uuidPathIndex.Delete(uuid)
	defer formatCache.Delete(uuid)

	call := func(method, body string) isbn.Response {
		t.Helper()
		req := httptest.NewRequest(method, "/metadata/"+uuid+"/isbns", strings.NewReader(body))
		req.SetPathValue("uuid", uuid)
		rec := httptest.NewRecorder()
		isbnCandidatesHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body)
		}
		var resp isbn.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	got := call("GET", "")
	if got.PDF != "Book.pdf" || len(got.Candidates) != 2 ||
		got.Candidates[0] != (isbn.Candidate{ISBN13: "9780306406157", File: "Book.azw3", Source: "exth", Label: ""}) ||
		got.Candidates[1] != (isbn.Candidate{ISBN13: "9780134190440", File: "Book.epub", Source: "text", Label: "hardcover"}) {
		t.Fatalf("GET: %+v", got)
	}

	// The editor posts what pdf.js read from the PDF.
	got = call("POST", `{"xmp": "<x:xmpmeta xmlns:x='adobe:ns:meta/'><prism:isbn xmlns:prism='p'>9780134190587</prism:isbn></x:xmpmeta>",
		"pages": ["Copyright page\nISBN 978-3-16-148410-0 (PDF)"]}`)
	var isbns []string
	for _, c := range got.Candidates {
		isbns = append(isbns, c.ISBN13+" "+c.File+" "+c.Source)
	}
	want := []string{"9783161484100 Book.pdf text", "9780306406157 Book.azw3 exth", "9780134190587 Book.pdf xmp", "9780134190440 Book.epub text"}
	if !reflect.DeepEqual(isbns, want) {
		t.Fatalf("POST:\n got  %q\n want %q", isbns, want)
	}

	req := httptest.NewRequest("GET", "/metadata/nope/isbns", nil)
	req.SetPathValue("uuid", "nope")
	rec := httptest.NewRecorder()
	isbnCandidatesHandler(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown book: %d", rec.Code)
	}
}
