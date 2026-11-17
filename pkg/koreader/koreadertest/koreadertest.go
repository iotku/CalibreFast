// Package koreadertest has EPUB fixtures for testing code that converts
// KOReader positions and annotations.
package koreadertest

import (
	"archive/zip"
	"bytes"
	"testing"
)

// EPUB builds an EPUB whose spine is ch1, an itemref missing from the
// manifest (crengine makes no DocFragment for it), ch2 and ch3.
func EPUB(t testing.TB) []byte {
	t.Helper()
	files := []struct{ name, body string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"OEBPS/content.opf", `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>T</dc:title></metadata>
  <manifest>
    <item id="c1" href="Text/ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="Text/ch%202.xhtml" media-type="application/xhtml+xml"/>
    <item id="c3" href="Text/ch3.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine toc="ncx">
    <itemref idref="c1"/>
    <itemref idref="gone"/>
    <itemref idref="c2"/>
    <itemref idref="c3"/>
  </spine>
</package>`},
		{"OEBPS/Text/ch1.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>1</title></head><body><p>One</p></body></html>`},
		{"OEBPS/Text/ch 2.xhtml", `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
<head><title>Two</title><link rel="stylesheet" href="s.css"/></head>
<body>
  <h1 id="c2">Chapter&nbsp;2</h1>
  <div class="text">
    <p>first</p>
    <p>second<br/>line</p>
    <blockquote><p>quoted</p></blockquote>
    <p>third <img src="x.png" alt=""/> &mdash; end</p>
    <!-- a comment -->
    <p>fourth</p>
  </div>
</body>
</html>`},
		{"OEBPS/Text/ch3.xhtml", `<html xmlns="http://www.w3.org/1999/xhtml"><head></head><body><section><p>a</p><p>b</p></section></body></html>`},
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(f.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// KOChapter is a chapter with what makes the two readers count differently: runs of
// white space, white space at the start of blocks, text split by inline
// elements, an entity and a character outside the BMP.
const KOChapter = `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>t</title></head><body>
<div>
  <p>Alpha   beta
 gamma.</p>
  <p>One <b>two</b> three &amp; four 😀 five.</p>
</div>
<p> Lead <i>x</i> <i>y</i> tail</p>
</body></html>`

// KOEPUB is a two-chapter EPUB 3 whose second chapter is KOChapter.
func KOEPUB(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, f := range [][2]string{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`},
		// An EPUB 3 package: <meta> with text inside.
		{"content.opf", `<package xmlns="http://www.idpf.org/2007/opf" version="3.0"><metadata><meta property="dcterms:modified">2026-01-01T00:00:00Z</meta></metadata><manifest>
			<item id="a" href="a.xhtml"/><item id="b" href="b.xhtml"/></manifest>
			<spine><itemref idref="a"/><itemref idref="b"/></spine></package>`},
		{"a.xhtml", `<html><head/><body><p>First chapter.</p></body></html>`},
		{"b.xhtml", KOChapter},
	} {
		w, _ := z.Create(f[0])
		_, _ = w.Write([]byte(f[1]))
	}
	z.Close()
	return buf.Bytes()
}
