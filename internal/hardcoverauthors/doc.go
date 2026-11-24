// Package hardcoverauthors looks authors up on Hardcover (hardcover.app)
// for the top of their page: a picture, a bio and the years they lived.
//
// Looked up only on demand, as a job in the app's Hardcover queue
// (internal/hardcoverseries, as its Background kind JobKind): behind every
// editor's series request, one at most every Spacing, so a queue of them
// (say, held overnight while the plan's daily limit was used up) trickles
// out rather than firing at once. They also leave the last Reserve of the
// day's limit to the editors: past it Lookup returns a
// *hardcoverseries.HeldError and they wait until the limit resets. The
// app enqueues them; this package only runs them.
//
// What Hardcover answers, found or not, is kept for good and never asked
// again unless the app forgets it (an editor's refresh). A lookup that
// failed is retried after RetryAfter. Pictures are fetched once,
// re-encoded as a JPEG and handed to the app to keep (Config.StoreImage),
// never hot-linked.
//
// Only the author's name (and, failing an exact match, a search for it)
// is sent. Everything Hardcover answers is untrusted: the bio is plain
// text, to be shown escaped.
//
// A Service is given the app's database and works on one table in it,
// hardcover_authors, which the app's migrations create. The library's
// author groups and books it reaches through Host.
package hardcoverauthors
