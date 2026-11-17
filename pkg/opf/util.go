package opf

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// UndefinedLanguage is the ISO 639-2 code for an undetermined language.
const UndefinedLanguage = "und"

// ParseAnyDate parses the dates found in OPF files, PDF metadata (D:…) and
// metadata services: RFC 3339, Calibre's "2006-01-02 15:04:05.000000-07:00",
// partial dates ("2006-01", "2006") and a few written forms ("Jan 2, 2006").
func ParseAnyDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("empty date string")
	}

	clean := s
	// Handle PDF date prefix "D:" or "d:"
	if strings.HasPrefix(clean, "D:") || strings.HasPrefix(clean, "d:") {
		clean = clean[2:]
	}
	// Remove PDF timezone apostrophes e.g. +00'00' or -05'00'
	if strings.Contains(clean, "'") {
		clean = strings.ReplaceAll(clean, "'", "")
	}

	layouts := []string{
		"2006-01-02 15:04:05.000000-07:00",
		"2006-01-02 15:04:05.000000",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006-01",
		"2006",
		"January 2, 2006",
		"2 January 2006",
		"Jan 2, 2006",
		"Jan 2006",
		"January 2006",
		"20060102150405-0700",
		"20060102150405Z0700",
		"20060102150405Z",
		"20060102150405",
		"20060102",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, clean); err == nil {
			return t, nil
		}
	}

	if clean != s {
		for _, layout := range layouts {
			if t, err := time.Parse(layout, s); err == nil {
				return t, nil
			}
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse date %q", s)
}

// UniqueTags trims tags and drops empty ones and repeats, ignoring case: the
// first spelling stays. A book never has the same tag twice, whether it came
// from the editor, a file or a lookup.
func UniqueTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if k := strings.ToLower(t); t != "" && !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}
