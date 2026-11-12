# Plan: Discover home page

Status: proposal. Done so far: guest and per-user reading data kept apart (first half of phase 5, PR #98); phase 1, the catalog (#99); metadata.db read-only without `-allow-writes` (#100); phase 1b, the catalog snapshot (#101); phase 2, Shuffle (#102); phase 3, the Discover page with the introduction, Shuffle and Recently added, and `/latest`.

Phase 3 as built: the Shuffle row's finished books come from the signed-in user's marks only; signed-out visitors' finished-on-this-device books are skipped once phase 4 indexes them (`cf-local-reading`). Only Latest was added to the top bar; Shuffle is reached from its row and the introduction. The grid's cover pump wasn't moved into a shared module: Discover has a small one of its own for its ≤24-card rows, sharing only `maxCovers` (`web/cover-limits.ts`) and `setupCoverStates` (now in `web/utils.ts`).

Phase 1b as built: the key adds `metadata.db`'s header change counter (a 28-byte read) to the size and modification time, since some network and FAT filesystems keep whole seconds and an in-place edit can keep the size. Outside writers are detected with `PRAGMA data_version` on `calibreDB`, which ends snapshot saving for that run. Still read from the library at every start: the aggregate overviews (`preloadCaches`: authors, publishers, tags, languages with counts) and `calibrefast-groups.json`. Authors and tags could come from the catalog; publishers and languages would need adding to it.

Phase 1 as built, where it differs from section 1 below:
- Per-book `TagIDs`/`AuthorIDs` are plain slices (the full build allocates them in passing). Arenas, tag postings, series lists and group resolution come with their first users (phases 2, 7, 8).
- `has_cover` is kept but not trusted for thumbnails: a missing `cover.jpg` is remembered after the first look (`noCover`), so a stale `has_cover = 0` never hides a real cover.
- `libraryIO` counts statements prepared on `metadata.db` (an SQLite authorizer on the `sqlite3_calibre` driver) and the cover and folder reads under `-basedir`. The startup build uses its own connection and isn't counted.
- The "every write endpoint" test is per path for now: the editor's Apply has an HTTP test; the other paths call `catalogChanged`/`catalogRemoved` beside their existing page updates. Format-name repairs change only `data.name`, which the catalog doesn't hold.
- Found while testing and fixed: after an upload only page 1 was rewritten and `totalBooks` kept its startup value, so new books could be hidden until a restart and books pushed off page 1 were on no page. The pages are now written again from the catalog after uploads and edits (`refreshLibraryPages`), only where a file changes.

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

### Same layout for everyone

Every visitor sees the same sections in the same order; there's no per-server or per-user choice of landing page. For anyone who isn't signed in, or has nothing tracked yet, the page doubles as an introduction to the app.

**Introduction band** (top of the page, in place of Continue reading when there's nothing to continue):
- The library at a glance: *48,211 books · 9,402 authors · 3,104 tags · 17 languages*. Counts come from the aggregate caches already in memory and are written into the HTML. These are counts of what the visitor can already browse; the page never shows anything about accounts (how many users, who is reading what).
- Search box, prominent (the same `/search`).
- Short "browse by" links: Authors, Tags, Languages, Shelves, Latest, Shuffle.
- Signed out: one line on what an account adds (*Track what you're reading on every device, keep a want-to-read list, get suggestions*), with **Sign in**, and **Create account** when registration is open. With `-public` but not `-public-read`, it also says reading needs an account.
- Signed in with nothing tracked: the same band without the sign-in line. The rows that would be empty (Continue reading, Want to read, Recommended) show a one-line how-to in their place, e.g. *Hover a cover and click the bookmark to add it here*. These hints are shown the same way for everyone, so the page is easy to demonstrate.
- The band shrinks to the stats line once the user has something in Continue reading, or after **Dismiss** (remembered per browser).

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

## Storage model

The deployment this is designed for: the **library** (book folders and `metadata.db`) is on slow spinning storage or a network drive, possibly asleep; **CalibreFast** runs on a fast local SSD with `-datadir`, `-cachedir` and `pages/`. Target: **100k books** and well beyond.

Rules, in order of preference:

1. **A page load reads nothing from the library.** Discover, `/latest`, `/shuffle` and every grid are served from memory or from files on the SSD. That means no SQL against `metadata.db`, no `stat`, and no directory listings.
2. **After a write, read back only what changed**, with indexed queries (`WHERE id IN (…)`) rather than table scans.
3. **Restarts don't rescan the library** when it hasn't changed (catalog snapshot, below).
4. **What's missing is remembered too** (no cover, no file), so it isn't looked up again on every request.

What this finds in today's code (to fix along the way; each is small):

| Hot path | Today | Change |
|---|---|---|
| `resumeURL` (`/api/me/reading`, and later the Continue reading row) | `resolveFormats` lists the book's folder on the library (`os.ReadDir`) the first time per run, per book | formats from the catalog (`data` table: format + name); folder listing only for books with no `data` rows (the `formats_from_folder` case) |
| `/cover-thumb/` for a book **without** a cover | every request `stat`s the missing `cover.jpg` on the library | catalog's `has_cover`: answer 404 at once (the grid already shows a placeholder) |
| `/cover-thumb/` cache miss | `os.Stat(cover.jpg)` twice (`loadThumb`, `bookCoverStamp`) | once |
| Startup | full read of `books` + per-book page writes | snapshot when unchanged (below) |

Cached thumbnails already avoid the library: the stamp comes from memory (`modTimeCache`) and the file from `-cachedir`.

**Check it stays that way**: a counter of library operations (`libraryIO`: SQL on `calibreDB`, `stat`, open and `ReadDir` under `-basedir`), logged with the startup line and testable. A test loads Discover, `/latest`, `/shuffle`, `/api/me/home` and a page of thumbnails that are already cached, and asserts the counter didn't move.

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
    HasCover    bool       // books.has_cover
    Formats     uint8      // bitmask: epub, pdf, … from the data table
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

**Building** (`buildCatalog(db)`): the `writeLibraryPages` query gains `b.id`; the books are collected as the pages are written. Then four more queries on the same connection, each a sequential read of a small integer link table plus its names: `books_tags_link`/`tags`, `books_authors_link`, `books_series_link`/`series`, `data` (book, format). Sequential reads matter on a spinning disk: one pass per table in rowid order, never per-book lookups. Log one line: `catalog: 48211 books, 3104 tags, 812 series in 140ms`.

**Memory**: roughly the size of `pages/*.json` (~250 B/book: ~25 MB at 100k books) plus 4 B per tag/author link. Two existing caches hold copies of the same strings (`uuidPathIndex`: 19 uses, `modTimeCache`: 6). Later they can read from the catalog instead, which roughly evens out. Not part of phase 1.

**Updating: incremental, by book.** A full rebuild after each edit would re-read every table from the library: at 100k books on a network drive, tens of MB per edit. Instead:

- The write paths already know which books they changed. Each calls `catalogChanged(uuids)` or `catalogRemoved(uuids)` next to its existing `refreshInPages`/`generatePage1`/`removeFromPages` call: upload, editor, bulk edit, merge, import approval, recycle, restore, library repairs, cover changes.
- `catalogChanged` re-reads just those books: one `books` query by uuid, then the link tables by `book IN (…)` (each has a `(book, x)` unique index, so these are index lookups, not scans). Batches of 500.
- It applies them to a **copy-on-write** catalog: the books slice and maps are cloned shallowly (pointers and ints: ~5 ms at 100k), the changed entries replaced, the affected tag, series and author postings rebuilt, and the pointer swapped. Readers never lock.
- Changes arriving together (a bulk edit's batches) are coalesced: callers enqueue uuids, and one goroutine applies everything queued at most every 250 ms.
- **Group changes** don't touch the library: they only re-map tag ids to groups in memory.
- New books get random positions in the shuffle. Removed books stay as tombstones until restart, as the page gaps do (`removeFromPages`).
- A missed call site only means a stale title or tag in a row until the next restart or external-change check, never a wrong book. `TestCatalogHooks` (below) lists the write endpoints and fails if one doesn't notify the catalog.

**Snapshot on the SSD.** After a build or change (debounced, 5 s), the catalog is written to `<cachedir>/catalog.snap`: a versioned binary encoding (`encoding/gob`, or a hand-rolled one if gob's reflection is slow at 100k entries; measure). Alongside it go the `stat` key of `metadata.db` and its `-wal`/`-journal` files (size + mtime) and the library's `PRAGMA data_version`/schema cookie.

- **Startup**: one `stat` of `metadata.db` (and its journal). If the key matches the snapshot's and the page files on the SSD carry the same stamp, load the snapshot (~30 MB from SSD, ~100 ms) and **skip `writeLibraryPages` entirely**. Nothing else is read from the library.
- If the key differs (Calibre desktop edited the library, an older CalibreFast version), do the full sequential build as today, in the background:
  - With a snapshot from before, serve from that snapshot while the build runs.
  - Without one, the server waits for the build, as it does today.

**External changes** (Calibre desktop or `calibre-server` writing the same `metadata.db` while CalibreFast runs; today only a restart picks them up):

- **Only while the site is in use**: at most once a minute, a page load triggers a background `PRAGMA data_version` on the held connection. It never blocks the request. A fixed timer would wake a sleeping drive every minute, all night; this way an idle server leaves the library alone.
- If another process committed, read the `books` table's `id, uuid, last_modified` in one sequential pass (~4 MB at 100k) and diff it against the catalog. Changed and new ids go through `catalogChanged`, missing ones through `catalogRemoved`, and the affected pages are rewritten.
- `-library-poll=0` turns it off; `-library-poll=10m` checks less often.
- Separate phase (10). SQLite's own locking over network filesystems is a known caveat and unchanged by this.

**First users** (phase 1, no visible change):
- `booksByUUIDs` (userdata.go:31) answers from the catalog: every `/me/*` list, shelves, and later `/api/me/home`. A miss falls back to the SQL it uses today (and counts in `libraryIO`, so tests show if it happens).
- `bookExists` (userdata.go:24), on every favorite and status change: catalog first, SQL on a miss.
- `resumeURL` and the thumbnail handler, per the table in **Storage model**.

**Tests** (`catalog_test.go`, on the `setupTestDB` fixture):
- After `writeLibraryPages`, every book in `metadata.db` is in the catalog with its exact tags, authors and series, in `timestamp DESC` order.
- Tag groups collapse member tags into one id.
- `booksByUUIDs` gives the same answer from the catalog as from SQL, including order and skipped missing uuids.
- `catalogChanged` after an editor save, a bulk edit and an upload gives the same catalog as a fresh full build (tags, authors, series, formats, cover), and keeps existing shuffle positions.
- A recycled book is tombstoned and absent from `booksByUUIDs`; a restored one comes back.
- `TestCatalogHooks`: every write endpoint in the router (behind `requireWrites`) leaves the catalog equal to a fresh build afterwards.
- Snapshot round-trip: write, reload, equal. A changed `metadata.db` key forces a build; an unchanged one reads nothing from the library (`libraryIO` = 1 stat).
- `libraryIO` stays at zero across the page loads listed under **Storage model**.
- `-race` run with handlers reading while changes are applied.

**Tag groups**: map each member tag id to its group (`groups.all("tag")`) when building `tagBooks`, so "Science Fiction"/"Sci-Fi" count as one, matching `/tags`.

### 2. Random order at startup

- `shuffle` = Fisher–Yates over `0..n-1` with `math/rand/v2` seeded per run: O(n), microseconds even for 100k books.
- Written as `pages/shuffle/pageN.json` (same `Book` shape, `booksPerPage` each) in the same `writeLibraryPages` pass and under the same `pagesVersion`, so `servePages` caches them for good.
- **Different on every load, never the same books twice in a row** (decided): the browser walks the shuffle order a strip at a time instead of jumping to random places.
  - It keeps a cursor in localStorage (`cf-shuffle`: `{version, at}`, `at` the index of the next book in the order). Each load shows the ~12 books from `at`, skipping finished ones, and moves `at` past them. At the end it wraps to the start. The pages it needs are the static, immutable-cached `pages/shuffle/pageN.json`, so the server still does nothing per load.
  - First visit, or a new `pagesVersion` (a restart made a new order): start at a random place in the order, so different visitors and devices don't all begin with the same strip.
  - Repeats then only come back after the whole library has gone by: N books ÷ 12 loads.
  - Small libraries (under ~24 books after finished ones are skipped): the strip can't avoid every book from the last load, so it starts right after where the last one ended and wraps. The overlap is as small as the library allows.
  - Storage blocked (private windows, some previews): fall back to a random place each load, which can repeat. Nothing breaks.
  - Signed in or not, the same files and cursor; the finished books to skip come from the `cf-reading` copy of `/api/me/reading` (signed in) or this device's finished books (signed out).
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

### 7. Invalidation

| Event | Catalog | Static files |
|---|---|---|
| Upload, edit, bulk edit, merge, import, repairs, covers | `catalogChanged(uuids)`: indexed re-read of those books only (section 1) | page1 as today; the tag files holding them rewritten, `pagesVersion` bumped; shuffle files untouched until restart |
| Remove / restore | `catalogRemoved` / `catalogChanged`: tombstone and back | `removeFromPages` / `restoreToPages` also handle shuffle and tag files |
| Group change | re-map tags to groups in memory (no library read) | rewrite tag files |
| External change (phase 10) | `data_version` check, `books` diff, then the same calls | affected pages rewritten |
| Any of the above | snapshot rewritten to `-cachedir` (debounced) | — |
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

## Cover thumbnails: fewer round trips (later)

Goal from the maintainer: a thumbnail the browser already has should cost **no request at all**. Today it often still costs one. Not urgent, but nothing in Discover may make it harder.

**What's in place:** `/cover-thumb/<uuid>?t=<last_modified>` is answered with `public, max-age=31536000, immutable`, and the stamp changes when the cover does (`coverChanged` sets `last_modified`), so a cached copy is always right.

**Likely sources of the requests that remain** (to measure first, in a browser's network panel and the server's log):
- `web/sw.ts` passes every request it doesn't handle, thumbnails included, through `respondWith(fetch(req))`. The HTTP cache should still answer, but every image wakes the service worker. Not intercepting `/cover-thumb/` would leave thumbnails to the browser alone.
- **Coverless books:** the 404 has no cache headers, so the browser asks again on every view (the server answers from memory, but it's still a round trip). A 404 for a given `?t=` could be cached the same way, since a new cover changes the stamp.
- **Cancelled downloads:** `pumpCovers` removes `src` from covers scrolled away mid-download; a partial response isn't cached, so the cover is requested again when it comes back.
- **HTTPS with a certificate the browser doesn't trust:** Chrome doesn't keep a disk cache for those pages at all.
- Beyond the HTTP cache: a client-side store of thumbnails (Cache Storage, keyed by the same URL), or one request per row or page of covers instead of one per cover.

**Constraints on everything Discover adds:**
- Every cover anywhere (grid, Discover rows, shelves, recommendations, the book popup) uses **the same URL for the same cover**: built by one helper (`web/utils.ts`) from the book's `last_modified` as the catalog and page files carry it. No per-view query parameters, no other thumbnail endpoint, so one cached copy serves every place a book shows up.
- Every Book JSON Discover sends (`/api/me/home`, shuffle and tag files, `featured.json`, `/api/books`) carries the same `last_modified` stamp as `pages/*.json`.
- The Discover cover pump (`web/covers.ts`, shared with the grid) keeps cancellation, but nothing new may add cache-busting (`?t=Date.now()`) outside the editor's just-uploaded preview.
- The service worker gains no new handling of `/cover-thumb/`.

## Phases (each a PR)

1. **Catalog index** (section 1): built in `writeLibraryPages`, incremental `catalogChanged`/`catalogRemoved` at the write paths, `booksByUUIDs`, `bookExists`, `resumeURL` and coverless thumbnails served from it, and the `libraryIO` counter. No visible change.
1b. **Catalog snapshot** on `-cachedir`: restarts with an unchanged library read one `stat` and skip writing the pages again.
2. **Shuffle pages** + `/shuffle` grid (static signed out, per-user without finished books signed in); removal/restore handling. Tests: permutation covers every book once; finished books absent from the signed-in pages; `servePages` caching.
3. **Routes**: `/latest`, redirect `/?page=`, nav item, Discover shell with Shuffle (finished filtered) + Recently added, for everyone.
4. **`/api/me/home`**: Reading (activity order) + Want to read rows; localStorage copy. Signed-out local reading row (`cf-local-reading`, `/api/books`).
5. **Guest reading import**: ~~records kept apart per owner~~ (done, #98), then preview, review page and apply. Tests: preview classifications (new / same / conflict, KOReader position counted), choosing the older side makes it newest, re-applying is a no-op, duplicate highlights keep both notes.
6. **Pinned and featured shelves**: migration, pin endpoints, `featured` on `PATCH /api/shelves/{id}`, featured order, user pin order and `/shelves` order, `web/reorder.ts`, `featured.json`, shelf page and dashboard controls. Tests: featured-by-default, unpin sticks, re-feature doesn't override an unpin, first reorder freezes the list, other users' personal shelves never show.
7. **Tag rows + admin include/deny + cutoff setting**.
8. **Recommendations + next in series** (denied tags excluded).
9. **Invalidation polish**, docs (`ARCHITECTURE.md`, `FEATURES.md`).
10. **External changes**: `data_version` polling, `books` diff, `-library-poll`.

## Decisions

- Shelves: users pin any shelf they can see; admins feature global shelves, which are pinned by default for signed-in users (unpinnable) and shown to signed-out visitors.
- Ordering: admins order featured shelves; users order their dashboard rows and their own shelves.
- Signed-out visitors get Discover, with this device's reading progress but no tailored recommendations.
- Signing in or up offers to add this device's guest reading to the account, with a review of conflicts; nothing guest-made reaches an account without that.
- Tag rows: 25% cutoff by default, adjustable; admin include and deny lists on top.
- Shuffle leaves out finished books.
- The dashboard's sections are in a fixed order for everyone (easier to explain and demonstrate); users reorder only their pinned shelf rows, within the shelves section.
- Phase 5's first half (record ownership) fixes a leak that exists today, so it ships on its own ahead of Discover.
- Scale and storage: 100k books as the target, and as far beyond as the design allows, with the library on slow spinning or network storage and CalibreFast on a local SSD. Page loads read nothing from the library; writes read back only what changed; unchanged restarts don't rescan.
- One layout for everyone, no `-home` option. Discover is the landing page and introduces the app to signed-out visitors when the server is public.
- **A private server volunteers nothing.** Without `-public`, signed-out visitors only ever see the sign-in page, which says nothing about the library or its users: no book or user counts, no covers, no featured shelves. The introduction band is for signed-in users there.

## Privacy checks

Everything this plan adds stays behind the existing deny-by-default guard (`withAuth` in `auth.go`: without `-public`, only `isPublicPath` is reachable signed out). In particular:

- **No new public paths.** `isPublicPath` is unchanged. `/pages/shuffle/*`, `/pages/discover/*` (tag rows, `featured.json`), `/shuffle`, `/latest`, `/api/books` and the Discover page itself all require sign-in on a private server, like `/pages/page1.json` today.
- **Sign-in and registration pages** gain nothing from Discover: no stats, covers or shelf names.
- **`/api/me`** (public path) keeps answering only about the caller. The guest-import endpoints are `requireUser`.
- **No user counts anywhere**, public or not. `featured.json` and tag rows describe books, never who reads them; recommendations are computed per user and returned only to that user.
- **Tests:** extend the existing guard tests (`guard_test.go`) with every new route. Signed out on a private server, each must answer 401 or redirect to `/login`, and the login page's HTML must contain no count or book title.
