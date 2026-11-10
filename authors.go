package main

// How CalibreFast reads and shows authors, everywhere:
//   - shown as their names ("Alan A. A. Donovan"), several joined with " & "
//     as Calibre does; the sort form ("Donovan, Alan A. A.") is only for
//     sorting (authors.sort, books.author_sort)
//   - read (the editor's field, uploaded files' creators) by parseAuthors,
//     which splits on ";" and "&" and turns "Last, First" into "First Last"
//
// web/authors.ts has the same rules for the editor: keep them in sync.

import (
	"regexp"
	"strings"
)

// bookAuthorsSQL is a book's (alias b) author names in Calibre's order,
// "A & B", for the grids. Calibre's books.author_sort is the sort form
// ("Last, First"), which isn't what the book popup shows.
const bookAuthorsSQL = `COALESCE((SELECT group_concat(a.name, ' & ' ORDER BY bal.id)
	FROM books_authors_link bal JOIN authors a ON a.id = bal.author WHERE bal.book = b.id), '')`

var authorSeparatorRe = regexp.MustCompile(`[;&]`)

// nameSuffixes come after a comma without making a name "Last, First":
// "Martin Luther King, Jr.".
var nameSuffixes = map[string]bool{"jr": true, "sr": true, "ii": true, "iii": true, "iv": true, "phd": true, "md": true}

// parseAuthors splits a list of authors and writes each as "First Last".
//   - ";" and "&" separate authors: "Hailu, Gebreyesus; Negash, Ghirmai"
//     and "Alan Donovan & Brian Kernighan".
//   - Without them, commas separate authors only if every part is a full
//     name ("Alan Donovan, Brian Kernighan"). Otherwise the comma is inside
//     one name ("Donovan, Alan A. A.").
//   - "Last, First" becomes "First Last" (not "King, Jr." or names with
//     more than one comma).
//
// Repeats are dropped, ignoring case.
func parseAuthors(s string) []string {
	var parts []string
	switch {
	case authorSeparatorRe.MatchString(s):
		parts = authorSeparatorRe.Split(s, -1)
	case strings.Contains(s, ","):
		parts = strings.Split(s, ",")
		for _, p := range parts {
			if !strings.Contains(strings.TrimSpace(p), " ") {
				parts = []string{s} // "Last, First": one name
				break
			}
		}
	default:
		parts = []string{s}
	}
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		name := firstLast(strings.Join(strings.Fields(p), " "))
		if k := strings.ToLower(name); name != "" && !seen[k] {
			seen[k] = true
			out = append(out, name)
		}
	}
	return out
}

// firstLast turns "Last, First" into "First Last".
func firstLast(name string) string {
	last, first, ok := strings.Cut(name, ",")
	last, first = strings.TrimSpace(last), strings.TrimSpace(first)
	if !ok || last == "" || first == "" || strings.Contains(first, ",") ||
		nameSuffixes[strings.ToLower(strings.Trim(first, ". "))] {
		return name
	}
	return first + " " + last
}

// normalizeAuthors applies parseAuthors to each creator of a file (one
// dc:creator can hold several authors) and flattens the result.
func normalizeAuthors(creators []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range creators {
		for _, name := range parseAuthors(c) {
			if k := strings.ToLower(name); !seen[k] {
				seen[k] = true
				out = append(out, name)
			}
		}
	}
	return out
}
