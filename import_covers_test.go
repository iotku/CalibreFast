package main

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"testing"

	pdfapi "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
)

// testPNG is a w×h PNG of one color.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{200, 40, 40, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// coverEPUB is an EPUB whose OPF (at OEBPS/content.opf) has the given
// metadata and manifest, with an image at OEBPS/images/c.png.
func coverEPUB(t *testing.T, meta, manifest string, img []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	add("mimetype", []byte("application/epub+zip"))
	add("META-INF/container.xml", []byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`))
	add("OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>T</dc:title>`+meta+`</metadata>
  <manifest>`+manifest+`</manifest>
</package>`))
	add("OEBPS/images/c.png", img)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestEPUBCoverHref(t *testing.T) {
	cases := []struct{ name, meta, manifest, want string }{
		{"epub3", ``, `<item id="x" href="images/c.png" media-type="image/png" properties="cover-image"/>`, "OEBPS/images/c.png"},
		{"epub2", `<meta name="cover" content="img1"/>`, `<item id="img1" href="images/c.png" media-type="image/png"/>`, "OEBPS/images/c.png"},
		{"id cover", ``, `<item id="cover" href="images/c.png" media-type="image/png"/>`, "OEBPS/images/c.png"},
		{"relative", ``, `<item id="x" href="../Images/My%20c.png#frag" media-type="image/png" properties="cover-image"/>`, "Images/My c.png"},
		{"not an image", ``, `<item id="cover" href="cover.xhtml" media-type="application/xhtml+xml"/>`, ""},
		{"none", ``, `<item id="x" href="images/c.png" media-type="image/png"/>`, ""},
	}
	for _, c := range cases {
		opf := `<package xmlns="http://www.idpf.org/2007/opf"><metadata>` + c.meta + `</metadata><manifest>` + c.manifest + `</manifest></package>`
		got, err := epubCoverHref("OEBPS/content.opf", []byte(opf))
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: got %q, want no cover", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestPDFImageIsPage(t *testing.T) {
	cases := []struct {
		w, h         int
		pageW, pageH float64
		want         bool
	}{
		{1240, 1754, 595, 842, true},  // A4 scan
		{1754, 1240, 595, 842, true},  // turned with /Rotate
		{1200, 1200, 595, 842, false}, // a square picture on the page
		{200, 283, 595, 842, false},   // a logo of the page's shape
		{1650, 2550, 612, 792, false}, // a taller picture than the Letter page (0.65 vs 0.77)
	}
	for _, c := range cases {
		if got := pdfImageIsPage(c.w, c.h, c.pageW, c.pageH); got != c.want {
			t.Errorf("%dx%d on %vx%v: got %v, want %v", c.w, c.h, c.pageW, c.pageH, got, c.want)
		}
	}
}

func TestExtractImportCoverEPUB(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "b.epub"), coverEPUB(t, ``,
		`<item id="x" href="images/c.png" media-type="image/png" properties="cover-image"/>`, testPNG(t, 1600, 2400)))
	files := []importFile{{Name: "b.epub", Format: "epub"}}
	if !extractImportCover(dir, files) {
		t.Fatal("no cover taken from the EPUB")
	}
	f, err := os.Open(filepath.Join(dir, "cover.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(f)
	f.Close()
	// A PNG is encoded as JPEG once, at its own size.
	if err != nil || format != "jpeg" || cfg.Width != 1600 || cfg.Height != 2400 {
		t.Fatalf("cover.jpg: %s %dx%d, %v; want a jpeg 1600x2400", format, cfg.Width, cfg.Height, err)
	}

	// A cover already there (chosen by the admin) is kept.
	writeTestFile(t, filepath.Join(dir, "cover.jpg"), []byte("chosen"))
	if !extractImportCover(dir, files) {
		t.Fatal("a book with a cover reported none")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "cover.jpg")); string(data) != "chosen" {
		t.Fatal("the chosen cover was replaced")
	}
}

func TestExtractImportCoverNone(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "b.epub"), coverEPUB(t, ``,
		`<item id="x" href="images/c.png" media-type="image/png"/>`, testPNG(t, 10, 10)))
	if extractImportCover(dir, []importFile{{Name: "b.epub", Format: "epub"}}) {
		t.Fatal("a cover from an EPUB that declares none")
	}
	if _, err := os.Stat(filepath.Join(dir, "cover.jpg")); err == nil {
		t.Fatal("cover.jpg written")
	}
}

// imagePDF is a one-page A4 PDF with img placed on it (pdfcpu's image import).
func imagePDF(t *testing.T, img []byte, full bool) []byte {
	t.Helper()
	imp := pdfcpu.DefaultImportConfig()
	if !full {
		imp.Pos, imp.Scale = 1, 0.2 // centered, small (types.Center)
	}
	var buf bytes.Buffer
	if err := pdfapi.ImportImages(context.Background(), nil, &buf, []io.Reader{bytes.NewReader(img)}, imp, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractImportCoverPDF(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "scan.pdf"), imagePDF(t, testPNG(t, 850, 1202), true)) // A4's shape
	if !extractImportCover(dir, []importFile{{Name: "scan.pdf", Format: "pdf"}}) {
		t.Fatal("no cover from a PDF whose first page is a picture")
	}

	dir = t.TempDir()
	writeTestFile(t, filepath.Join(dir, "logo.pdf"), imagePDF(t, testPNG(t, 300, 300), false))
	if extractImportCover(dir, []importFile{{Name: "logo.pdf", Format: "pdf"}}) {
		t.Fatal("a small square picture taken as the page")
	}
}

func TestExtractImportCoverKeepsJPEG(t *testing.T) {
	// The EPUB's own JPEG becomes cover.jpg byte for byte.
	dir := t.TempDir()
	orig := testJPEG(t, 1600, 2400, 85)
	writeTestFile(t, filepath.Join(dir, "b.epub"), coverEPUB(t, ``,
		`<item id="x" href="images/c.png" media-type="image/jpeg" properties="cover-image"/>`, orig)) // a JPEG under the helper's name
	if !extractImportCover(dir, []importFile{{Name: "b.epub", Format: "epub"}}) {
		t.Fatal("no cover taken from the EPUB")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "cover.jpg")); !bytes.Equal(got, orig) {
		t.Errorf("cover.jpg was re-encoded: %d bytes, the EPUB's %d", len(got), len(orig))
	}
}
