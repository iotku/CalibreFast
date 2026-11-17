// Package kosync has the building blocks of a KOReader progress sync
// ("kosync") server: document ids, sync keys, a limiter for key guessing,
// and the protocol's errors and lenient JSON fields.
//
// KOReader's Progress sync plugin stores one reading position per (user,
// document) on a small JSON API. The document is an id the device derives
// from the book: by default a partial MD5 of the file ([PartialMD5]), or the
// MD5 of its file name ([FileNameDigest]).
//
// The protocol is defined by KOReader's plugins/kosync.koplugin and
// github.com/koreader/koreader-sync-server, and described in
// github.com/pid1/kosync-conformance. See README.md for a summary.
//
// This package has no storage or HTTP routing: a server combines these
// pieces with its own users and database.
package kosync
