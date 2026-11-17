package metalookup

import (
	"html"
	"regexp"
	"strings"

	"github.com/iotku/CalibreFast/pkg/opf"
)

func joinSubtitle(title, subtitle string) string {
	title, subtitle = strings.TrimSpace(title), strings.TrimSpace(subtitle)
	if subtitle == "" {
		return title
	}
	return title + ": " + subtitle
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

var yearRe = regexp.MustCompile(`(?:^|[^0-9])((?:1[5-9]|20)[0-9]{2})(?:[^0-9]|$)`)

// lookupDate turns a publication date ("2016", "October 26, 2015",
// "c1995") into the editor's YYYY-MM-DD, or "" if it has no year.
func lookupDate(s string) string {
	if t, err := opf.ParseAnyDate(s); err == nil {
		return t.Format("2006-01-02")
	}
	if m := yearRe.FindStringSubmatch(s); m != nil {
		return m[1] + "-01-01"
	}
	return ""
}

var blankLinesRe = regexp.MustCompile(`\n\s*\n`)

// textToHTML turns a plain-text description into HTML paragraphs.
func textToHTML(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	var b strings.Builder
	for _, p := range blankLinesRe.Split(s, -1) {
		if p = strings.TrimSpace(p); p != "" {
			b.WriteString("<p>" + strings.ReplaceAll(html.EscapeString(p), "\n", "<br>") + "</p>")
		}
	}
	return b.String()
}
