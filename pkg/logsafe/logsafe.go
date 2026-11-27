// Package logsafe keeps data from forging log lines.
//
// Book metadata, file names, URLs, usernames and errors that quote them are
// attacker-controlled (see docs/SECURITY.md). Written to the log as they are,
// a value holding a newline starts what looks like a new entry of our own
// ("…\n2026/01/01 12:00:00 admin bob signed in"). Text escapes CR and LF so a
// value always stays on its line.
package logsafe

import (
	"fmt"
	"strings"
)

// Text is v as %v would print it, with "\r" and "\n" written as the two
// characters `\r` and `\n`. Use it for every log argument that comes from a
// request, a book or the database, and for errors that may quote them.
func Text(v any) string {
	s := fmt.Sprint(v)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}
