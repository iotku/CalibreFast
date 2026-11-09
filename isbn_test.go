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
)

func TestNormalizeISBN(t *testing.T) {
	cases := map[string]string{
		"9780134190440":              "9780134190440",
		"978-0-13-419044-0":          "9780134190440",
		"0134190440":                 "9780134190440", // ISBN-10 → 13
		"0-13-419044-0":              "9780134190440",
		"080442957X":                 "9780804429573", // X check digit
		"0-8044-2957-x":              "9780804429573",
		"urn:isbn:978-0-13-419058-7": "9780134190587",
		"ISBN-13: 978 0 13 419058 7": "9780134190587",
		"ISBN13: 9780134190587":      "9780134190587",
		"isbn:0306406152":            "9780306406157",
		"ISBN 0-306-40615-2":         "9780306406157",
		"979-10-90636-07-1":          "9791090636071", // 979 prefix
		"978–0–13–419044–0":          "9780134190440", // en dashes

		"0134190441":         "", // wrong ISBN-10 check digit
		"9780134190441":      "", // wrong ISBN-13 check digit
		"9770134190440":      "", // 977 is ISSN, not ISBN
		"978013419044":       "", // 12 digits
		"01341904X0":         "", // X not last
		"97801341904X0":      "",
		"978-0-13-419044-0a": "",
		"":                   "",
	}
	for in, want := range cases {
		if got := normalizeISBN(in); got != want {
			t.Errorf("normalizeISBN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentifierISBN(t *testing.T) {
	cases := []struct{ scheme, value, want string }{
		{"ISBN", "978-0-13-419044-0", "9780134190440"},
		{"", "9780134190440", "9780134190440"},
		{"", "urn:isbn:9780134190440", "9780134190440"},
		{"AMAZON", "0134190440", ""},                       // an ASIN that looks like an ISBN-10
		{"calibre", "isbn:9780134190440", "9780134190440"}, // says so itself
		{"uuid", "urn:uuid:0b6f4d3a-5c1e-4f6a-9e3b-2f1d8c7a6b5e", ""},
	}
	for _, c := range cases {
		if got := identifierISBN(c.scheme, c.value); got != c.want {
			t.Errorf("identifierISBN(%q, %q) = %q, want %q", c.scheme, c.value, got, c.want)
		}
	}
}

type isbnLabel struct{ isbn, label string }

func labels(cs []isbnCandidate) []isbnLabel {
	out := []isbnLabel{}
	for _, c := range cs {
		out = append(out, isbnLabel{c.ISBN13, c.Label})
	}
	return out
}

func TestISBNsInText(t *testing.T) {
	const hard, ebook, isbn10 = "9780134190440", "9780134190587", "9780306406157"
	cases := []struct {
		name, text string
		want       []isbnLabel
	}{
		{"one per line, in brackets",
			"ISBN 978-0-13-419044-0 (hardcover : alk. paper)\nISBN 978-0-13-419058-7 (ebook)",
			[]isbnLabel{{hard, "hardcover"}, {ebook, "ebook"}}},
		{"labels before, one line",
			"eBook ISBN: 9780134190587 Print ISBN: 9780134190440",
			[]isbnLabel{{ebook, "ebook"}, {hard, "print"}}},
		{"labels after, one line",
			"ISBN 9780134190440 hardcover, ISBN 9780134190587 ebook",
			[]isbnLabel{{hard, "hardcover"}, {ebook, "ebook"}}},
		{"eISBN", "e-ISBN 978-0-13-419058-7", []isbnLabel{{ebook, "ebook"}}},
		{"pbk in brackets", "ISBN 0-306-40615-2 (pbk.)", []isbnLabel{{isbn10, "paperback"}}},
		{"ISBN-10 and -13 of one book",
			"ISBN-13: 978-0-13-419044-0\nISBN-10: 0-13-419044-0",
			[]isbnLabel{{hard, ""}, {hard, ""}}},
		{"no keyword: 13 digits count", "Printed in the USA 978-0-13-419044-0", []isbnLabel{{hard, ""}}},
		{"no keyword: 10 digits don't", "Call 0-306-40615-2 for orders", []isbnLabel{}},
		{"keyword too far back", "ISBN is printed on the back cover, look for the barcode there: 0-306-40615-2", []isbnLabel{}},
		{"wrong check digit", "ISBN 978-0-13-419044-1", []isbnLabel{}},
		{"part of a longer number", "Order 12-9780134190440, ref 97801341904405", []isbnLabel{}},
		{"spaces and dashes", "ISBN 978 0 13 419058 7 / 978–0–13–419044–0", []isbnLabel{{ebook, ""}, {hard, ""}}},
		{"label on a different line doesn't count", "ebook\nISBN 978-0-13-419044-0", []isbnLabel{{hard, ""}}},
	}
	for _, c := range cases {
		got := labels(isbnsInText(c.text, "b.epub"))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %v\n want %v", c.name, got, c.want)
		}
	}
}

func TestRankISBNs(t *testing.T) {
	in := []isbnCandidate{
		{ISBN13: "A", Source: "text", Label: "hardcover"},
		{ISBN13: "B", Source: "text"},
		{ISBN13: "C", Source: "opf"},
		{ISBN13: "D", Source: "text", Label: "epub"},
		{ISBN13: "C", Source: "exth"}, // repeat
		{ISBN13: "A", Source: "text", Label: "audiobook"},
	}
	var got []string
	for _, c := range rankISBNs(in) {
		got = append(got, c.ISBN13+"/"+c.Source)
	}
	want := []string{"D/text", "C/opf", "B/text", "A/text"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestXHTMLText(t *testing.T) {
	doc := `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>ISBN 9783161484100</title>
<style>p { margin: 0 }</style></head>
<body><div class="copy"><p>Copyright &copy; 2015</p>
<p>ISBN&nbsp;978-0-13-419058-7
   (ebook)</p><script>var x = "ISBN 9783161484100";</script>
<p>Next<br/>line</p></div></body></html>`
	text := xhtmlText([]byte(doc))
	if strings.Contains(text, "9783161484100") {
		t.Errorf("text from <title>/<script> leaked: %q", text)
	}
	for _, want := range []string{"Copyright © 2015", "ISBN 978-0-13-419058-7 (ebook)", "Next\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}

// writeTestEPUB writes an EPUB whose package declares ids and whose spine
// has the given documents (path relative to OEBPS → body XHTML).
func writeTestEPUB(t *testing.T, path, metadata string, docs []string) {
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

func TestEPUBISBNs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.epub")
	docs := make([]string, 10)
	for i := range docs {
		docs[i] = "<p>Chapter text.</p>"
	}
	docs[1] = "<p>ISBN 978-0-13-419044-0 (hardcover)</p><p>ISBN 978-0-13-419058-7 (ebook)</p>"
	docs[6] = "<p>ISBN 978-3-16-148410-0</p>" // the middle of the book isn't read
	docs[9] = "<p>Also available: ISBN 0-306-40615-2</p>"
	writeTestEPUB(t, path, `
    <dc:identifier id="uid">urn:uuid:0b6f4d3a-5c1e-4f6a-9e3b-2f1d8c7a6b5e</dc:identifier>
    <dc:identifier opf:scheme="ISBN">9780134190587</dc:identifier>
    <dc:identifier opf:scheme="AMAZON">0134190440</dc:identifier>
    <dc:source>urn:isbn:9780134190440</dc:source>`, docs)

	found, err := epubISBNs(path, "book.epub")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range rankISBNs(found) {
		got = append(got, c.ISBN13+" "+c.Source+" "+c.Label)
	}
	want := []string{
		"9780134190587 text ebook",
		"9780306406157 text ",
		"9780134190440 opf print",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
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

func TestMobiEXTH(t *testing.T) {
	data := testMOBI(map[uint32]string{100: "A. Author", 104: "978-0-13-419058-7", 113: "B00XYZ"})
	got, err := mobiEXTH(bytes.NewReader(data), 104)
	if err != nil || !reflect.DeepEqual(got, []string{"978-0-13-419058-7"}) {
		t.Fatalf("got %q, %v", got, err)
	}

	// No EXTH block: nothing, no error.
	if got, err := mobiEXTH(bytes.NewReader(testMOBI(nil)), 104); err != nil || got != nil {
		t.Errorf("without EXTH: got %q, %v", got, err)
	}
	// Not a MOBI file, or cut short: an error, never a panic.
	if _, err := mobiEXTH(bytes.NewReader([]byte("PK\x03\x04 not a mobi file at all, just some bytes here................................")), 104); err == nil {
		t.Error("zip accepted as MOBI")
	}
	for _, n := range []int{10, 90, 120, 200, 300} {
		if n < len(data) {
			mobiEXTH(bytes.NewReader(data[:n]), 104)
		}
	}

	path := filepath.Join(t.TempDir(), "b.azw3")
	os.WriteFile(path, data, 0o644)
	cs, err := mobiISBNs(path, "b.azw3")
	if err != nil || len(cs) != 1 || cs[0] != (isbnCandidate{ISBN13: "9780134190587", File: "b.azw3", Source: "exth"}) {
		t.Errorf("mobiISBNs: %+v, %v", cs, err)
	}
}

func TestXMPISBNs(t *testing.T) {
	xmp := `<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about="" xmlns:prism="http://prismstandard.org/namespaces/basic/2.0/"
      xmlns:dc="http://purl.org/dc/elements/1.1/" prism:eIsbn="9780134190587">
   <prism:isbn>978-0-13-419044-0</prism:isbn>
   <dc:identifier><rdf:Bag><rdf:li>urn:isbn:0306406152</rdf:li><rdf:li>uuid:1234</rdf:li></rdf:Bag></dc:identifier>
   <dc:title><rdf:Alt><rdf:li xml:lang="x-default">9783161484100</rdf:li></rdf:Alt></dc:title>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>`
	got := labels(xmpISBNs(xmp, "b.pdf"))
	want := []isbnLabel{{"9780134190587", "ebook"}, {"9780134190440", ""}, {"9780306406157", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
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
	writeTestEPUB(t, filepath.Join(dir, "Book.epub"), "", []string{"<p>ISBN 978-0-13-419044-0 (hardcover)</p>"})
	os.WriteFile(filepath.Join(dir, "Book.azw3"), testMOBI(map[uint32]string{104: "9780306406157"}), 0o644)
	os.WriteFile(filepath.Join(dir, "Book.pdf"), []byte("%PDF-1.4"), 0o644)
	uuidPathIndex.Store(uuid, "Author/Book (1)")
	formatCache.Delete(uuid)
	defer uuidPathIndex.Delete(uuid)
	defer formatCache.Delete(uuid)

	call := func(method, body string) isbnResponse {
		t.Helper()
		req := httptest.NewRequest(method, "/metadata/"+uuid+"/isbns", strings.NewReader(body))
		req.SetPathValue("uuid", uuid)
		rec := httptest.NewRecorder()
		isbnCandidatesHandler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body)
		}
		var resp isbnResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}

	got := call("GET", "")
	if got.PDF != "Book.pdf" || len(got.Candidates) != 2 ||
		got.Candidates[0] != (isbnCandidate{"9780306406157", "Book.azw3", "exth", ""}) ||
		got.Candidates[1] != (isbnCandidate{"9780134190440", "Book.epub", "text", "hardcover"}) {
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
