package main

import (
	"archive/zip"
	"bytes"
	"testing"
)

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

func TestExtractEPUBMetaXML11(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.1"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"OEBPS/content.opf": opf11,
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
	meta, err := extractEPUBMeta(bytes.NewReader(buf.Bytes()), int64(buf.Len()), "x.epub")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Eleven" {
		t.Errorf("title %q", meta.Title)
	}
}
