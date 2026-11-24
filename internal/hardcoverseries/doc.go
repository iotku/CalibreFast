// Package hardcoverseries keeps the library's series linked to Hardcover
// (hardcover.app) and runs every request the server makes to it with its
// own token.
//
// An editor links a series (as browsed, after groups) to a Hardcover
// series, or to several parts of one; its book list is fetched then and
// whenever the editor asks again, and kept with when it was fetched.
// Matching it against the library (Match) gives the series' review: books
// here, books here without the series tag, and books not here, which can
// become placeholders. Nothing is written to the library or the
// placeholders here: the app does that when the editor applies the review.
//
// Every request goes through one queue, run by one worker, within the
// plan's limits (hardcover.Budget, its use saved for the next start). When
// the limits are used up, jobs are held, not failed, and run when they are
// restored (by midnight UTC for the daily one). Editors' requests run
// first; the app may add one Background kind of job (author lookups) that
// waits behind them.
//
// Only the series' name, the ISBNs of its books in the library and
// Hardcover ids are sent. What Hardcover answers is untrusted.
//
// A Service is given the app's database and works on these tables in it,
// which the app's migrations create: hardcover_series and
// hardcover_series_parts (links), hardcover_dismissed and
// hardcover_not_match (an editor's decisions), hardcover_jobs (the queue)
// and hardcover_usage (the plan's use). The library itself (its series,
// books and placeholders) it reaches through Host.
package hardcoverseries
