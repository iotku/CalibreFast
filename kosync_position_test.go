package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// testEPUB builds an EPUB whose spine is ch1, an itemref missing from the
// manifest (crengine makes no DocFragment for it), ch2 and ch3.
func testEPUB(t *testing.T) []byte {
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

func writeTestEPUB(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(path, testEPUB(t), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseXPointer(t *testing.T) {
	for xp, want := range map[string]struct {
		frag  int
		steps int
	}{
		"/body/DocFragment[11]/body/div/p[7]/text().123":             {11, 3},
		"/body[1]/DocFragment[11]/body[1]/div[1]/p[7]/text()[1].123": {11, 3},
		"/body/DocFragment[20]/body/p[22]/img.0":                     {20, 3},
		"/body/DocFragment[3]":                                       {3, 0},
		"/body/DocFragment[3].0":                                     {3, 0},
		"/body/DocFragment[3]/body/div/text()[2].5/p":                {3, 2},
	} {
		frag, steps, ok := parseXPointer(xp)
		if !ok || frag != want.frag || len(steps) != want.steps {
			t.Errorf("%s: %d %v %v", xp, frag, steps, ok)
		}
	}
	for _, bad := range []string{"", "56", "/html/body", "/body/DocFragment[0]", "/body/Fragment[2]/p"} {
		if _, _, ok := parseXPointer(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestXPointerToCFI(t *testing.T) {
	path := writeTestEPUB(t)
	for xp, want := range map[string]string{
		// DocFragment[2] is ch2: the third itemref, since the second has no
		// manifest entry. In ch2, <body> is /4, its <div> /4, and the third
		// <p> is the div's fourth element: /8.
		"/body/DocFragment[2]/body/div/p[3]/text().5":             "epubcfi(/6/6!/4/4/8)",
		"/body[1]/DocFragment[2]/body[1]/div[1]/p[3]/text()[1].5": "epubcfi(/6/6!/4/4/8)",
		"/body/DocFragment[2]/body/div/p[3]/img.0":                "epubcfi(/6/6!/4/4/8/2)",
		"/body/DocFragment[2]/body/div/blockquote/p":              "epubcfi(/6/6!/4/4/6/2)",
		"/body/DocFragment[3]/body/section/p[2]":                  "epubcfi(/6/8!/4/2/4)",
		"/body/DocFragment[1]":                                    "epubcfi(/6/2!/4)",
		// A path that doesn't match the file keeps what resolved…
		"/body/DocFragment[2]/body/div/p[9]": "epubcfi(/6/6!/4/4)",
		// …or falls back to the chapter's start.
		"/body/DocFragment[2]/article/p": "epubcfi(/6/6!/4)",
	} {
		got, err := xpointerToCFI(path, xp)
		if err != nil || got != want {
			t.Errorf("%s → %s, %v; want %s", xp, got, err, want)
		}
	}
	for _, bad := range []string{"/body/DocFragment[4]", "12"} {
		if got, err := xpointerToCFI(path, bad); err == nil {
			t.Errorf("%s → %s", bad, got)
		}
	}
}

func TestCFIToXPointer(t *testing.T) {
	path := writeTestEPUB(t)
	for cfi, want := range map[string]string{
		"epubcfi(/6/6[c2ref]!/4/4[x]/8/1:3)": "/body/DocFragment[2]/body/div/p[3]",
		"epubcfi(/6/6!/4/4/2/1:0)":           "/body/DocFragment[2]/body/div/p[1]",
		"epubcfi(/6/6!/4/2[c2]/1:0)":         "/body/DocFragment[2]/body/h1",
		"epubcfi(/6/6!/4/4/6/2/1:2)":         "/body/DocFragment[2]/body/div/blockquote/p",
		"epubcfi(/6/8!/4/2/4/1:0)":           "/body/DocFragment[3]/body/section/p[2]",
		"epubcfi(/6/2!/4/2,/1:0,/1:3)":       "/body/DocFragment[1]/body/p",
		"epubcfi(/6/6!/4/4/40)":              "/body/DocFragment[2]/body/div",
		"epubcfi(/6/6!/2/2)":                 "/body/DocFragment[2]", // in <head>
		"epubcfi(/6/6!)":                     "/body/DocFragment[2]",
	} {
		got, err := cfiToXPointer(path, cfi)
		if err != nil || got != want {
			t.Errorf("%s → %s, %v; want %s", cfi, got, err, want)
		}
	}
	// The itemref missing from the manifest has no DocFragment.
	for _, bad := range []string{"epubcfi(/6/4!/4/2)", "epubcfi(/6/20!/4)", "56", "epubcfi(/6/7!/4)"} {
		if got, err := cfiToXPointer(path, bad); err == nil {
			t.Errorf("%s → %s", bad, got)
		}
	}
}

func TestPositionRoundTrip(t *testing.T) {
	path := writeTestEPUB(t)
	for _, xp := range []string{
		"/body/DocFragment[2]/body/div/p[4]",
		"/body/DocFragment[2]/body/h1",
		"/body/DocFragment[3]/body/section/p[1]",
	} {
		cfi, err := xpointerToCFI(path, xp)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := cfiToXPointer(path, cfi); err != nil || back != xp {
			t.Errorf("%s → %s → %s, %v", xp, cfi, back, err)
		}
	}
}

// With KOREADER_TEST_DATA set (see TestPartialMD5KOReaderFixtures), convert
// a position in KOReader's leaves.epub both ways.
func TestPositionKOReaderFixture(t *testing.T) {
	dir := os.Getenv("KOREADER_TEST_DATA")
	if dir == "" {
		t.Skip("KOREADER_TEST_DATA not set")
	}
	path := filepath.Join(dir, "leaves.epub")
	// Chapter 11's <body> holds h2, div, p, div; the first div holds <br>s.
	xp := "/body/DocFragment[11]/body/div[1]/br[7].0"
	cfi, err := xpointerToCFI(path, xp)
	if err != nil || cfi != "epubcfi(/6/22!/4/4/14)" {
		t.Fatalf("%s → %s, %v", xp, cfi, err)
	}
	if back, err := cfiToXPointer(path, cfi); err != nil || back != "/body/DocFragment[11]/body/div[1]/br[7]" {
		t.Errorf("back: %s, %v", back, err)
	}
}
