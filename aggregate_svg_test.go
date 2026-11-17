package main

import (
	"strings"
	"testing"
)

func TestSanitizeSVG(t *testing.T) {
	in := `<?xml version="1.0"?>
<!DOCTYPE svg>
<!-- made by hand -->
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"
     xmlns:inkscape="http://www.inkscape.org/namespaces/inkscape"
     viewBox="0 0 10 10" onload="alert(1)" inkscape:version="1">
  <script>alert(2)</script>
  <style>rect { fill: url(#g) } .x { fill: red }</style>
  <style>@import url(https://evil.example/x.css);</style>
  <defs><linearGradient id="g"><stop offset="0" stop-color="#fff"/></linearGradient></defs>
  <rect width="10" height="10" style="fill:url(https://evil.example/t)" fill="url(#g)"/>
  <circle r="2" onclick="alert(3)" fill="blue"/>
  <use xlink:href="#g"/><use href="javascript:alert(4)"/>
  <image href="https://evil.example/p.png"/>
  <image href="data:image/png;base64,iVBORw0KGgo="/>
  <image href="data:image/svg+xml;base64,PHN2Zy8+"/>
  <foreignObject><div xmlns="http://www.w3.org/1999/xhtml">hi</div></foreignObject>
  <text x="1" y="5">A &amp; B &lt;ok&gt;</text>
</svg>`
	out, err := sanitizeSVG([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"script", "alert", "onload", "onclick", "evil.example", "foreignObject",
		"inkscape", "DOCTYPE", "<!--", "@import", "svg+xml", "javascript"} {
		if strings.Contains(s, bad) {
			t.Errorf("kept %q:\n%s", bad, s)
		}
	}
	for _, good := range []string{`<svg xmlns="http://www.w3.org/2000/svg"`, `viewBox="0 0 10 10"`,
		`fill="url(#g)"`, `xlink:href="#g"`, `fill: url(#g)`, "data:image/png;base64",
		`<circle r="2" fill="blue">`, "A &amp; B &lt;ok&gt;", `<stop offset="0" stop-color="#fff">`} {
		if !strings.Contains(s, good) {
			t.Errorf("lost %q:\n%s", good, s)
		}
	}
	if n := strings.Count(s, "xmlns="); n != 1 {
		t.Errorf("%d xmlns attributes:\n%s", n, s)
	}
	if _, err := sanitizeSVG(out); err != nil {
		t.Errorf("cleaned SVG doesn't parse again: %v", err)
	}

	for name, bad := range map[string]string{
		"html root":     `<html><body/></html>`,
		"not xml":       `<svg><rect></svg>`,
		"custom entity": `<!DOCTYPE svg [<!ENTITY a "aaaa">]><svg>&a;</svg>`,
		"empty":         ``,
	} {
		if _, err := sanitizeSVG([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !looksLikeSVG([]byte("\xef\xbb\xbf\n <svg/>")) || looksLikeSVG([]byte("\x89PNG")) {
		t.Error("looksLikeSVG")
	}
}
