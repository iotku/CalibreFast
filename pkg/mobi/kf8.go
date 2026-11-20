package mobi

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"golang.org/x/net/html"
)

// content is a book turned into EPUB-shaped pieces.
type content struct {
	docs  []doc       // the spine, in order
	files []*resource // style sheets, SVG flows
	toc   []tocEntry
}

type doc struct {
	name string
	data []byte // XHTML
}

type tocEntry struct {
	label, href string
	level       int
}

type kfFrag struct {
	insert, length int
}

type kfPart struct {
	name       string
	raw        []byte
	start, end int // in the text flow
}

type kf8 struct {
	b      *Book
	main   []byte
	frags  []kfFrag
	parts  []kfPart
	res    map[int]*resource
	flows  map[int]*resource
	linked map[string]bool // aid values links point at
	memo   map[[2]int]string
	svgs   map[int]*html.Node // SVG flows, parsed, to put inline
}

func (b *Book) kf8Content(res map[int]*resource) (*content, error) {
	text, err := b.text()
	if err != nil {
		return nil, err
	}
	k := &kf8{b: b, res: res, flows: map[int]*resource{}, linked: map[string]bool{}, memo: map[[2]int]string{}, svgs: map[int]*html.Node{}}
	flows := splitFlows(text, b.p.recordAt(b.h.fdst))
	k.main = flows[0]

	for _, e := range indexOrNil(b.p, b.h.fragment) {
		ins, err := strconv.Atoi(string(e.label))
		ln, ok := e.tag(6, 1)
		if err != nil || !ok {
			ins, ln = 0, 0
		}
		k.frags = append(k.frags, kfFrag{insert: ins, length: int(ln)})
	}
	k.buildParts(indexOrNil(b.p, b.h.skeleton))

	c := &content{}
	for j := 1; j < len(flows); j++ {
		f := k.flowFile(j, flows[j])
		k.flows[j] = f
		c.files = append(c.files, f)
	}
	// Resolve every link first, so the targets' aid attributes become ids.
	for i := range k.parts {
		for _, m := range rePosFidRaw.FindAll(k.parts[i].raw, -1) {
			k.href(-1, string(m))
		}
	}
	toc := k.toc()
	for i, p := range k.parts {
		rw := k.rewriter(i)
		c.docs = append(c.docs, doc{name: p.name, data: toXHTML(p.raw, rw)})
	}
	for _, f := range c.files {
		if f.mediaType == "image/svg+xml" {
			f.data = toSVG(f.data, k.rewriter(-1))
		}
	}
	c.toc = toc
	return c, nil
}

var rePosFidRaw = regexp.MustCompile(`(?i)kindle:pos:fid:[0-9A-V]+:off:[0-9A-V]+`)

func indexOrNil(p *pdb, idx uint32) []indexEntry {
	if idx == noIndex {
		return nil
	}
	e, _ := readIndex(p, idx)
	return e
}

// splitFlows cuts the text into the flows the FDST record lists (flow 0
// is the XHTML, the rest style sheets and SVG).
func splitFlows(text []byte, fdst []byte) [][]byte {
	if len(fdst) < 12 || string(fdst[:4]) != "FDST" {
		return [][]byte{text}
	}
	n := int(be32(fdst, 8))
	if n < 1 || n > 4096 || 12+8*n > len(fdst) {
		return [][]byte{text}
	}
	var out [][]byte
	prev := 0
	for i := range n {
		start := int(be32(fdst, 12+8*i))
		end := len(text)
		if i+1 < n {
			end = int(be32(fdst, 12+8*(i+1)))
		}
		start = min(max(start, prev), len(text))
		end = min(max(end, start), len(text))
		out = append(out, text[start:end])
		prev = end
	}
	return out
}

func (k *kf8) buildParts(skels []indexEntry) {
	if len(skels) == 0 {
		k.parts = []kfPart{{name: "part0000.xhtml", raw: k.main, start: 0, end: len(k.main)}}
		return
	}
	fp := 0
	total := 0
	for si, s := range skels {
		cnt, _ := s.tag(1, 0)
		pos32, _ := s.tag(6, 0)
		ln32, _ := s.tag(6, 1)
		pos, ln := int(pos32), int(ln32)
		if pos > len(k.main) {
			continue
		}
		ln = min(ln, len(k.main)-pos)
		skel := bytes.Clone(k.main[pos : pos+ln])
		base := pos + ln
		for i := 0; i < int(cnt) && fp < len(k.frags); i++ {
			f := k.frags[fp]
			fp++
			n := min(f.length, len(k.main)-base)
			if n <= 0 {
				continue
			}
			ins := min(max(f.insert-pos, 0), len(skel))
			skel = slices.Insert(skel, ins, k.main[base:base+n]...)
			base += n
		}
		total += len(skel)
		if total > 2*k.b.limits.MaxText {
			break
		}
		k.parts = append(k.parts, kfPart{name: fmt.Sprintf("part%04d.xhtml", si), raw: skel, start: pos, end: base})
	}
	if len(k.parts) == 0 {
		k.parts = []kfPart{{name: "part0000.xhtml", raw: k.main, start: 0, end: len(k.main)}}
	}
}

// partAt is the part holding text position pos, or -1.
func (k *kf8) partAt(pos int) int {
	for i, p := range k.parts {
		if pos >= p.start && pos < p.end {
			return i
		}
	}
	return -1
}

// target is the "file#id" a text position becomes.
func (k *kf8) target(pos int) (int, string) {
	pi := k.partAt(pos)
	if pi < 0 {
		return -1, ""
	}
	id := idBefore(k.parts[pi].raw, pos-k.parts[pi].start)
	if aid, ok := cutPrefix(id, "aid-"); ok {
		k.linked[aid] = true
	}
	return pi, id
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return "", false
}

// href resolves kindle:pos:fid for a link in part from ("" when it can't).
func (k *kf8) href(from int, v string) string {
	fid, off, ok := posFid(v)
	if !ok || fid >= len(k.frags) {
		return ""
	}
	key := [2]int{fid, off}
	t, done := k.memo[key]
	if !done {
		pi, id := k.target(k.frags[fid].insert + off)
		if pi >= 0 {
			t = k.parts[pi].name
			if id != "" {
				t += "#" + id
			}
		}
		k.memo[key] = t
	}
	if from >= 0 && t != "" {
		if name := k.parts[from].name; len(t) > len(name) && t[:len(name)] == name && t[len(name)] == '#' {
			return t[len(name):]
		}
	}
	return t
}

var (
	reIDAttr   = regexp.MustCompile(`(?i)^<[^>]*\sid\s*=\s*['"]([^'"]*)['"]`)
	reNameAttr = regexp.MustCompile(`(?i)^<[^>]*\sname\s*=\s*['"]([^'"]*)['"]`)
	reAIDAttr  = regexp.MustCompile(`^<[^>]+\s(?:aid|AID)\s*=\s*['"]([^'"]+)['"]`)
)

// idBefore finds the id (or name, or aid as "aid-…") of the last tag that
// starts before pos in an XHTML part: the anchor a link to pos lands on.
func idBefore(part []byte, pos int) string {
	pos = min(max(pos, 0), len(part))
	gt := bytes.IndexByte(part[pos:], '>')
	lt := bytes.IndexByte(part[pos:], '<')
	if gt >= 0 && (lt == 0 || lt < 0 || gt < lt) {
		pos += gt + 1 // pos is inside a tag: include it
	}
	block := part[:pos]
	floor := max(0, len(block)-1<<18) // don't scan back further than 256 KiB
	for end := len(block); end > floor; {
		gt := bytes.LastIndexByte(block[:end], '>')
		if gt < floor {
			break
		}
		lt := bytes.LastIndexByte(block[:gt], '<')
		if lt < 0 {
			break
		}
		tag := block[lt : gt+1]
		end = lt
		if bytes.HasPrefix(tag, []byte("<body ")) || bytes.HasPrefix(tag, []byte("<body>")) {
			return ""
		}
		if bytes.HasPrefix(tag, []byte("<meta ")) {
			continue
		}
		if m := reIDAttr.FindSubmatch(tag); m != nil {
			return string(m[1])
		}
		if m := reNameAttr.FindSubmatch(tag); m != nil {
			return string(m[1])
		}
		if m := reAIDAttr.FindSubmatch(tag); m != nil {
			return "aid-" + string(m[1])
		}
	}
	return ""
}

func (k *kf8) flowFile(j int, data []byte) *resource {
	t := bytes.TrimLeft(data, " \t\r\n\ufeff")
	if bytes.HasPrefix(t, []byte("<svg")) || bytes.HasPrefix(t, []byte("<?xml")) {
		return &resource{name: fmt.Sprintf("flow%04d.svg", j), mediaType: "image/svg+xml", data: data}
	}
	css := rewriteCSS(string(data), k.embedName, k.flowName)
	return &resource{name: fmt.Sprintf("flow%04d.css", j), mediaType: "text/css", data: []byte(cleanText(css))}
}

func (k *kf8) embedName(n int) string {
	if r := k.res[n]; r != nil {
		return r.name
	}
	return ""
}

func (k *kf8) flowName(n int) string {
	if f := k.flows[n]; f != nil {
		return f.name
	}
	return ""
}

// inlineSVG: an <img> showing an SVG flow becomes the SVG itself, as in the
// book's source. An SVG shown as an image can't load the images it uses.
func (k *kf8) inlineSVG(n *html.Node) *html.Node {
	if n.Namespace != "" || n.Data != "img" {
		return nil
	}
	j, ok := flowNumber(attr(n, "src"))
	if !ok || k.flows[j] == nil || k.flows[j].mediaType != "image/svg+xml" {
		return nil
	}
	if svg, done := k.svgs[j]; done {
		return svg
	}
	var svg *html.Node
	if doc, err := html.Parse(bytes.NewReader(k.flows[j].data)); err == nil {
		svg = findSVG(doc)
	}
	k.svgs[j] = svg
	return svg
}

func (k *kf8) rewriter(from int) rewriter {
	css := func(v string) string { return rewriteCSS(v, k.embedName, k.flowName) }
	return rewriter{
		replace: k.inlineSVG,
		title:   k.b.Metadata.Title,
		lang:    k.b.Metadata.Language,
		css:     css,
		url: func(v string) string {
			if n, ok := embedNumber(v); ok {
				return k.embedName(n)
			}
			if n, ok := flowNumber(v); ok {
				return k.flowName(n)
			}
			if rePosFid.MatchString(v) {
				return k.href(from, v)
			}
			if !safeURL(v) {
				return ""
			}
			return v
		},
		id: func(attrs []html.Attribute) string {
			for _, a := range attrs {
				if a.Namespace == "" && (a.Key == "aid") && k.linked[a.Val] {
					return "aid-" + a.Val
				}
			}
			return ""
		},
	}
}

// toc reads the NCX index.
func (k *kf8) toc() []tocEntry {
	entries, cncx := readIndex(k.b.p, k.b.h.ncx)
	var out []tocEntry
	for _, it := range ncxOrder(entries) {
		e := entries[it.i]
		off, ok := e.tag(3, 0)
		if !ok {
			continue
		}
		label := cleanText(k.b.h.decode(cncx[off]))
		href := ""
		if fid, ok := e.tag(6, 0); ok {
			o, _ := e.tag(6, 1)
			href = k.href(-1, fmt.Sprintf("kindle:pos:fid:%s:off:%s", toBase32(int(fid)), toBase32(int(o))))
		} else if pos, ok := e.tag(1, 0); ok {
			if pi, id := k.target(int(pos)); pi >= 0 {
				href = k.parts[pi].name
				if id != "" {
					href += "#" + id
				}
			}
		}
		if href == "" || label == "" {
			continue
		}
		out = append(out, tocEntry{label: label, href: href, level: it.depth})
	}
	return out
}

func toBase32(n int) string {
	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 32 {
		b = append([]byte{digits[n%32]}, b...)
	}
	return string(b)
}
