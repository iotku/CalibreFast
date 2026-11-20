package mobi

import (
	"regexp"
	"strings"
)

// base32 parses Kindle's base 32 numbers (digits 0-9, A-V).
func base32(s string) (int, bool) {
	if s == "" || len(s) > 10 {
		return 0, false
	}
	n := 0
	for _, c := range strings.ToUpper(s) {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'A' && c <= 'V':
			d = int(c-'A') + 10
		default:
			return 0, false
		}
		n = n*32 + d
		if n > 1<<40 {
			return 0, false
		}
	}
	return n, true
}

var (
	reEmbed  = regexp.MustCompile(`(?i)^kindle:embed:([0-9A-V]+)(?:\?.*)?$`)
	reFlow   = regexp.MustCompile(`(?i)^kindle:flow:([0-9A-V]+)(?:\?.*)?$`)
	rePosFid = regexp.MustCompile(`(?i)^kindle:pos:fid:([0-9A-V]+):off:([0-9A-V]+)`)
	// kindle: URLs inside CSS (url(...) and @import strings).
	reCSSKindle = regexp.MustCompile(`(?i)kindle:(embed|flow):([0-9A-V]+)(?:\?mime=[^)'"\s]*)?`)
)

// embedNumber parses kindle:embed:XXXX[?mime=…].
func embedNumber(s string) (int, bool) {
	m := reEmbed.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	return base32(m[1])
}

func flowNumber(s string) (int, bool) {
	m := reFlow.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	return base32(m[1])
}

func posFid(s string) (fid, off int, ok bool) {
	m := rePosFid.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, false
	}
	fid, ok1 := base32(m[1])
	off, ok2 := base32(m[2])
	return fid, off, ok1 && ok2
}

// rewriteCSS points kindle:embed and kindle:flow URLs in CSS at the files
// they became (name returns "" for one that doesn't exist).
func rewriteCSS(css string, embed func(int) string, flow func(int) string) string {
	if !strings.Contains(strings.ToLower(css), "kindle:") {
		return css
	}
	return reCSSKindle.ReplaceAllStringFunc(css, func(m string) string {
		sub := reCSSKindle.FindStringSubmatch(m)
		n, ok := base32(sub[2])
		if !ok {
			return ""
		}
		if strings.EqualFold(sub[1], "embed") {
			return embed(n)
		}
		return flow(n)
	})
}

// safeURL keeps relative URLs, fragments and web/mail links; it drops
// anything else (javascript:, data:, file:, unresolved kindle: links).
func safeURL(u string) bool {
	s := strings.TrimSpace(u)
	i := strings.IndexAny(s, ":/?#")
	if i < 0 || s[i] != ':' {
		return true // no scheme
	}
	switch strings.ToLower(s[:i]) {
	case "http", "https", "mailto":
		return true
	}
	return false
}
