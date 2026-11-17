package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// SVG pictures for aggregate entries. An uploaded SVG is rebuilt from an
// allowlist of drawing elements and attributes: no scripts, event
// handlers, foreignObject, or links to anything outside the file (only
// "#id" references and embedded raster data: URLs). It's also served with
// a CSP that blocks scripts (aggregateImageHandler), in case it's opened
// on its own rather than through an <img>.

const (
	svgNS   = "http://www.w3.org/2000/svg"
	xlinkNS = "http://www.w3.org/1999/xlink"
	xmlNS   = "http://www.w3.org/XML/1998/namespace"
	// aggregateSVGMaxBytes bounds an SVG picture, before and after cleaning.
	aggregateSVGMaxBytes = 2 << 20
	// svgMaxDepth bounds element nesting.
	svgMaxDepth = 256
)

// svgElements are the elements kept; any other is dropped with its content.
var svgElements = setOf(
	"svg", "g", "defs", "symbol", "use", "title", "desc", "switch",
	"path", "rect", "circle", "ellipse", "line", "polyline", "polygon",
	"text", "tspan", "textPath",
	"linearGradient", "radialGradient", "stop", "pattern",
	"clipPath", "mask", "marker", "image", "style",
	"filter", "feBlend", "feColorMatrix", "feComponentTransfer", "feComposite",
	"feConvolveMatrix", "feDiffuseLighting", "feDisplacementMap", "feDistantLight",
	"feDropShadow", "feFlood", "feFuncA", "feFuncB", "feFuncG", "feFuncR",
	"feGaussianBlur", "feMerge", "feMergeNode", "feMorphology", "feOffset",
	"fePointLight", "feSpecularLighting", "feSpotLight", "feTile", "feTurbulence",
)

// svgAttrRe: attribute names kept (in no namespace): presentation and
// geometry attributes are plain words with dashes; on* handlers are
// rejected separately.
var svgAttrRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*$`)

// svgSafeURLRe: what href may point at.
var svgSafeURLRe = regexp.MustCompile(`^(#[^\s]*|data:image/(png|jpe?g|gif|webp);base64,[A-Za-z0-9+/=\s]*)$`)

// svgCSSURLRe finds url(...) references in CSS.
var svgCSSURLRe = regexp.MustCompile(`(?i)url\(\s*['"]?([^'")]*)`)

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// looksLikeSVG reports whether data is XML (an SVG to clean) rather than a
// raster picture.
func looksLikeSVG(data []byte) bool {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	data = bytes.TrimLeft(data, " \t\r\n")
	return len(data) > 0 && data[0] == '<'
}

// safeCSS reports whether CSS (a style attribute or <style>) loads nothing
// from outside the file.
func safeCSS(css string) bool {
	lower := strings.ToLower(css)
	if strings.Contains(lower, "@import") || strings.Contains(lower, "expression(") || strings.Contains(lower, `\`) {
		return false
	}
	for _, m := range svgCSSURLRe.FindAllStringSubmatch(css, -1) {
		if !svgSafeURLRe.MatchString(strings.TrimSpace(m[1])) {
			return false
		}
	}
	return true
}

// keepAttr returns the name to write an attribute under, or "" to drop it.
func keepAttr(a xml.Attr) string {
	local := a.Name.Local
	switch a.Name.Space {
	case "":
		// xmlns is written once, on the root, by sanitizeSVG.
		if !svgAttrRe.MatchString(local) || local == "xmlns" || strings.HasPrefix(strings.ToLower(local), "on") {
			return ""
		}
		switch strings.ToLower(local) {
		case "href":
			if !svgSafeURLRe.MatchString(strings.TrimSpace(a.Value)) {
				return ""
			}
		case "style":
			if !safeCSS(a.Value) {
				return ""
			}
		}
		if strings.Contains(strings.ToLower(a.Value), "url(") && !safeCSS(a.Value) {
			return ""
		}
		return local
	case xlinkNS:
		if local == "href" && svgSafeURLRe.MatchString(strings.TrimSpace(a.Value)) {
			return "xlink:href"
		}
	case xmlNS:
		if local == "space" || local == "lang" {
			return "xml:" + local
		}
	}
	return ""
}

// sanitizeSVG rebuilds an SVG document from what svgElements and keepAttr
// allow. The root element must be <svg>.
func sanitizeSVG(data []byte) ([]byte, error) {
	if len(data) > aggregateSVGMaxBytes {
		return nil, fmt.Errorf("the SVG is too big (at most %d MB)", aggregateSVGMaxBytes>>20)
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	d.Entity = nil // only the five predefined entities

	var out bytes.Buffer
	depth, skip := 0, 0 // skip: depth of the dropped element we're inside, 0 for none
	inStyle := false
	rooted := false
	var style strings.Builder
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a valid SVG: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > svgMaxDepth {
				return nil, errors.New("the SVG is nested too deeply")
			}
			if depth == 1 {
				if t.Name.Local != "svg" || (t.Name.Space != svgNS && t.Name.Space != "") {
					return nil, errors.New("not an SVG picture")
				}
				rooted = true
			}
			if skip > 0 {
				continue
			}
			if (t.Name.Space != svgNS && t.Name.Space != "") || !svgElements[t.Name.Local] {
				skip = depth
				continue
			}
			out.WriteString("<" + t.Name.Local)
			if depth == 1 {
				out.WriteString(` xmlns="` + svgNS + `" xmlns:xlink="` + xlinkNS + `"`)
			}
			for _, a := range t.Attr {
				name := keepAttr(a)
				if name == "" {
					continue
				}
				out.WriteString(" " + name + `="`)
				_ = xml.EscapeText(&out, []byte(a.Value))
				out.WriteString(`"`)
			}
			out.WriteString(">")
			if t.Name.Local == "style" {
				inStyle = true
				style.Reset()
			}
		case xml.EndElement:
			if skip == depth {
				skip = 0
			} else if skip == 0 {
				if inStyle && t.Name.Local == "style" {
					if css := style.String(); safeCSS(css) {
						_ = xml.EscapeText(&out, []byte(css))
					}
					inStyle = false
				}
				out.WriteString("</" + t.Name.Local + ">")
			}
			depth--
		case xml.CharData:
			if skip > 0 || depth == 0 {
				continue
			}
			if inStyle {
				style.Write(t)
				continue
			}
			_ = xml.EscapeText(&out, t)
		}
		// Comments, processing instructions and directives (DOCTYPE) are dropped.
	}
	if !rooted {
		return nil, errors.New("not an SVG picture")
	}
	if out.Len() > aggregateSVGMaxBytes {
		return nil, errors.New("the SVG is too big")
	}
	return out.Bytes(), nil
}
