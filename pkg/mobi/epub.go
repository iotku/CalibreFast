package mobi

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Version changes whenever the EPUB made from the same book changes (a
// fix or improvement in the conversion), so a cache of converted books
// knows to make them again.
const Version = 2

// epochTime stamps every file in the archive, so the same book always
// converts to the same bytes.
var epochTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// WriteEPUB converts the book to an EPUB 3 (with an EPUB 2 NCX too) and
// writes it to w.
func (b *Book) WriteEPUB(w io.Writer) error {
	res := b.resources()
	var c *content
	var err error
	if b.KF8 {
		c, err = b.kf8Content(res)
	} else {
		c, err = b.mobi6Content(res)
	}
	if err != nil {
		return err
	}
	return b.writeEPUB(w, c, res)
}

// ConvertFile converts the book at src to an EPUB at dst. dst is written
// next to itself and renamed into place, so it never holds half a book.
func ConvertFile(src, dst string, lim Limits) error {
	b, err := OpenFile(src, lim)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".mobi-*.epub")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := b.WriteEPUB(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (b *Book) identifier() string {
	sum := sha1.Sum(b.p.data)
	sum[6] = sum[6]&0x0F | 0x50 // a version 5 (name-based, SHA-1) UUID
	sum[8] = sum[8]&0x3F | 0x80
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

func (b *Book) writeEPUB(w io.Writer, c *content, res map[int]*resource) error {
	z := zip.NewWriter(w)
	put := func(name string, data []byte, store bool) error {
		h := &zip.FileHeader{Name: name, Modified: epochTime, Method: zip.Deflate}
		if store {
			h.Method = zip.Store
		}
		f, err := z.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	if err := put("mimetype", []byte("application/epub+zip"), true); err != nil {
		return err
	}
	if err := put("META-INF/container.xml", []byte(containerXML), false); err != nil {
		return err
	}
	cover := b.coverNumber(res)
	if err := put("OEBPS/content.opf", b.opf(c, res, cover), false); err != nil {
		return err
	}
	if err := put("OEBPS/nav.xhtml", b.nav(c), false); err != nil {
		return err
	}
	if err := put("OEBPS/toc.ncx", b.ncx(c), false); err != nil {
		return err
	}
	for _, d := range c.docs {
		if err := put("OEBPS/"+d.name, d.data, false); err != nil {
			return err
		}
	}
	for _, f := range c.files {
		if err := put("OEBPS/"+f.name, f.data, false); err != nil {
			return err
		}
	}
	for _, n := range sortedKeys(res) {
		r := res[n]
		if err := put("OEBPS/"+r.name, r.data, strings.HasPrefix(r.mediaType, "image/")); err != nil {
			return err
		}
	}
	return z.Close()
}

func sortedKeys(m map[int]*resource) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

const containerXML = `<?xml version="1.0" encoding="utf-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`

func xmlText(s string) string {
	var b bytes.Buffer
	escape(&b, cleanText(s), true)
	return b.String()
}

func (b *Book) opf(c *content, res map[int]*resource, cover int) []byte {
	m := b.Metadata
	var o strings.Builder
	o.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
`)
	fmt.Fprintf(&o, "<dc:identifier id=\"bookid\">%s</dc:identifier>\n", b.identifier())
	title := m.Title
	if title == "" {
		title = "Untitled"
	}
	fmt.Fprintf(&o, "<dc:title>%s</dc:title>\n", xmlText(title))
	lang := m.Language
	if lang == "" || !validName(lang) {
		lang = "und"
	}
	fmt.Fprintf(&o, "<dc:language>%s</dc:language>\n", xmlText(lang))
	for _, a := range m.Authors {
		fmt.Fprintf(&o, "<dc:creator>%s</dc:creator>\n", xmlText(a))
	}
	if m.Publisher != "" {
		fmt.Fprintf(&o, "<dc:publisher>%s</dc:publisher>\n", xmlText(m.Publisher))
	}
	if m.Description != "" {
		fmt.Fprintf(&o, "<dc:description>%s</dc:description>\n", xmlText(m.Description))
	}
	if m.Date != "" {
		fmt.Fprintf(&o, "<dc:date>%s</dc:date>\n", xmlText(m.Date))
	}
	for _, s := range m.Subjects {
		fmt.Fprintf(&o, "<dc:subject>%s</dc:subject>\n", xmlText(s))
	}
	if m.ISBN != "" {
		fmt.Fprintf(&o, "<dc:identifier>urn:isbn:%s</dc:identifier>\n", xmlText(m.ISBN))
	}
	if m.ASIN != "" {
		fmt.Fprintf(&o, "<dc:identifier>urn:asin:%s</dc:identifier>\n", xmlText(m.ASIN))
	}
	fmt.Fprintf(&o, "<meta property=\"dcterms:modified\">%s</meta>\n", epochTime.Format("2006-01-02T15:04:05Z"))
	if cover > 0 {
		fmt.Fprintf(&o, "<meta name=\"cover\" content=\"r%d\"/>\n", cover)
	}
	o.WriteString("</metadata>\n<manifest>\n")
	o.WriteString(`<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>` + "\n")
	o.WriteString(`<item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>` + "\n")
	for i, d := range c.docs {
		props := ""
		if bytes.Contains(d.data, []byte("<svg")) {
			props = ` properties="svg"`
		}
		fmt.Fprintf(&o, "<item id=\"d%d\" href=\"%s\" media-type=\"application/xhtml+xml\"%s/>\n", i, d.name, props)
	}
	for i, f := range c.files {
		fmt.Fprintf(&o, "<item id=\"f%d\" href=\"%s\" media-type=\"%s\"/>\n", i, f.name, f.mediaType)
	}
	for _, n := range sortedKeys(res) {
		r := res[n]
		props := ""
		if n == cover {
			props = ` properties="cover-image"`
		}
		fmt.Fprintf(&o, "<item id=\"r%d\" href=\"%s\" media-type=\"%s\"%s/>\n", n, r.name, r.mediaType, props)
	}
	o.WriteString("</manifest>\n<spine toc=\"ncx\"")
	if m.RTL {
		o.WriteString(` page-progression-direction="rtl"`)
	}
	o.WriteString(">\n")
	for i := range c.docs {
		fmt.Fprintf(&o, "<itemref idref=\"d%d\"/>\n", i)
	}
	o.WriteString("</spine>\n</package>\n")
	return []byte(o.String())
}

// tocTree turns the flat, levelled TOC into nesting depths that never
// skip a level (a hostile book may jump from 0 to 9).
func tocDepths(toc []tocEntry) []int {
	depths := make([]int, len(toc))
	prev := -1
	for i, e := range toc {
		d := max(0, min(e.level, prev+1, 16))
		depths[i] = d
		prev = d
	}
	return depths
}

func (b *Book) toc(c *content) []tocEntry {
	if len(c.toc) > 0 {
		return c.toc
	}
	if len(c.docs) == 0 {
		return nil
	}
	title := b.Metadata.Title
	if title == "" {
		title = "Start"
	}
	return []tocEntry{{label: title, href: c.docs[0].name}}
}

func (b *Book) nav(c *content) []byte {
	toc := b.toc(c)
	depths := tocDepths(toc)
	var o strings.Builder
	o.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
<head><title>` + xmlText(b.Metadata.Title) + `</title></head>
<body>
<nav epub:type="toc" id="toc">
<ol>`)
	cur := 0
	for i, e := range toc {
		d := depths[i]
		switch {
		case i == 0:
		case d > cur:
			o.WriteString("<ol>")
		default:
			o.WriteString("</li>")
			for ; cur > d; cur-- {
				o.WriteString("</ol></li>")
			}
		}
		cur = d
		fmt.Fprintf(&o, "\n<li><a href=\"%s\">%s</a>", xmlText(e.href), xmlText(e.label))
	}
	if len(toc) > 0 {
		o.WriteString("</li>")
		for ; cur > 0; cur-- {
			o.WriteString("</ol></li>")
		}
	}
	o.WriteString("\n</ol>\n</nav>\n</body>\n</html>\n")
	return []byte(o.String())
}

func (b *Book) ncx(c *content) []byte {
	toc := b.toc(c)
	depths := tocDepths(toc)
	var o strings.Builder
	o.WriteString(`<?xml version="1.0" encoding="utf-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
<head><meta name="dtb:uid" content="` + b.identifier() + `"/></head>
<docTitle><text>` + xmlText(b.Metadata.Title) + `</text></docTitle>
<navMap>`)
	cur := -1
	for i, e := range toc {
		d := depths[i]
		for ; cur >= d; cur-- {
			o.WriteString("</navPoint>")
		}
		cur = d
		fmt.Fprintf(&o, "\n<navPoint id=\"n%d\" playOrder=\"%d\"><navLabel><text>%s</text></navLabel><content src=\"%s\"/>",
			i+1, i+1, xmlText(e.label), xmlText(e.href))
	}
	for ; cur >= 0; cur-- {
		o.WriteString("</navPoint>")
	}
	o.WriteString("\n</navMap>\n</ncx>\n")
	return []byte(o.String())
}
