package main

// Exact text positions in an EPUB chapter, in the two forms the readers use.
//
// kosync_position.go converts reading positions, where the paragraph is
// enough. A highlight needs its first and last character, and the two
// readers count characters differently:
//
//   - The web reader (epub.js) uses an EPUB CFI: "/4/2/6/1:42" is a path of
//     child steps (even: the n/2-th element; odd: the text between two
//     elements) and an offset in UTF-16 units into that text, exactly as it
//     is in the file.
//   - KOReader (crengine) uses an XPointer: "/body/DocFragment[3]/body/p[2]/text()[2].17"
//     names elements, then a text node, then an offset in characters into
//     the text as crengine stores it. crengine turns line breaks and tabs
//     into spaces, collapses runs of spaces into one, and drops white space
//     that opens a block element (lvxml.cpp: PreProcessXmlString;
//     lvtinydom.cpp: ldomElementWriter::onText).
//
// A chapter is parsed once into elements and text; a place in it is a text
// node and an offset in characters of the text as written (textPoint).

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// tnode is an element (name set) or a run of text.
type tnode struct {
	name     string
	text     []rune
	parent   *tnode
	children []*tnode
	order    int // text nodes: place in reading order
}

func (n *tnode) isText() bool { return n.name == "" }

// chapter is a parsed content document.
type chapter struct {
	root  *tnode   // <html>
	texts []*tnode // every text node, in reading order
}

// textPoint is a place in a chapter: before character off of a text node.
type textPoint struct {
	node *tnode
	off  int
}

func parseChapter(data []byte) (*chapter, error) {
	d := xml.NewDecoder(bytes.NewReader(xml11To10(data)))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	c := &chapter{}
	var stack []*tnode
	joinable := false // the last child is text that more text continues
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
			n := &tnode{name: strings.ToLower(t.Name.Local)}
			if len(stack) == 0 {
				if c.root != nil {
					return nil, errors.New("more than one root element")
				}
				c.root = n
			} else {
				n.parent = stack[len(stack)-1]
				n.parent.children = append(n.parent.children, n)
			}
			stack = append(stack, n)
			joinable = false
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			joinable = false
		case xml.CharData:
			if len(stack) == 0 || len(t) == 0 {
				continue
			}
			p := stack[len(stack)-1]
			if joinable {
				last := p.children[len(p.children)-1]
				last.text = append(last.text, []rune(string(t))...)
				continue
			}
			n := &tnode{text: []rune(string(t)), parent: p, order: len(c.texts)}
			p.children = append(p.children, n)
			c.texts = append(c.texts, n)
			joinable = true
		case xml.Comment, xml.ProcInst:
			// Text on either side stays two runs, as it is for crengine; a
			// CFI counts through both (cfiGroup).
			joinable = false
		}
	}
	if c.root == nil {
		return nil, errors.New("empty document")
	}
	return c, nil
}

// ---- crengine's view of the text ---------------------------------------------

// creInline are the elements crengine lays out inline (or not at all) by
// default (fb2def.h); every other element is a block, at whose start white
// space is dropped.
var creInline = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`a abbr acronym b bdi bdo big blink br cite code data del dfn em embed font i img ins kbd
		label mark meter nobr object option picture progress q rb rbc rp rt rtc ruby s samp slot small spacer span strike
		strong sub sup svg time tt u var wbr style script meta link base basefont bgsound`) {
		creInline[n] = true
	}
}

// crePre are the elements whose text crengine keeps as written.
var crePre = map[string]bool{"pre": true, "textarea": true, "listing": true, "plaintext": true, "xmp": true}

func isCreSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// creText is a text node as crengine stores it, and for each offset into the
// text as written (0…len) the offset into that. ok is false when crengine has
// no node for it.
func creText(n *tnode) (text []rune, at []int, ok bool) {
	pre := false
	for p := n.parent; p != nil; p = p.parent {
		if crePre[p.name] {
			pre = true
			break
		}
	}
	at = make([]int, len(n.text)+1)
	blank := true
	spaces := 0
	for i, r := range n.text {
		at[i] = len(text)
		switch {
		case pre:
			if r == '\r' {
				continue
			}
			text = append(text, r)
		case isCreSpace(r):
			if spaces == 0 {
				text = append(text, ' ')
			}
			spaces++
		default:
			text = append(text, r)
			spaces = 0
		}
		if !isCreSpace(r) {
			blank = false
		}
	}
	at[len(n.text)] = len(text)
	if len(text) == 0 {
		return nil, at, false
	}
	if blank && !pre && !creInline[n.parent.name] {
		// White space that opens a block is dropped: nothing before it but
		// more of the same.
		first := true
		for _, sib := range n.parent.children {
			if sib == n {
				break
			}
			if !sib.isText() {
				first = false
				break
			}
			if _, _, exists := creText(sib); exists {
				first = false
				break
			}
		}
		if first {
			return nil, at, false
		}
	}
	return text, at, true
}

// creNode moves a point to a text node crengine has: itself, or the start of
// the next one.
func (c *chapter) creNode(p textPoint) (textPoint, bool) {
	if _, _, ok := creText(p.node); ok {
		return p, true
	}
	for _, n := range c.texts[p.node.order+1:] {
		if _, _, ok := creText(n); ok {
			return textPoint{n, 0}, true
		}
	}
	return p, false
}

// xpointer is the point as crengine writes it, in DocFragment number fragment.
func (c *chapter) xpointer(fragment int, p textPoint) (string, bool) {
	p, ok := c.creNode(p)
	if !ok {
		return "", false
	}
	_, at, _ := creText(p.node)
	off := at[min(max(p.off, 0), len(p.node.text))]

	// text()[k], among the parent's text nodes that crengine has.
	k, count := 0, 0
	for _, sib := range p.node.parent.children {
		if !sib.isText() {
			continue
		}
		if _, _, ok := creText(sib); ok {
			count++
			if sib == p.node {
				k = count
			}
		}
	}
	path := "/text()"
	if count > 1 {
		path = fmt.Sprintf("/text()[%d]", k)
	}
	path += "." + strconv.Itoa(off)

	// Elements up to <body>; an index only where there are several of a name,
	// as kosync_position.go writes them.
	n := p.node.parent
	for ; n != nil && n.parent != nil; n = n.parent {
		pos, same := 0, 0
		for _, sib := range n.parent.children {
			if sib.name == n.name {
				same++
				if sib == n {
					pos = same
				}
			}
		}
		if same > 1 {
			path = fmt.Sprintf("/%s[%d]", n.name, pos) + path
		} else {
			path = "/" + n.name + path
		}
		if n.parent == c.root {
			break
		}
	}
	// crengine keeps only <body> of each chapter.
	if n == nil || n.name != "body" {
		return "", false
	}
	return fmt.Sprintf("/body/DocFragment[%d]", fragment) + path, true
}

var (
	xpTextStep = regexp.MustCompile(`^text\(\)(?:\[(\d+)\])?$`)
	xpOffset   = regexp.MustCompile(`\.(\d+)$`)
)

// xpointerFragment is the DocFragment number of a crengine XPointer.
func xpointerFragment(xp string) (int, bool) {
	fragment, _, ok := parseXPointer(xp)
	return fragment, ok
}

// fromXPointer finds the point an XPointer names. exact is false when it
// only resolved as far as an element (the point is then that element's start).
func (c *chapter) fromXPointer(xp string) (p textPoint, exact, ok bool) {
	off := -1
	if m := xpOffset.FindStringSubmatch(xp); m != nil {
		off, _ = strconv.Atoi(m[1])
		xp = xpOffset.ReplaceAllString(xp, "")
	}
	parts := strings.Split(xp, "/")
	if len(parts) < 4 {
		return p, false, false
	}
	node := c.root
	for i, part := range parts[3:] {
		if m := xpTextStep.FindStringSubmatch(part); m != nil {
			k := 1
			if m[1] != "" {
				k, _ = strconv.Atoi(m[1])
			}
			for _, child := range node.children {
				if !child.isText() {
					continue
				}
				text, at, exists := creText(child)
				if !exists {
					continue
				}
				if k--; k > 0 {
					continue
				}
				// The offset as written: past any white space crengine
				// collapsed away just before it.
				raw := 0
				if off >= len(text) {
					raw = len(child.text)
				} else if off > 0 {
					for j, o := range at {
						if o > off {
							break
						}
						raw = j
					}
				}
				return textPoint{child, raw}, true, true
			}
			break // no such text node in this file
		}
		m := xpElement.FindStringSubmatch(part)
		if m == nil {
			break
		}
		idx := 1
		if m[2] != "" {
			idx, _ = strconv.Atoi(m[2])
		}
		var next *tnode
		for _, child := range node.children {
			if child.name == strings.ToLower(m[1]) {
				if idx--; idx == 0 {
					next = child
					break
				}
			}
		}
		if next == nil {
			if i == 0 {
				return p, false, false // not even <body>
			}
			break
		}
		node = next
	}
	start, ok := c.startOf(node)
	return start, false, ok
}

// startOf is the first text in an element, or failing that after it.
func (c *chapter) startOf(n *tnode) (textPoint, bool) {
	for cur := n; cur != nil; {
		if cur.isText() {
			return textPoint{cur, 0}, true
		}
		if len(cur.children) > 0 {
			cur = cur.children[0]
			continue
		}
		// Next in reading order: a following sibling, of this or an ancestor.
		for cur != nil {
			if cur.parent == nil {
				return textPoint{}, false
			}
			sibs := cur.parent.children
			i := 0
			for sibs[i] != cur {
				i++
			}
			if i+1 < len(sibs) {
				cur = sibs[i+1]
				break
			}
			cur = cur.parent
		}
	}
	return textPoint{}, false
}

// ---- EPUB CFI ------------------------------------------------------------------

var (
	cfiAssertion = regexp.MustCompile(`\[(?:\^.|[^\]^])*\]`)
	cfiPathStep  = regexp.MustCompile(`^(\d+)(?::(\d+))?`)
)

// splitCFI reads "epubcfi(/6/8!/4/2,/1:0,/1:42)" into the spine itemref
// (0-based) and the paths of its start and end inside the chapter (the same
// for a CFI that isn't a range). Assertions ("[id]") are dropped.
func splitCFI(cfi string) (item int, start, end string, ok bool) {
	s := strings.TrimSpace(cfi)
	if !strings.HasPrefix(s, "epubcfi(") || !strings.HasSuffix(s, ")") {
		return 0, "", "", false
	}
	s = cfiAssertion.ReplaceAllString(s[len("epubcfi("):len(s)-1], "")
	parts := strings.Split(s, ",")
	base, common, found := strings.Cut(parts[0], "!")
	if !found || (len(parts) != 1 && len(parts) != 3) {
		return 0, "", "", false
	}
	b := strings.Split(base, "/")
	if len(b) != 3 || b[0] != "" {
		return 0, "", "", false
	}
	n, err := strconv.Atoi(b[2])
	if err != nil || n < 2 || n%2 != 0 {
		return 0, "", "", false
	}
	if len(parts) == 1 {
		return n/2 - 1, common, common, true
	}
	return n/2 - 1, common + parts[1], common + parts[2], true
}

// cfiGroup is the text nodes of the odd CFI step after `before` element
// children of n: a CFI counts through all the text between two elements.
func cfiGroup(n *tnode, before int) []*tnode {
	var group []*tnode
	elements := 0
	for _, child := range n.children {
		if !child.isText() {
			if elements++; elements > before {
				break
			}
			continue
		}
		if elements == before {
			group = append(group, child)
		}
	}
	return group
}

// fromCFIPath finds the point a CFI path inside the chapter ("/4/2/6/1:42") names.
func (c *chapter) fromCFIPath(path string) (textPoint, bool) {
	node := c.root
	for _, part := range strings.Split(path, "/")[1:] {
		m := cfiPathStep.FindStringSubmatch(part)
		if m == nil {
			return textPoint{}, false
		}
		step, _ := strconv.Atoi(m[1])
		if step%2 == 1 {
			units := 0
			if m[2] != "" {
				units, _ = strconv.Atoi(m[2])
			}
			group := cfiGroup(node, (step-1)/2)
			for i, t := range group {
				n := len(utf16.Encode(t.text))
				if units <= n || i == len(group)-1 {
					return textPoint{t, runeIndex(t.text, units)}, true
				}
				units -= n
			}
			// No text there: the next text after that many elements.
			elements := 0
			for _, child := range node.children {
				if !child.isText() {
					if elements++; elements > (step-1)/2 {
						return c.startOf(child)
					}
				}
			}
			return c.after(node)
		}
		if step < 2 {
			return textPoint{}, false
		}
		var next *tnode
		elements := 0
		for _, child := range node.children {
			if !child.isText() {
				if elements++; elements == step/2 {
					next = child
					break
				}
			}
		}
		if next == nil {
			return textPoint{}, false
		}
		node = next
	}
	return c.startOf(node)
}

// after is the first text after an element.
func (c *chapter) after(n *tnode) (textPoint, bool) {
	last := -1
	var walk func(*tnode)
	walk = func(x *tnode) {
		if x.isText() {
			last = x.order
		}
		for _, child := range x.children {
			walk(child)
		}
	}
	walk(n)
	if last < 0 {
		// Empty: the first text whose ancestors don't include it.
		for _, t := range c.texts {
			if !isAncestor(n, t) && comesAfter(n, t) {
				return textPoint{t, 0}, true
			}
		}
		return textPoint{}, false
	}
	if last+1 < len(c.texts) {
		return textPoint{c.texts[last+1], 0}, true
	}
	return textPoint{}, false
}

func isAncestor(a, n *tnode) bool {
	for p := n.parent; p != nil; p = p.parent {
		if p == a {
			return true
		}
	}
	return false
}

// comesAfter reports whether text node t is after element e in reading order.
func comesAfter(e, t *tnode) bool {
	path := func(n *tnode) []int {
		var out []int
		for ; n.parent != nil; n = n.parent {
			for i, sib := range n.parent.children {
				if sib == n {
					out = append([]int{i}, out...)
					break
				}
			}
		}
		return out
	}
	a, b := path(e), path(t)
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return b[i] > a[i]
		}
	}
	return len(b) > len(a)
}

// runeIndex is the character index of a UTF-16 offset.
func runeIndex(text []rune, units int) int {
	for i, r := range text {
		if units <= 0 {
			return i
		}
		units -= utf16.RuneLen(r)
	}
	return len(text)
}

// cfiSteps is a point as CFI steps from the chapter's root: its elements,
// then the text step with its offset.
func cfiSteps(p textPoint) (elements []string, text string) {
	before, units := 0, 0
	for _, sib := range p.node.parent.children {
		if sib == p.node {
			break
		}
		if sib.isText() {
			units += len(utf16.Encode(sib.text))
		} else {
			before++
			units = 0
		}
	}
	units += len(utf16.Encode(p.node.text[:min(max(p.off, 0), len(p.node.text))]))
	text = fmt.Sprintf("/%d:%d", before*2+1, units)
	for n := p.node.parent; n.parent != nil; n = n.parent {
		pos := 0
		for _, sib := range n.parent.children {
			if !sib.isText() {
				pos++
			}
			if sib == n {
				break
			}
		}
		elements = append([]string{fmt.Sprintf("/%d", pos*2)}, elements...)
	}
	return elements, text
}

// cfiRange is the range between two points as epub.js writes one: the path
// they share, then what is left of each.
func cfiRange(spineStep, item int, start, end textPoint) string {
	a, aText := cfiSteps(start)
	b, bText := cfiSteps(end)
	shared := 0
	for shared < len(a) && shared < len(b) && a[shared] == b[shared] {
		shared++
	}
	return fmt.Sprintf("epubcfi(/%d/%d!%s,%s%s,%s%s)", spineStep, (item+1)*2,
		strings.Join(a[:shared], ""), strings.Join(a[shared:], ""), aText, strings.Join(b[shared:], ""), bText)
}

// ---- finding text ----------------------------------------------------------------

// squeezed is a chapter's text without any white space, and where each
// character came from. KOReader and the web reader keep a highlight's text
// with different spacing (line breaks, paragraph joins), so text is
// compared without it.
type squeezed struct {
	text  []rune
	nodes []*tnode
	offs  []int
}

func (c *chapter) squeeze() *squeezed {
	s := &squeezed{}
	for _, t := range c.texts {
		for i, r := range t.text {
			if isCreSpace(r) || r == ' ' {
				continue
			}
			s.text = append(s.text, r)
			s.nodes = append(s.nodes, t)
			s.offs = append(s.offs, i)
		}
	}
	return s
}

func squeezeString(text string) []rune {
	var out []rune
	for _, r := range text {
		if !isCreSpace(r) && r != ' ' {
			out = append(out, r)
		}
	}
	return out
}

// index is how many characters of the squeezed text come before a point.
func (s *squeezed) index(p textPoint) int {
	lo, hi := 0, len(s.text)
	for lo < hi {
		mid := (lo + hi) / 2
		if s.nodes[mid].order < p.node.order || (s.nodes[mid] == p.node && s.offs[mid] < p.off) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// points are the start and end of the squeezed characters [from, to).
func (s *squeezed) points(from, to int) (textPoint, textPoint) {
	return textPoint{s.nodes[from], s.offs[from]}, textPoint{s.nodes[to-1], s.offs[to-1] + 1}
}

// find returns where needle occurs, nearest to squeezed index near; -1 if nowhere.
func (s *squeezed) find(needle []rune, near int) int {
	if len(needle) == 0 || len(needle) > len(s.text) {
		return -1
	}
	best := -1
	hay, pat := string(s.text), string(needle)
	at, counted := 0, 0 // characters before byte `counted`
	for from := 0; ; {
		i := strings.Index(hay[from:], pat)
		if i < 0 {
			break
		}
		at += utf8.RuneCountInString(hay[counted : from+i])
		counted = from + i
		if best < 0 || abs(at-near) < abs(best-near) {
			best = at
		}
		if at > near {
			break // they only get further away
		}
		from += i + 1
		for from < len(hay) && hay[from]&0xC0 == 0x80 {
			from++ // to the start of a character
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
