package main

// How CalibreFast reads and shows authors, everywhere:
//   - shown as their names ("Alan A. A. Donovan"), several joined with " & "
//     as Calibre does; the sort form ("Donovan, Alan A. A.") is only for
//     sorting (authors.sort, books.author_sort)
//   - read (the editor's field, uploaded files' creators) by splitAuthors,
//     which splits on "&" only and keeps names as written. A list written
//     another way ("Hailu, Gebreyesus; Negash, Ghirmai") stays one name until
//     the admin clicks the editor's Split button (web/authors.ts), so a wrong
//     guess is never saved by itself.
//
// web/authors.ts reads the field the same way: keep them in sync.

import "strings"

// bookAuthorsSQL is a book's (alias b) author names in Calibre's order,
// "A & B", for the grids. Calibre's books.author_sort is the sort form
// ("Last, First"), which isn't what the book popup shows.
const bookAuthorsSQL = `COALESCE((SELECT group_concat(a.name, ' & ' ORDER BY bal.id)
	FROM books_authors_link bal JOIN authors a ON a.id = bal.author WHERE bal.book = b.id), '')`

// splitAuthors splits authors written "A & B". Spaces are tidied; empty
// names and repeats (ignoring case) are dropped.
func splitAuthors(s string) []string {
	parts := strings.Split(s, "&")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		name := strings.Join(strings.Fields(p), " ")
		if k := strings.ToLower(name); name != "" && !seen[k] {
			seen[k] = true
			out = append(out, name)
		}
	}
	return out
}

// splitCreators applies splitAuthors to each creator of a file (one
// dc:creator can hold "A & B") and flattens the result.
func splitCreators(creators []string) []string {
	return splitAuthors(strings.Join(creators, "&"))
}
