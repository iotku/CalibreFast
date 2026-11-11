package main

// Converting EPUB reading positions between KOReader and the web reader.
//
// KOReader (crengine) points into an EPUB with an XPointer into its own DOM,
// which puts each spine item in a <DocFragment>:
//
//	/body/DocFragment[11]/body/div/p[7]/text().123
//
// The web reader (epub.js) uses an EPUB CFI:
//
//	epubcfi(/6/22!/4/2/14/1:123)
//
// DocFragment[N] is the N-th spine item (crengine makes one for every spine
// itemref found in the manifest), so the chapter always converts. Inside the
// chapter the element path is followed in the chapter's XHTML: when every
// step resolves, the position converts to that element (usually a
// paragraph); otherwise to the start of the chapter. Character offsets are
// dropped, since crengine and browsers don't count text the same way.

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// epubBook is the reading order of an EPUB file.
type epubBook struct {
	zr        *zip.ReadCloser
	spineStep int      // CFI step of <spine> within <package> (6 when it is the third element)
	itemrefs  []string // zip path of each spine item, "" when its idref isn't in the manifest
}

// xnode is an element of a parsed document. Text isn't kept, and attributes
// only for the package document.
type xnode struct {
	name     string
	attrs    map[string]string
	children []*xnode
}

const maxContentDoc = 32 << 20 // larger chapters aren't parsed

func openEPUB(file string) (*epubBook, error) {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil, err
	}
	b := &epubBook{zr: zr}
	if err := b.readPackage(); err != nil {
		zr.Close()
		return nil, err
	}
	return b, nil
}

func (b *epubBook) Close() error { return b.zr.Close() }

func (b *epubBook) readFile(name string) ([]byte, error) {
	f, err := b.zr.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxContentDoc+1))
	if err == nil && len(data) > maxContentDoc {
		err = errors.New("file too large")
	}
	return data, err
}

func (b *epubBook) readPackage() error {
	data, err := b.readFile("META-INF/container.xml")
	if err != nil {
		return err
	}
	var container struct {
		Rootfiles []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if err := xmlUnmarshal(data, &container); err != nil || len(container.Rootfiles) == 0 {
		return errors.New("no package document")
	}
	opfPath := container.Rootfiles[0].Path
	if data, err = b.readFile(opfPath); err != nil {
		return err
	}
	root, err := parseXML(data, true)
	if err != nil {
		return err
	}
	manifest := map[string]string{}
	var spine *xnode
	for i, el := range root.children {
		switch el.name {
		case "manifest":
			for _, item := range el.children {
				if item.name == "item" {
					href, _ := url.PathUnescape(item.attrs["href"])
					manifest[item.attrs["id"]] = path.Join(path.Dir(opfPath), href)
				}
			}
		case "spine":
			spine = el
			b.spineStep = (i + 1) * 2
		}
	}
	if spine == nil {
		return errors.New("no spine")
	}
	for _, ref := range spine.children {
		if ref.name == "itemref" {
			b.itemrefs = append(b.itemrefs, manifest[ref.attrs["idref"]])
		}
	}
	return nil
}

// itemForFragment is the spine itemref (0-based) of KOReader's DocFragment[n].
func (b *epubBook) itemForFragment(n int) (int, bool) {
	for i, href := range b.itemrefs {
		if href == "" {
			continue
		}
		if n--; n == 0 {
			return i, true
		}
	}
	return 0, false
}

// fragmentForItem is KOReader's DocFragment number of spine itemref i.
func (b *epubBook) fragmentForItem(i int) (int, bool) {
	if i < 0 || i >= len(b.itemrefs) || b.itemrefs[i] == "" {
		return 0, false
	}
	n := 0
	for _, href := range b.itemrefs[:i+1] {
		if href != "" {
			n++
		}
	}
	return n, true
}

// content parses spine item i into its element tree; the root is <html>.
func (b *epubBook) content(i int) (*xnode, error) {
	data, err := b.readFile(b.itemrefs[i])
	if err != nil {
		return nil, err
	}
	return parseXML(data, false)
}

// parseXML builds the element tree of an XML (or XHTML) document. Names are
// local and lower-case, as crengine names elements in its XPointers.
// keepAttrs is for the package document: attributes are kept, and it is read
// as XML proper.
func parseXML(data []byte, keepAttrs bool) (*xnode, error) {
	d := xml.NewDecoder(bytes.NewReader(xml11To10(data)))
	d.Strict = false
	if !keepAttrs {
		// Content documents may leave <br> or <img> open, as HTML does. The
		// package document is plain XML, and an EPUB 3 one has <meta> with
		// text inside, which this would close too early.
		d.AutoClose = xml.HTMLAutoClose
	}
	d.Entity = xml.HTMLEntity
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil } // only tags matter
	var root *xnode
	var stack []*xnode
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{name: strings.ToLower(t.Name.Local)}
			if keepAttrs {
				n.attrs = map[string]string{}
				for _, a := range t.Attr {
					n.attrs[a.Name.Local] = a.Value
				}
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("more than one root element")
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if root == nil {
		return nil, errors.New("empty document")
	}
	return root, nil
}

// ---------------------------------------------------------------------------
// XPointer → CFI
// ---------------------------------------------------------------------------

type xpStep struct {
	name  string
	index int // 1-based among same-name siblings
}

var (
	xpPointOffset = regexp.MustCompile(`\.\d+$`)
	xpFragment    = regexp.MustCompile(`^DocFragment\[(\d+)\]$`)
	xpElement     = regexp.MustCompile(`^([A-Za-z_][\w.:-]*?)(?:\[(\d+)\])?$`)
)

// parseXPointer reads crengine's DocFragment number and the element steps
// below it, up to any text() step. Both "p" and "p[1]" forms are accepted
// (newer KOReader versions always write the index).
func parseXPointer(xp string) (fragment int, steps []xpStep, ok bool) {
	parts := strings.Split(xpPointOffset.ReplaceAllString(xp, ""), "/")
	if len(parts) < 3 || parts[0] != "" || (parts[1] != "body" && parts[1] != "body[1]") {
		return 0, nil, false
	}
	m := xpFragment.FindStringSubmatch(parts[2])
	if m == nil {
		return 0, nil, false
	}
	fragment, _ = strconv.Atoi(m[1])
	if fragment < 1 {
		return 0, nil, false
	}
	for _, p := range parts[3:] {
		m := xpElement.FindStringSubmatch(p)
		if m == nil {
			break // text() or something unknown: stop at the element
		}
		idx := 1
		if m[2] != "" {
			idx, _ = strconv.Atoi(m[2])
		}
		steps = append(steps, xpStep{strings.ToLower(m[1]), idx})
	}
	return fragment, steps, true
}

// xpointerToCFI converts a KOReader position in an EPUB file to an EPUB CFI.
func xpointerToCFI(file, xp string) (string, error) {
	fragment, steps, ok := parseXPointer(xp)
	if !ok {
		return "", fmt.Errorf("not a crengine XPointer: %q", xp)
	}
	book, err := openEPUB(file)
	if err != nil {
		return "", err
	}
	defer book.Close()
	item, ok := book.itemForFragment(fragment)
	if !ok {
		return "", fmt.Errorf("no spine item for DocFragment[%d]", fragment)
	}
	base := fmt.Sprintf("/%d/%d", book.spineStep, (item+1)*2)
	chapterStart := "epubcfi(" + base + "!/4)" // <body>, after <head>
	html, err := book.content(item)
	if err != nil {
		return chapterStart, nil
	}
	var cfi strings.Builder
	node := html
	for _, s := range steps {
		next, pos := -1, 0
		for i, c := range node.children {
			if c.name == s.name {
				if pos++; pos == s.index {
					next = i
					break
				}
			}
		}
		if next < 0 {
			break // doesn't match this file: keep what resolved
		}
		fmt.Fprintf(&cfi, "/%d", (next+1)*2)
		node = node.children[next]
	}
	if cfi.Len() == 0 {
		return chapterStart, nil
	}
	return "epubcfi(" + base + "!" + cfi.String() + ")", nil
}

// ---------------------------------------------------------------------------
// CFI → XPointer
// ---------------------------------------------------------------------------

var cfiStep = regexp.MustCompile(`^(\d+)`)

// parseCFI reads the spine itemref (0-based) of an EPUB CFI and the element
// steps (0-based among element children) below the content document's root,
// up to any text step. Assertions and offsets are ignored; a range CFI is
// read as its start.
func parseCFI(cfi string) (item int, steps []int, ok bool) {
	s := strings.TrimSuffix(strings.TrimPrefix(cfi, "epubcfi("), ")")
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	base, rest, found := strings.Cut(s, "!")
	if !found {
		return 0, nil, false
	}
	b := strings.Split(base, "/")
	if len(b) != 3 || b[0] != "" {
		return 0, nil, false
	}
	m := cfiStep.FindStringSubmatch(b[2])
	if m == nil {
		return 0, nil, false
	}
	n, _ := strconv.Atoi(m[1])
	if n < 2 || n%2 != 0 {
		return 0, nil, false
	}
	for _, p := range strings.Split(rest, "/")[1:] {
		m := cfiStep.FindStringSubmatch(p)
		if m == nil {
			break
		}
		k, _ := strconv.Atoi(m[1])
		if k%2 != 0 || k == 0 {
			break // a text node: stop at its element
		}
		steps = append(steps, k/2-1)
	}
	return n/2 - 1, steps, true
}

// cfiToXPointer converts a web reader position in an EPUB file to a
// KOReader XPointer.
func cfiToXPointer(file, cfi string) (string, error) {
	item, steps, ok := parseCFI(cfi)
	if !ok {
		return "", fmt.Errorf("not an EPUB CFI: %q", cfi)
	}
	book, err := openEPUB(file)
	if err != nil {
		return "", err
	}
	defer book.Close()
	fragment, ok := book.fragmentForItem(item)
	if !ok {
		return "", fmt.Errorf("no DocFragment for spine item %d", item)
	}
	chapterStart := fmt.Sprintf("/body/DocFragment[%d]", fragment)
	html, err := book.content(item)
	if err != nil {
		return chapterStart, nil
	}
	var segs []string
	node := html
	for _, idx := range steps {
		if idx >= len(node.children) {
			break
		}
		child := node.children[idx]
		pos, count := 0, 0
		for _, c := range node.children {
			if c.name == child.name {
				count++
				if c == child {
					pos = count
				}
			}
		}
		if count > 1 {
			segs = append(segs, fmt.Sprintf("%s[%d]", child.name, pos))
		} else {
			segs = append(segs, child.name)
		}
		node = child
	}
	// crengine keeps only <body> of each chapter.
	if len(segs) == 0 || segs[0] != "body" {
		return chapterStart, nil
	}
	return chapterStart + "/" + strings.Join(segs, "/"), nil
}
