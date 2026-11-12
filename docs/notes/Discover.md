# Plan: Discover home page

Status: proposal. Done so far: guest and per-user reading data kept apart (first half of phase 5, PR #98).

## Goal

`/` becomes a dashboard of short horizontal rows instead of the raw grid:

1. **Continue reading**: the user's `reading` books, most recent activity first, with progress bars and resume links.
2. **Want to read**: `wanted` books, newest first.
3. **Pinned shelves**: one row per shelf the user pinned, plus admin-**featured** global shelves they haven't unpinned.
4. **Recommended for you**: books sharing tags (and series/authors) with what the user reads, wants, finished and starred.
5. **More in _Tag_** ×2–3: the user's strongest tags, then random popular tags.
6. **Shuffle**: a random handful of the library, leaving out books the user finished, different on every load.
7. **Recently added**: the first dozen of today's grid, with "See all" to `/latest`.

Every row has a **See all →** link to an existing full grid (`/me/reading`, `/me/wanted`, `/shelf/N`, `/tag/X`, `/latest`, `/shuffle`), so the scroll engine stays the one place for browsing a whole list.

### Signed-out visitors (`-public` / `-public-read`)

- **Continue reading** from this device: the readers already save positions locally when not signed in (`cf-progress-<uuid>-<format>` in `web/reading-sync.ts`). See [Local reading for signed-out visitors](#local-reading-for-signed-out-visitors).
- **Featured shelves** (all of them; nothing to unpin).
- **More in _Tag_**: random popular tags only.
- **Shuffle**: leaves out books read to the end on this device.
- **Recently added**.
- No recommendations or Want to read rows: none of this reaches the server, so there's nothing to tailor from.

## Constraints kept

- **No metadata.db queries on a page load.** Everything Discover shows about books comes from static, immutable-cached JSON (like `pages/pageN.json`) or from an in-memory index built in the same startup pass that writes the pages.
- **One dynamic request per load** when signed in: `GET /api/me/home` (app database only, a few indexed queries on `user_id`). Its last answer is kept in localStorage, as `web/reading.ts` already does for `/api/me/reading`, so personal rows paint before it returns. Signed out, the only dynamic request is the optional local-reading lookup (below), and only when this device has read something.
- Pages stay identical for everyone and cacheable; personal parts are filled by script (same rule as `nav.ts`).

## Load-time request budget

| Request | Cache | Notes |
|---|---|---|
| `/` HTML | revalidated | embeds the tag-row index, `pagesVersion` and `featuredVersion` |
| `/api/me/home` | `private, no-store` | signed in only; includes pinned shelf rows |
| `/pages/discover/featured.json?v=` | immutable | signed out only: featured shelf rows |
| `/api/books?uuids=` | short `max-age` | signed out, only when this device has local reading; served from memory |
| `/pages/shuffle/pageK.json?v=` | immutable | K picked at random by the browser |
| `/pages/discover/tag-N.json?v=` | immutable | one per tag row, fetched when the row nears the viewport |
| `/pages/page1.json?v=` | immutable | already cached for anyone who has used `/latest` |
| covers | existing thumbnail path | row-aware cover pump (below) |

## Server

### 1. In-memory catalog (`catalog.go`, new)

Built inside `writeLibraryPages` (main.go:993), which already reads every book once in `timestamp DESC` order. Extend that query with `b.id` and keep a compact slice:

```go
// One book. Tags and authors are ranges into the catalog's shared arenas,
// so 100k books aren't 200k small slices.
type catalogBook struct {
    Book                   // what the grid JSON already has
    ID          int32      // calibre books.id, for link tables
    tagLo, tagHi int32     // c.tagArena[tagLo:tagHi]: tag ids (groups resolved)
    authLo, authHi int32   // c.authArena[authLo:authHi]: author ids
    Series      int32      // 0 if none
    Removed     bool       // tombstone (recycled in this run)
}
type catalog struct {
    books       []catalogBook      // index = position in Latest order
    byUUID      map[string]int32
    tagArena, authArena []int32
    tagBooks    map[int32][]int32  // tag -> books, Latest order
    tagNames    map[int32]string   // groups resolved (below)
    seriesBooks map[int32][]int32  // series -> books by series_index
    shuffle     []int32            // permutation of books (section 2)
    built       time.Time
}
var cat atomic.Pointer[catalog]
func currentCatalog() *catalog  // never nil after startup
```

**Building** (`buildCatalog(db)`): the `writeLibraryPages` query gains `b.id`; the books are collected as the pages are written. Then three more queries on the same connection, each a full scan of a small integer link table plus its names: `books_tags_link`/`tags`, `books_authors_link`, `books_series_link`/`series`. Log one line: `catalog: 48211 books, 3104 tags, 812 series in 140ms`.

**Memory**: roughly the size of `pages/*.json` (~250 B/book: ~25 MB at 100k books) plus 4 B per tag/author link. Two existing caches hold copies of the same strings (`uuidPathIndex`: 19 uses, `modTimeCache`: 6). Later they can read from the catalog instead, which roughly evens out. Not part of phase 1.

**Updating: rebuild and swap, debounced.** Every write path (upload, editor, bulk edit, merge, import approval, recycle and restore, group edits, library repairs) already ends in `refreshAllCaches()`. That calls `scheduleCatalogRebuild()`: after 2 s with no further calls, a goroutine builds a new catalog and swaps the pointer. That's one hook instead of eight, it's always correct, and a bulk edit of 500 books costs one rebuild. Handlers keep reading the old catalog meanwhile and never lock. The rebuild keeps the existing shuffle order for books it already had and inserts new ones at random places. The static shuffle files only change at restart, but `/shuffle` for signed-in users (built from the catalog) sees new books at once. Recycled books stay in the catalog as tombstones until restart, for the same reason as the page gaps (`removeFromPages`). If rebuilds turn out slow on big libraries, the known uuids at each call site allow incremental updates later. Measure first.

**First users** (phase 1, no visible change):
- `booksByUUIDs` (userdata.go:31) answers from the catalog: every `/me/*` list, shelves, and later `/api/me/home`. A miss (a book added since the last rebuild) falls back to the SQL it uses today.
- `bookExists` (userdata.go:24), on every favorite and status change: catalog first, SQL on a miss.

**Tests** (`catalog_test.go`, on the `setupTestDB` fixture):
- After `writeLibraryPages`, every book in `metadata.db` is in the catalog with its exact tags, authors and series, in `timestamp DESC` order.
- Tag groups collapse member tags into one id.
- `booksByUUIDs` gives the same answer from the catalog as from SQL, including order and skipped missing uuids.
- A rebuild after an insert keeps existing shuffle positions and contains the new book.
- A recycled book is tombstoned and absent from `booksByUUIDs`.
- `-race` run with handlers reading during a rebuild.

**Tag groups**: map each member tag id to its group (`groups.all("tag")`) when building `tagBooks`, so "Science Fiction"/"Sci-Fi" count as one, matching `/tags`.

### 2. Random order at startup

- `shuffle` = Fisher–Yates over `0..n-1` with `math/rand/v2` seeded per run: O(n), microseconds even for 100k books.
- Written as `pages/shuffle/pageN.json` (same `Book` shape, `booksPerPage` each) in the same `writeLibraryPages` pass and under the same `pagesVersion`, so `servePages` caches them for good.
- **"Different on every load"**: the browser picks a random page K and a random 12-book window inside it. With 60 books/page and N pages that's ~N×48 distinct strips without the server doing anything per request.
- **Finished books are left out.** In the Discover row the browser filters the window against the finished set it already has (signed in: the `cf-reading` copy of `/api/me/reading`; signed out: books read to the end on this device). If fewer than 12 remain it slides the window on, and fetches page K+1 if the page runs out.
- **The full `/shuffle` grid** walks the permutation in order. The grid places books by index, so it can't drop cards in the browser without breaking its layout. Instead:
  - Signed in: `/shuffle` is a `uuidList` (`serveUUIDList`, like `/me/*`) whose pages are the catalog's `shuffle` minus the user's finished set, built in memory from one `reading_status` query, cached per user marks version. No metadata.db queries.
  - Signed out: the static `pages/shuffle/pageN.json`. Books finished on this device still show there (with their mark); only the Discover row filters them. Acceptable, since the row is what people see first.
- New uploads don't enter the shuffle until restart (they're on Latest and in tag rows' next rebuild); removals reuse the existing gap mechanism (`removeFromPages`) by recording shuffle pages in a second `bookPages`-style map.

### 3. Tag rows (static)

At startup, write `pages/discover/tag-N.json` for each **eligible** tag: up to 24 books from the tag's posting list, taken in shuffle order (so a tag row isn't just its newest books). The index (`[{n, name, href, count}]`) is embedded in the Discover HTML.

**Eligible tags** (names resolved through tag groups, so a group or any of its members works):
- **Include list**: always eligible, whatever their share of the library, and always get a file.
- **Deny list**: never eligible. Also ignored by recommendations: no affinity score, no row, no `because` (so "Unknown", "To sort", "Owned" never drive suggestions).
- **Everything else**: the top ~40 by count among tags covering at most **25%** of the library (dull rows like "Fiction", "Ebook" drop out). 25% is the default; admins can change it.

The browser chooses which rows to show: the user's affinity tags from `/api/me/home` first, then random picks from the index, weighted by count, with included tags weighted up.

#### Admin tag settings

- App database (migration at the end of `migrations` in `appdb.go`):
  ```sql
  CREATE TABLE discover_tags (name TEXT PRIMARY KEY COLLATE NOCASE,
                              mode TEXT NOT NULL CHECK (mode IN ('include', 'deny')));
  CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL); -- discover.broad_tag_share = 0.25
  ```
  Kept in the app database, not the library: it's a CalibreFast presentation choice, so it needs a signed-in admin (`requireAdminUser`), not `-allow-writes`.
- `GET /api/admin/discover` (lists and cutoff, plus the tags the cutoff currently excludes, so admins can see what they'd be including), `PUT`/`DELETE /api/admin/discover/tags/{name}`, `PUT /api/admin/discover/settings`.
- UI: an admin-only **Discover** toggle on `/tags` (alongside Group) that marks each tag Include / Deny / Default, plus the cutoff field. A tag the cutoff excludes is shown dimmed there.
- A change rebuilds the eligible set and rewrites the tag files (≤ a few dozen small files, from memory), then bumps `pagesVersion`.

### 4. `GET /api/me/home` (`home.go`, new)

One handler, app database only, book details from the catalog:

```jsonc
{
  "user": "name",
  "reading":  [{"book": {...}, "progress": 0.42, "resume": "/read?uuid=…", "at": 1731400000000}],
  "wanted":   [{...}],
  "recommended": [{"book": {...}, "because": "Dune"}],
  "tags": [{"name": "Space Opera", "href": "/tag/Space%20Opera"}],
  "series": [{"book": {...}, "after": "Dune Messiah"}],  // next unread in series
  "shelves": [{"id": 7, "name": "Hugo winners", "href": "/shelf/7", "featured": true, "count": 63, "books": [{...}]}]
}
```

- **Reading order**: `max(reading_status.updated_at, latestProgress(u).UpdatedAt)` per book, newest first. `latestProgress` (bookprogress.go:37) already returns `UpdatedAt` in ms and is what `/api/me/reading` uses; reuse the same loop. Cap at 20; "See all" covers the rest.
- **Wanted**: existing `statusList` query, cap 20.
- **Shelves**: the user's effective pins (next section), first 24 books of each in the shelf page's own order, capped at 12 rows. One `shelf_books` query with a window function (`ROW_NUMBER() OVER (PARTITION BY shelf_id …) <= 24`).
- Shares queries with `readingHandler`; factor the status/favorites scan into one helper both call.

### 4a. Pinned and featured shelves

Both kinds of shelf (personal and global, `shelves.go`) can be pinned; only global ones can be featured.

**Data** (app database, new migration):
```sql
ALTER TABLE shelves ADD COLUMN featured_pos INTEGER;  -- NULL = not featured; global shelves only
CREATE TABLE shelf_pins (
    user_id  INTEGER NOT NULL REFERENCES users(id)   ON DELETE CASCADE,
    shelf_id INTEGER NOT NULL REFERENCES shelves(id) ON DELETE CASCADE,
    pinned   INTEGER NOT NULL,   -- 1 pinned; 0 = "unpinned a featured shelf"
    position INTEGER NOT NULL,
    PRIMARY KEY (user_id, shelf_id)
);
```

**A user's rows** = their `pinned = 1` rows by `position`, then featured shelves they have no row for, by `featured_pos`. So featured shelves appear pinned by default, unpinning one stores a `pinned = 0` row that hides it for good (even if it's re-featured later), and re-pinning flips it back. Shelves they can't see (another user's personal shelf) never qualify; deletion cascades.

**Endpoints:**
- `PUT /api/me/pins/{shelf}` pin (appends to the end), `DELETE /api/me/pins/{shelf}` unpin, `PUT /api/me/pins` with an ordered id list to reorder (`requireUser`).
- `PATCH /api/shelves/{id}` accepts `featured: true|false` from admins, global shelves only, next to the rename/description it already handles. Featuring appends to the end of the featured order. App data only, so admin, not `-allow-writes`, as other global shelf edits.
- `PUT /api/admin/featured-shelves` with an ordered id list renumbers `featured_pos` in one transaction (`requireAdminUser`).

**Ordering:**
- **Admins** set the featured order: it's what signed-out visitors see and the default order for signed-in users who haven't reordered.
- **Users** reorder their dashboard rows. The first reorder writes a `pinned = 1` row for every shelf currently on their dashboard (featured ones included), so their order is complete and stable. A shelf featured after that is shown after their ordered rows, until they move it.
- **Users also order their own shelves on `/shelves`**: `ALTER TABLE shelves ADD COLUMN position INTEGER` (personal shelves, owner's order; NULL sorts last by name as today), `PUT /api/me/shelves/order`. Global shelves on that page follow the featured order, then name.
- One shared drag-to-reorder helper (`web/reorder.ts`: pointer events, no library) with Move up/down buttons as the keyboard and screen-reader path, used on the dashboard's **Edit rows** mode, `/shelves`, and the admin's featured list. Each drop sends the full ordered list, so concurrent edits from two tabs end with the last one's order, never a mix.

**Signed out**: `pages/discover/featured.json`: every featured shelf with its first 24 books, written from memory whenever a featured shelf changes (feature toggle, rename, books added or removed, deletion) and at startup. `featuredVersion` (an `atomic.Int64` like `pagesVersion`) goes in the Discover HTML, so the file is cached immutably and a change shows on the next page load.

**UI:**
- Shelf page and `/shelves` list: **Pin to home** toggle for signed-in users; admins also get **Feature** on global shelves.
- Dashboard row header: the shelf name links to `/shelf/N`; a ⋯ menu with Unpin and Move up/down. An **Edit rows** button at the bottom of the shelf rows switches them to compact drag handles.
- `/shelves` (admins): a **Featured** section at the top, in featured order, draggable.

### 5. Recommendations (in memory)

Seed set S = reading (w=2) ∪ finished (w=2) ∪ favorites (w=3) ∪ wanted (w=1).

1. **Tag affinity**: for each tag t on seed books, `score(t) = Σ w(book) × idf(t)`, `idf = ln(N / |tagBooks[t]|)`. Rare-but-shared tags beat "Fiction". Top 3 → `tags` in the answer (rows 4).
2. **Candidates**: union of posting lists of the top ~8 tags, capped (e.g. first 2,000 by shuffle rank). Score each by `Σ score(t)` over its tags, + bonus for a seed author. Exclude anything with a status or favorite. Top 24 → `recommended`; `because` = the seed book sharing the most tags.
3. **Next in series**: for each finished/reading book with a series, the next `series_index` not yet finished/reading → `series` (shown inside Recommended, first).
4. No seed (new user): omit `recommended`; the page shows popular tag rows and Shuffle only.

Cost is bounded by the candidate cap: a few thousand integer lookups. Cache per user for 60 s keyed by a per-user "marks version" (bumped in `setStatus`/`setFavoriteHandler`) plus the catalog pointer, so repeat loads are free.

Diversity: a small random jitter on scores, so the row changes between visits while the top matches stay near the front.

Denied tags (section 3) are dropped from every book's tag list before scoring.

### 5a. Local reading for signed-out visitors

With `-public-read`, signed-out visitors can read, and `web/reading-sync.ts` keeps their position in `cf-progress-<uuid>-<format>` (`{position, percent, updatedAt}`). Nothing reaches the server, so this row is built entirely in the browser:

- **Index**: `saveProgress` also writes `cf-local-reading` (`uuid → {format, percent, updatedAt}`) when not signed in, so Discover doesn't have to scan every localStorage key. On first run it's built once from existing `cf-progress-*` keys.
- **Book details**: `GET /api/books?uuids=a,b,…` (new, ≤50 uuids, catalog only, under the same catalog guard as the grid). Answers grid `Book`s for uuids still in the library; missing ones are dropped from the index. Short `max-age`, so the row is a single small request.
- **Continue reading (this device)**: entries with `percent < 0.98`, newest `updatedAt` first, resume link from the format (same rule as `resumeURL`). Labelled as this device's, with a hint that signing in keeps it everywhere.
- **Finished on this device** (`percent ≥ 0.98`): not shown as a row; used only to filter Shuffle.
- No recommendations, Want to read or pins.

### 5b. Bringing guest reading into an account

When a visitor who read signed out signs in or signs up, they're offered to add that reading to their account, and shown any conflicts with what the account already has, so neither side's place or notes are lost.

#### Done first: guest and per-user records kept apart (PR #98)

Per book, in localStorage: position (`progress-<uuid>-<format>`), bookmarks (`bookmarks-<uuid>`, `bookmarks-deleted-<uuid>`) and highlights (`annotations-<uuid>`).

- **Guest** records use the plain names (`cf-progress-…`). **Signed-in** records live under `cf-u/<username>/…` (`web/local-keys.ts`), and only those are sent. Guest data therefore never reaches an account except through the flow below.
- Guest positions are saved with `pending: false` and guest highlights and bookmarks with `synced: false`. The import reads those as-is.
- Records saved before #98 are under the plain names, so they're guest data too, including a signed-in user's changes that hadn't been sent. The review hides any that already match the account, so a user's own older copies don't show up as conflicts.
- Sign-out drops the user's synced records and keeps unsent ones under their name, so they never become guest data.
- **Still to do for the flow:** `cf-local-reading` (section 5a) indexes guest books only. The flow finds guest highlights and bookmarks by scanning for `cf-annotations-*` and `cf-bookmarks-*` keys. The scan runs once per sign-in, and only when the index or such a key exists.
- After a successful import the guest records are deleted (the account now has them, under `cf-u/<username>/` once the reader next opens the book). Skipped books keep their guest records.

#### The flow

1. **Trigger.** After any sign-in or sign-up (password, registration, GitHub/Google: they all redirect to a normal page), `nav.ts` sees a signed-in `/api/me` answer and checks for guest records. If there are any, it shows a dialog: *"You read 4 books on this device while signed out. Add them to your account?"* **Review** · **Not now** · **These aren't mine**.
   - **Not now** keeps the guest data and leaves a "Reading on this device" item in the user menu until it's resolved.
   - **These aren't mine** (shared devices) never offers this data to this account again (remembered per username), and leaves it for whoever it belongs to.
   - A **Discard** option inside Review deletes the guest data after confirmation.
2. **Preview.** `POST /api/me/import/preview` with a summary per book: each format's position, percent and `updatedAt`; annotation ids, anchors and `updated_at`; bookmark ids and positions (no note text). The server compares against `reading_progress`, `device_progress` (via `latestProgress`, so a KOReader position counts), annotations, bookmarks and `reading_status`, and answers per book:
   - **new**: the account has nothing for it.
   - **same**: the account already has this (hidden from the list).
   - **conflict**: both sides have a position for the same format and they differ.
3. **Review** (`/account/import`, or the dialog expanded): one row per book with its cover.
   - New books are ticked.
   - Conflicts show both sides: *This device: 62%, ch. 14 · Tue 14:03* against *Account: 40% · Kobo · Mon 21:10*, with the newer one selected (the rule sync already uses), and the user can pick the other.
   - **Annotations and bookmarks never conflict**: their ids are made per device, so both sides' are kept. The exception is the same passage highlighted on both sides (same format and anchor): shown as a duplicate, defaulting to keeping the account's and adding the device's note to it if they differ, so no text is lost.
   - Status: a guest book at ≥98% offers **Mark finished**. If the account already says finished, importing an older position doesn't change that.
   - **Sign-up** is the same flow with nothing to conflict: the dialog reads *"Keep the reading you did on this device?"*, ticked by default, one button.
4. **Apply.** `POST /api/me/import` with the chosen items, in one app-database transaction:
   - Positions: written with the time they were chosen at when the user picked the older side, so it becomes the newest on every device (including KOReader, which takes the newest of `reading_progress` and `device_progress`); otherwise their own `updatedAt`.
   - Annotations and bookmarks: the existing upsert rules (`annotationPutHandler`, newest `updated_at` wins), ids kept, so running it twice changes nothing.
   - Status: `setStatus(..., "auto")` as reading does today, `"manual"` for Mark finished.
   - Limits: 500 books per request, body size capped, `requireUser`.
5. **After.** The browser sets `owner` on what was imported and marks it synced; skipped books stay guest data. The answer lists failures per book, which stay in the list for another try.

The Discover "Continue reading" row switches from the device's guest list to `/api/me/home` as soon as the import finishes.

### 6. Routes and nav

- `/` → `discover.html`. `/latest` → today's `index.html` unchanged (Title "Latest").
- `/?page=N` (old bookmarks, the grid's own URLs) → 301 to `/latest?page=N`.
- `/shuffle` → signed out: `index.html` with `data-page-url="/pages/shuffle/page{n}.json?v=…"` (`IndexPageData` gains `PageURL` and `Title`); signed in: the per-user list from section 2. Both the same grid. Signed-out visitors land on Discover too (with the rows listed under Goal).
- Top bar: add **Latest** to the browse nav (`templates/topbar.html`, `Active "latest"`); brand link stays `/`.
- Optional server flag `-home=latest` for admins who want the old landing page.

### 7. Invalidation

| Event | Catalog | Static files |
|---|---|---|
| Upload, edit, bulk edit, merge, import, repairs | debounced rebuild from `refreshAllCaches` (section 1) | page1 as today; tag files rewritten after the rebuild, `pagesVersion` bumped; shuffle files untouched until restart |
| Remove / restore | rebuild keeps removed books as tombstones | `removeFromPages` / `restoreToPages` also handle shuffle and tag files |
| Group change | rebuild (`refreshAllCaches`) | rewrite tag files |
| Discover tag settings | recompute the eligible set (no rebuild) | rewrite tag files |
| Featured shelf change | — | rewrite `featured.json`, bump `featuredVersion` |
| Status / favorite change | bump user marks version (recs and `/shuffle` caches) | — |

## Browser

- **`web/discover.ts`** (new entry, `{{ asset "discover.js" }}`): fetches the pieces above, renders rows from `<template>` markup, reuses `modal.ts` (book popup via `#book/<uuid>`), `reading.ts` (card marks, resume buttons) and the card markup from `app-htmx.ts`.
- **Rows**: CSS `overflow-x: auto` with `scroll-snap`, fixed card size (same aspect ratio as the grid), ≤24 cards each, so no virtualization needed. Arrow buttons on wide screens; swipe on touch.
- **Covers**: factor `pumpCovers` out of `app-htmx.ts` into `web/covers.ts` (bundled into both entries per the one-file-per-entry rule). On Discover it prioritizes cards visible in the viewport and in each row's visible scroll window, then the next screenful; same `maxCovers` limits and cancellation.
- **Skeletons**: rows render as skeleton cards at their final size immediately (from the embedded index and localStorage copy of `/api/me/home`), so nothing shifts when data lands.
- **Empty states**: hide Reading/Want rows when empty for a signed-in user, but show a one-line hint ("Mark books as Want to read to see them here") the first time.
- Service worker: Discover HTML is not kept offline (same as the grid); `/offline` unchanged.

## Phases (each a PR)

1. **Catalog index** (section 1): built in `writeLibraryPages`, debounced rebuild from `refreshAllCaches`, `booksByUUIDs` and `bookExists` served from it. No visible change; startup time logged.
2. **Shuffle pages** + `/shuffle` grid (static signed out, per-user without finished books signed in); removal/restore handling. Tests: permutation covers every book once; finished books absent from the signed-in pages; `servePages` caching.
3. **Routes**: `/latest`, redirect `/?page=`, nav item, Discover shell with Shuffle (finished filtered) + Recently added, for everyone.
4. **`/api/me/home`**: Reading (activity order) + Want to read rows; localStorage copy. Signed-out local reading row (`cf-local-reading`, `/api/books`).
5. **Guest reading import**: ~~records kept apart per owner~~ (done, #98), then preview, review page and apply. Tests: preview classifications (new / same / conflict, KOReader position counted), choosing the older side makes it newest, re-applying is a no-op, duplicate highlights keep both notes.
6. **Pinned and featured shelves**: migration, pin endpoints, `featured` on `PATCH /api/shelves/{id}`, featured order, user pin order and `/shelves` order, `web/reorder.ts`, `featured.json`, shelf page and dashboard controls. Tests: featured-by-default, unpin sticks, re-feature doesn't override an unpin, first reorder freezes the list, other users' personal shelves never show.
7. **Tag rows + admin include/deny + cutoff setting**.
8. **Recommendations + next in series** (denied tags excluded).
9. **Invalidation polish**, docs (`ARCHITECTURE.md`, `FEATURES.md`), `-home=latest`.

## Decisions

- Shelves: users pin any shelf they can see; admins feature global shelves, which are pinned by default for signed-in users (unpinnable) and shown to signed-out visitors.
- Ordering: admins order featured shelves; users order their dashboard rows and their own shelves.
- Signed-out visitors get Discover, with this device's reading progress but no tailored recommendations.
- Signing in or up offers to add this device's guest reading to the account, with a review of conflicts; nothing guest-made reaches an account without that.
- Tag rows: 25% cutoff by default, adjustable; admin include and deny lists on top.
- Shuffle leaves out finished books.
- The dashboard's sections are in a fixed order for everyone (easier to explain and demonstrate); users reorder only their pinned shelf rows, within the shelves section.
- Phase 5's first half (record ownership) fixes a leak that exists today, so it ships on its own ahead of Discover.

## Open questions

- How big is the largest library this should stay fast on (books, tags), and on what storage? The catalog's memory (~25 MB per 100k books) and the rebuild-on-write approach are sized for up to a few hundred thousand books on an HDD. Much larger would need incremental updates from the start.
- Keep `-home=latest` (an admin can make `/latest` the landing page), or drop it to keep one layout everywhere?
