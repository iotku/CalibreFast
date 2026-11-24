// Package hardcoversync sends users' reading to their own Hardcover
// accounts, links and unlinks those accounts (OAuth's device grant), and
// brings in or sends the authors they follow there.
//
// It is a feature of the app, not a library: a Service is given the app's
// database and works on these tables in it, which the app's migrations
// create:
//
//   - hardcover_links: a user's link, its tokens sealed (Host.Seal), and
//     what they chose to share
//   - hardcover_shared, hardcover_unshared: books shared, and books left out
//     of sharing every book
//   - hardcover_books, hardcover_user_books: which Hardcover book a library
//     book is, as found, or as the user chose
//   - hardcover_sync: books due to be sent, and how the last send went
//   - hardcover_follows, hardcover_aggregate_matches: the last import of
//     followed authors, and the user's choices among near matches
//   - hardcover_follow_queue: follows and unfollows waiting to be sent
//   - aggregate_favorites, Hardcover's columns only: stars brought in from
//     Hardcover (origin 'hardcover') and the ids of authors followed there
//     (hc_id, hc_ref)
//
// It also reads users (deleted_at), reading_status, reading_events and
// hardcover_authors. Everything else of the app's (its books and
// placeholders, reading progress, author groups, secrets, notifications)
// it reaches through Host.
//
// Nothing is sent about a book a user hasn't shared, and nothing on
// Hardcover is ever deleted (an unfollow only when the user asks for it,
// each time). Everything Hardcover answers is untrusted.
package hardcoversync
