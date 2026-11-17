// Package metalookup looks books up on Open Library and Google Books and
// returns what they know as OPF metadata (package opf).
//
// Open Library is asked by ISBN (the edition, its work and its authors) or by
// title and author (a search, as works, up to 5 matches). Google Books is
// asked for one volume by ISBN or title and author, and needs an API key to
// be useful.
//
// The client is built to be a polite, low-volume citizen:
//   - each source has a Limiter: a token bucket, a daily cap, and a pause
//     after an HTTP 429 for its Retry-After. Over the limit, a request is
//     refused with an error that says when to try again; it isn't queued.
//   - answers, including "no match", are cached (errors aren't).
//   - only the ISBN, or the title and first author, is sent, with the
//     User-Agent you configure and no cookies.
//   - keys and ids that come back from Open Library are checked before they
//     go into a URL.
package metalookup
