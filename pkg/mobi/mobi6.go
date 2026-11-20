package mobi

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

var (
	reFilepos   = regexp.MustCompile(`(?i)filepos\s*=\s*['"]?0*(\d{1,10})`)
	rePagebreak = regexp.MustCompile(`(?i)<mbp:pagebreak`)
	reAnchorID  = regexp.MustCompile(`<a id="filepos(\d+)"`)
)

// Most documents a MOBI 6 book is split into (at its page breaks).
const maxMobi6Docs = 2000

// mobi6Content turns a MOBI 6 book's single HTML stream into documents,
// split at page breaks, with an anchor at every filepos a link (or the
// NCX) points to.
func (b *Book) mobi6Content(res map[int]*resource) (*content, error) {
	text, err := b.text()
	if err != nil {
		return nil, err
	}
	ncx, cncx := readIndex(b.p, b.h.ncx)

	// Every position something links to.
	var targets []int
	for _, m := range reFilepos.FindAllSubmatch(text, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n <= len(text) {
			targets = append(targets, n)
		}
	}
	for _, e := range ncx {
		if pos, ok := e.tag(1, 0); ok && int(pos) <= len(text) {
			targets = append(targets, int(pos))
		}
	}
	slices.Sort(targets)
	targets = slices.Compact(targets)

	// Insert the anchors (a position inside a tag moves to before it).
	var out bytes.Buffer
	out.Grow(len(text) + 32*len(targets))
	last := 0
	for _, pos := range targets {
		at := pos
		if lt := bytes.LastIndexByte(text[last:at], '<'); lt >= 0 && bytes.IndexByte(text[last+lt:at], '>') < 0 {
			at = last + lt
		}
		out.Write(text[last:at])
		fmt.Fprintf(&out, `<a id="filepos%d"></a>`, pos)
		last = at
	}
	out.Write(text[last:])
	marked := out.Bytes()

	// Split at page breaks.
	var chunks [][]byte
	prev := 0
	for _, loc := range rePagebreak.FindAllIndex(marked, -1) {
		if len(chunks) >= maxMobi6Docs-1 {
			break
		}
		if loc[0] > prev && len(bytes.TrimSpace(marked[prev:loc[0]])) > 0 {
			chunks = append(chunks, marked[prev:loc[0]])
			prev = loc[0]
		}
	}
	chunks = append(chunks, marked[prev:])

	c := &content{}
	where := map[int]int{} // filepos → chunk
	for i, ch := range chunks {
		for _, m := range reAnchorID.FindAllSubmatch(ch, -1) {
			if n, err := strconv.Atoi(string(m[1])); err == nil {
				where[n] = i
			}
		}
	}
	name := func(i int) string { return fmt.Sprintf("part%04d.xhtml", i) }
	link := func(from, pos int) string {
		i, ok := where[pos]
		if !ok {
			return ""
		}
		frag := fmt.Sprintf("#filepos%d", pos)
		if i == from {
			return frag
		}
		return name(i) + frag
	}
	for i, ch := range chunks {
		markup := []byte(b.h.decode(ch))
		rw := rewriter{
			title: b.Metadata.Title,
			lang:  b.Metadata.Language,
			css:   func(v string) string { return v },
			url: func(v string) string {
				if rest, ok := strings.CutPrefix(v, "#filepos"); ok {
					if n, err := strconv.Atoi(rest); err == nil {
						return link(i, n)
					}
				}
				if !safeURL(v) {
					return ""
				}
				return v
			},
		}
		rw.element = func(n *html.Node) { fix(n, res) }
		c.docs = append(c.docs, doc{name: name(i), data: toXHTML(markup, rw)})
	}
	for _, it := range ncxOrder(ncx) {
		e := ncx[it.i]
		off, ok1 := e.tag(3, 0)
		pos, ok2 := e.tag(1, 0)
		if !ok1 || !ok2 {
			continue
		}
		label := cleanText(b.h.decode(cncx[off]))
		href := link(-1, int(pos))
		if label == "" || href == "" {
			continue
		}
		c.toc = append(c.toc, tocEntry{label: label, href: href, level: it.depth})
	}
	return c, nil
}

// fix turns MOBI 6 attributes into HTML ones: filepos links into hrefs,
// recindex images into srcs.
func fix(n *html.Node, res map[int]*resource) {
	var href, src string
	hasHref, hasSrc := false, false
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "filepos":
			if p, err := strconv.Atoi(strings.TrimLeft(strings.Trim(a.Val, `'" `), "0")); err == nil {
				href = fmt.Sprintf("#filepos%d", p)
			} else if strings.Trim(a.Val, `'" 0`) == "" {
				href = "#filepos0"
			}
		case "recindex", "hirecindex", "lowrecindex":
			if p, err := strconv.Atoi(strings.TrimLeft(a.Val, "0")); err == nil && res[p] != nil && src == "" {
				src = res[p].name
			}
		case "href":
			hasHref = true
		case "src":
			hasSrc = true
		}
	}
	if href != "" && !hasHref {
		n.Attr = append(n.Attr, html.Attribute{Key: "href", Val: href})
	}
	if src != "" && !hasSrc {
		n.Attr = append(n.Attr, html.Attribute{Key: "src", Val: src})
	}
}
