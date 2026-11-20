package mobi

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// rewriter says how a document's links and styles change on the way out.
type rewriter struct {
	// url maps an href/src value to the one written; "" drops the attribute.
	url func(v string) string
	// css rewrites a style sheet or style attribute.
	css func(v string) string
	// id gives an element without an id one to write (from its attributes),
	// or "".
	id func(attrs []html.Attribute) string
	// title is written when the document has none.
	title string
	// lang is the root element's language when the document has none.
	lang string
	// element, if set, may change an element's attributes before it's written.
	element func(n *html.Node)
	// replace, if set, may give an element to write in place of n.
	replace func(n *html.Node) *html.Node
}

// Elements dropped with everything in them.
var dropped = map[string]bool{
	"script": true, "noscript": true, "iframe": true, "frame": true, "frameset": true,
	"object": true, "embed": true, "applet": true, "base": true, "meta": true,
	"guide": true, "template": true, "form": true, "input": true, "button": true,
	"select": true, "textarea": true,
}

// SVG and MathML elements dropped: scripts, and animations that can set
// an href to anything.
var droppedForeign = map[string]bool{
	"script": true, "set": true, "animate": true, "animatemotion": true,
	"animatetransform": true, "handler": true, "listener": true, "iframe": true,
}

var voidElements = map[string]bool{
	"area": true, "br": true, "col": true, "hr": true, "img": true, "link": true,
	"source": true, "track": true, "wbr": true,
}

// URL-valued attributes.
var urlAttrs = map[string]bool{"href": true, "src": true, "poster": true, "xlink:href": true, "cite": true}

const (
	nsXHTML = "http://www.w3.org/1999/xhtml"
	nsSVG   = "http://www.w3.org/2000/svg"
	nsMath  = "http://www.w3.org/1998/Math/MathML"
	nsXLink = "http://www.w3.org/1999/xlink"
	nsEPUB  = "http://www.idpf.org/2007/ops"
)

// toXHTML parses markup (tag soup is fine) and writes it as a well-formed
// XHTML document.
func toXHTML(markup []byte, rw rewriter) []byte {
	doc, err := html.Parse(bytes.NewReader(markup))
	if err != nil {
		doc = &html.Node{Type: html.DocumentNode}
	}
	w := &xw{rw: rw}
	w.buf.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<!DOCTYPE html>\n")
	root := findChild(doc, atom.Html)
	if root == nil {
		root = &html.Node{Type: html.ElementNode, DataAtom: atom.Html, Data: "html"}
	}
	w.root(root)
	return w.buf.Bytes()
}

func findChild(n *html.Node, a atom.Atom) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == a {
			return c
		}
	}
	return nil
}

type xw struct {
	buf bytes.Buffer
	rw  rewriter
}

func (w *xw) root(n *html.Node) {
	lang := attr(n, "lang")
	if lang == "" {
		lang = attr(n, "xml:lang")
	}
	if lang == "" {
		lang = w.rw.lang
	}
	w.buf.WriteString(`<html xmlns="` + nsXHTML + `" xmlns:epub="` + nsEPUB + `"`)
	if lang = cleanText(strings.TrimSpace(lang)); lang != "" && validName(lang) {
		w.buf.WriteString(` lang="`)
		escape(&w.buf, lang, true)
		w.buf.WriteString(`" xml:lang="`)
		escape(&w.buf, lang, true)
		w.buf.WriteString(`"`)
	}
	w.buf.WriteString(">\n")
	head := findChild(n, atom.Head)
	body := findChild(n, atom.Body)
	w.buf.WriteString("<head>\n")
	hasTitle := false
	if head != nil {
		for c := head.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.DataAtom == atom.Title {
				if hasTitle {
					continue
				}
				hasTitle = true
			}
			if c.Type == html.ElementNode && c.DataAtom == atom.Link && !isStylesheet(c) {
				continue
			}
			w.node(c, nsXHTML)
		}
	}
	if !hasTitle {
		w.buf.WriteString("<title>")
		escape(&w.buf, cleanText(w.rw.title), false)
		w.buf.WriteString("</title>")
	}
	w.buf.WriteString("\n</head>\n")
	if body == nil {
		w.buf.WriteString("<body></body>\n</html>\n")
		return
	}
	w.element(body, nsXHTML)
	w.buf.WriteString("\n</html>\n")
}

func isStylesheet(n *html.Node) bool {
	return strings.Contains(strings.ToLower(attr(n, "rel")), "stylesheet")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func (w *xw) node(n *html.Node, parentNS string) {
	switch n.Type {
	case html.TextNode:
		escape(&w.buf, cleanText(n.Data), false)
	case html.ElementNode:
		w.element(n, parentNS)
	}
}

func (w *xw) children(n *html.Node, ns string) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		w.node(c, ns)
	}
}

func (w *xw) element(n *html.Node, parentNS string) {
	if w.rw.element != nil {
		w.rw.element(n)
	}
	if w.rw.replace != nil {
		if r := w.rw.replace(n); r != nil && r != n {
			w.element(r, parentNS)
			return
		}
	}
	name := n.Data
	ns := nsXHTML
	switch n.Namespace {
	case "svg":
		ns = nsSVG
	case "math":
		ns = nsMath
	}
	lower := strings.ToLower(name)
	if dropped[lower] && ns == nsXHTML || ns != nsXHTML && droppedForeign[lower] {
		return
	}
	if n.DataAtom == atom.Html || n.DataAtom == atom.Head || strings.Contains(name, ":") || !validName(name) {
		// Unknown prefixed tags (mbp:pagebreak, …) and junk: keep the content.
		w.children(n, parentNS)
		return
	}
	if n.DataAtom == atom.Style && ns == nsXHTML {
		w.buf.WriteString("<style>")
		var css strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				css.WriteString(c.Data)
			}
		}
		escape(&w.buf, cleanText(w.rw.css(css.String())), false)
		w.buf.WriteString("</style>")
		return
	}
	w.buf.WriteByte('<')
	w.buf.WriteString(name)
	if ns != parentNS {
		w.buf.WriteString(` xmlns="` + ns + `"`)
		if ns == nsSVG {
			w.buf.WriteString(` xmlns:xlink="` + nsXLink + `"`)
		}
	}
	w.attrs(n, ns)
	if len(n.Attr) > 0 && w.rw.id != nil && attr(n, "id") == "" {
		if id := w.rw.id(n.Attr); id != "" {
			w.buf.WriteString(` id="`)
			escape(&w.buf, id, true)
			w.buf.WriteByte('"')
		}
	}
	if n.FirstChild == nil && (ns != nsXHTML || voidElements[lower]) {
		w.buf.WriteString("/>")
		return
	}
	w.buf.WriteByte('>')
	if !(ns == nsXHTML && voidElements[lower]) {
		w.children(n, ns)
	}
	w.buf.WriteString("</")
	w.buf.WriteString(name)
	w.buf.WriteByte('>')
}

func (w *xw) attrs(n *html.Node, ns string) {
	seen := map[string]bool{}
	for _, a := range n.Attr {
		key := a.Key
		switch a.Namespace {
		case "xlink":
			key = "xlink:" + a.Key
		case "xml":
			key = "xml:" + a.Key
		case "":
		default:
			continue
		}
		lk := strings.ToLower(key)
		if strings.HasPrefix(lk, "on") || strings.HasPrefix(lk, "xmlns") || lk == "aid" ||
			lk == "filepos" || lk == "recindex" || lk == "hirecindex" || lk == "lowrecindex" {
			continue
		}
		if strings.Contains(key, ":") && !strings.HasPrefix(lk, "xml:") && !strings.HasPrefix(lk, "xlink:") && lk != "epub:type" {
			continue
		}
		if ns == nsXHTML && !strings.Contains(key, ":") {
			key = lk
		}
		if !validName(key) || seen[key] {
			continue
		}
		val := a.Val
		if urlAttrs[lk] {
			if val = w.rw.url(val); val == "" {
				continue
			}
		} else if lk == "style" {
			val = w.rw.css(val)
		}
		seen[key] = true
		w.buf.WriteByte(' ')
		w.buf.WriteString(key)
		w.buf.WriteString(`="`)
		escape(&w.buf, cleanText(val), true)
		w.buf.WriteByte('"')
	}
}

func escape(b *bytes.Buffer, s string, attr bool) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			if attr {
				b.WriteString("&quot;")
			} else {
				b.WriteRune(r)
			}
		case '\n', '\t', '\r':
			if attr {
				b.WriteString(" ")
			} else {
				b.WriteRune(r)
			}
		default:
			b.WriteRune(r)
		}
	}
}

// validName: a conservative XML name (ASCII letters, digits, - _ . and one
// optional prefix colon).
func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case (c >= '0' && c <= '9') || c == '-' || c == '.' || c == ':':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return !strings.HasSuffix(s, ":") && strings.Count(s, ":") <= 1
}

// toSVG re-writes an SVG document (a KF8 flow) through the same filter as
// XHTML: only its first <svg> element is kept.
func toSVG(markup []byte, rw rewriter) []byte {
	doc, err := html.Parse(bytes.NewReader(markup))
	if err != nil {
		return nil
	}
	svg := findSVG(doc)
	if svg == nil {
		return nil
	}
	w := &xw{rw: rw}
	w.buf.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	w.element(svg, "")
	return w.buf.Bytes()
}

// findSVG is the first <svg> element in a parsed document, or nil.
func findSVG(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Namespace == "svg" && c.Data == "svg" {
			return c
		}
		if s := findSVG(c); s != nil {
			return s
		}
	}
	return nil
}
