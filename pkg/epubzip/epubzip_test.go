package epubzip

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

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
		got, err := CoverHref("OEBPS/content.opf", []byte(opf))
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

// opf11 is an EPUB 2 package document declared as XML 1.1.
const opf11 = `<?xml version="1.1" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Eleven&#x1B;</dc:title>
    <dc:creator>A. Writer</dc:creator>
    <meta name="cover" content="cov"/>
  </metadata>
  <manifest>
    <item id="cov" href="images/c.png" media-type="image/png"/>
  </manifest>
</package>`

func TestEPUBCoverHrefXML11(t *testing.T) {
	got, err := CoverHref("OEBPS/content.opf", []byte(opf11))
	if err != nil {
		t.Fatal(err)
	}
	if got != "OEBPS/images/c.png" {
		t.Errorf("got %q", got)
	}
}

func TestPackagePathAndCoverData(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"OEBPS/content.opf":  opf11,
		"OEBPS/images/c.png": "PNGDATA",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "b.epub")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := PackagePath(zr); err != nil || got != "OEBPS/content.opf" {
		t.Errorf("PackagePath = %q, %v", got, err)
	}
	if _, err := ReadEntry(zr, "OEBPS/images/c.png", 3); err == nil {
		t.Error("ReadEntry: want an error for an entry over max")
	}
	if got, err := CoverData(p, 1<<20); err != nil || string(got) != "PNGDATA" {
		t.Errorf("CoverData = %q, %v", got, err)
	}
	if got := HrefPath("OEBPS/content.opf", "../Text/a%20b.xhtml#x"); got != "Text/a b.xhtml" {
		t.Errorf("HrefPath = %q", got)
	}
}
