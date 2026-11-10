# How CalibreFast works

See also: [FEATURES.md](FEATURES.md) for behaviour, [SECURITY.md](SECURITY.md) for the access model, and [notes/Performance.md](notes/Performance.md) for performance notes.

## Data sources

- **`metadata.db`** (Calibre's SQLite database) is the index for listing, searching and filtering.
- **`metadata.opf`** in each book folder is what the book page and the metadata editor read. Edits write both. The editor never sees library identifiers (`calibre`, `uuid`, a scheme-less `urn:uuid:`: `isLibraryIdentifier`), and a save keeps the book's own as they are (`updateBookInCalibreDB`).
- **`calibrefast-groups.json`** in the library root holds user-defined groups. It's the only file CalibreFast adds to a library.

## Main library pages

On startup CalibreFast queries `metadata.db` and writes paginated JSON (`pages/page1.json`, `page2.json`, …, `booksPerPage` = 60 books each). The frontend fetches these directly as static files, so browsing never hits the database. `pages/page1.json` is regenerated after uploads and edits.

Each book's `authors` is its authors' names in Calibre's order, `A & B` (`bookAuthorsSQL` in `authors.go`), in every grid: not `books.author_sort`, which is the "Last, First" sort form. `authors.go`'s `splitAuthors` reads author lists (editor, uploads) on `&` only; `web/authors.ts` reads the editor's field the same way. Other ways of writing them (`;`, `Last, First`) are only rewritten by the editor's Split button (`tidyAuthors`).

`index.html` asks for `pageN.json?v=<pagesVersion>`, a stamp that changes whenever the files are rewritten, so with a matching `v` they're cached for good (`servePages`). Anything else is revalidated.

## Filtered views, overview pages and search

- These query SQLite directly.
- **Pre-computed at startup and refreshed after uploads, edits and group changes:**
  - the overview lists (with groups collapsed and distinct book counts)
  - each filtered view's page count, cached in memory
- **Categories.** Authors, publishers, tags and languages are described in one table (`categories` in `aggregate.go`), so all four share the same pages and endpoints. Languages differ only in their column names (`lang_code`).

## Frontend

- **TypeScript and CSS, built by Go tools.** Browser code is TypeScript in `web/`, along with the stylesheet (`web/style.css`). There's no Node, npm or `package.json`.
  - `go generate` runs two steps, both pinned as Go tools in their own module, `tools/go.mod`, so the server itself still builds with Go 1.25 and none of their dependencies. Working on the browser code needs Go 1.26+, which `tsgo` requires.
    1. `tsgo`, the Go-native TypeScript compiler, type-checks `web/` (`tsconfig.json`: strict mode, no unused locals/parameters, every file is a module). It writes nothing.
    2. `tools/jsbuild` uses esbuild's Go API to bundle each entry with its imports, minify it, and write it to `static/dist/` named by content hash (`app-htmx-NGM5EDXU.js`), with a source map and `manifest.json`.
  - **Entries are what the templates use**, via `{{ asset "app-htmx.js" }}` (`web/app-htmx.ts`) or `{{ asset "style.css" }}` (`web/style.css`). Inline module scripts import the same way: `import … from {{ asset "metadata.js" }};`. jsbuild finds the names by scanning `templates/`, so adding a script to a page is just the template change plus `go generate`.
  - One file per page script, with no import waterfall. Code shared by two entries is copied into both, so module state isn't shared between them: e.g. the book modal's user controls come bundled inside `app-htmx.js`, and `book.html` loads `user.js` only when the page is opened on its own.
  - `static/dist/` is committed, so `go run .` works without a build step. **Don't edit it by hand.** `TestTypeScriptUpToDate` type-checks and rebuilds into a temporary directory and fails if the result differs from the committed files.
  - To update the tools: `cd tools && go get -tool github.com/microsoft/typescript-go/cmd/tsgo@latest` (or `go get github.com/evanw/esbuild@latest`), then `go generate` and run the tests.
- **Third-party libraries** (htmx, JSZip, epub.js, DOMPurify, pdf.js) are committed in `static/vendor/<package>@<version>/`: the files the pages use, at their paths in the npm package, plus its license. Templates link them by path; scripts that load one themselves take the path from `web/vendor.ts`, which esbuild leaves alone. Serving them ourselves saves a cold page load the DNS, TCP and TLS of another host, and browsers no longer share a CDN's cache between sites anyway.
  - `TestVendor` checks that every `/static/vendor/` path in `templates/` and `web/` exists, that every package is used, and that no script comes from another host.
  - pdf.js fetches its WebAssembly image decoders (JPEG 2000, JBIG2) from `wasm/` only when a page needs them.
  - To upgrade a library: download `https://registry.npmjs.org/<package>/-/<package>-<version>.tgz`, check it against `dist.integrity` from `https://registry.npmjs.org/<package>/<version>`, copy the same files into a new `<package>@<version>/`, update the paths, and delete the old directory.
  - Shared code: `web/dom.ts` (typed element lookups, `api()` fetch helper) and `web/types.ts` (shapes of the JSON the Go server sends; keep in sync with the Go structs named there). `web/globals.d.ts` describes the parts of htmx, epub.js and pdf.js the app uses.
- **Readers** (`/read`, `/read-pdf`). `templates/reader-parts.html` holds what both pages share; `web/reader.css` is their only stylesheet.
  - `web/reader-chrome.ts`: the top and bottom bars (they slide away together while reading; the page never reflows), the sidebar (Contents, Bookmarks, Search) and the settings sheet, and the theme.
  - `web/reader-settings.ts`: reading settings, per device, in `localStorage` (`cf-reader-settings`). A control binds to a setting by attribute (`data-setting`, `data-value` or `data-step`), so a new setting is a default there plus markup.
  - **Themes** are sets of CSS variables on `:root[data-theme]`: `--page-*` for the book, `--chrome-*` for the bars. An inline script sets `data-theme` before the first paint. EPUB chapters are iframes, so `web/reader.ts` adds a stylesheet of its own to each (`contentCss`: colours read back from the variables, font, size, line height, alignment, hyphenation) through epub.js's content hook; settings that change how much fits on a page lay the chapter out again at the same place (`rendition.resize()`).
  - **Input** on an EPUB page happens inside the chapter's iframe, so taps, swipes, the wheel and keys are listened for on each chapter's document (`rendition.on("rendered")`).
- **Top bar.** Every page except the readers uses the shared bar in `templates/topbar.html`: `{{ template "topbar-start" (topbar …) }}`, the page's own tools (pager, Group button), then `{{ template "topbar-end" … }}`. The `topbar` template function (main.go) takes key/value pairs: `Active` nav item, `Title`, `Query`, and so on.
  - The signed-in part (★ Favorites, Reading, user menu) is filled by `web/nav.ts` from `/api/me`, so pages stay identical for everyone and cacheable. `nav.ts` also opens and closes the ☰ menu, user menu and mobile search.
  - Wide screens show everything inline. At 1000px or less the browse links move into ☰; at 640px or less search becomes an icon and Favorites/Reading move into the user menu.
  - A book page opened directly (no `HX-Request` header) gets the bar; the modal's copy doesn't (`Vary: HX-Request`).
- **Scroll engine.** `web/app-htmx.ts` drives the main library, filtered views and search.
  - **One grid, addressed by book index.** Pages are only how the data arrives. Every row is the same height (cards have a fixed aspect ratio), so row `r` starts at `r × rowH`, and a book's place follows from its index and the column count. `measure()` reads both from the layout.
  - **Only the rows around the screen are in the DOM**, between two `.grid-spacer`s sized to the rows they stand for. Moving the window adds and removes rows at its edges and resizes the spacers by the same amount, so nothing on screen moves and there's no scroll compensation. The spacers are painted as empty cards: a CSS mask of one row, built from the measured layout and repeated down.
  - **Books whose page hasn't arrived are `.skeleton` cards**, replaced in place when it does.
  - **Fetching.** Up to 2 pages at a time: the screen first, then ahead in the scroll direction (at least 2 screens, more at speed: 2 fetch-times of scrolling), then behind. Pages no longer wanted are aborted. While pages go by faster than they can be fetched, nothing is fetched until scrolling stops.
  - **Position.** The URL has `?page`, the page of the top row's last book (so going to page N shows N). `history.state` has the exact place: `at` (first book of the top row) and `frac` (how far into that row). A reload or back/forward on the same `?page` returns there. A width change keeps `at` at the top: a `ResizeObserver` lays out again after layout and before paint.
- **Data source.** Each page's HTML sets attributes on `#books`:
  - `data-page-url`: a URL template with `{n}` for the page number
  - `data-page-size` (`booksPerPage`)
  - `data-total-books`
  - `data-empty-text`

  The same script therefore works with the static JSON files and with the API endpoints.
- **Book popup** (`web/modal.ts`) loads `/book/get/<uuid>` through htmx.
  - **Preloading.** Hovering a card preloads the book (`hx-preload`) and its per-user controls (`/book/me/<uuid>`, `prefetchControls` in `web/user.ts`). The card's click is only the hash change (`hx-trigger="none"`). `loadBook` makes the one request with the card as htmx's `source`, which reuses the preload even while it's still in flight. Without a hover, the controls are fetched alongside the book rather than after it.
  - It's a `#book/<uuid>` history entry, closed by going back. While it's open the grid doesn't write `?page` to the URL: going back only closes it if both entries have the same `?page` (otherwise there's no `hashchange`). The page is written once it closes.
  - The modal follows the URL on both `hashchange` and `popstate`, so back/forward between entries with different `?page` still opens and closes it.
  - htmx's own history is off on these pages (`<meta name="htmx-config" content="history:false">`). Otherwise, on back/forward it re-fetches the page and replaces the whole body, grid included.

## Caching

- **Cover thumbnails** are generated on first request and stored in `-cachedir`. The server handles one thumbnail request per core at a time (`imageSem` in `covers.go`, sized by `GOMAXPROCS`, which follows container CPU limits).
  - A request the browser cancels (a cover scrolled away) stops before its next expensive step: waiting for a slot, reading and decoding, scaling, encoding.
  - Its thumbnail goes to a backlog (`thumb_backlog.go`), made one at a time once no thumbnail requests have come in for 2 seconds. A new request stops the backlog's current one, which goes back in the queue.
- **Cover loading in the browser** (`pumpCovers` in `web/app-htmx.ts`): covers on screen first, a few at a time (`maxCovers`: 4 over HTTP/1.1, which leaves room for page JSON under the browser's 6 connections per host; 10 over HTTP/2 or 3; 2–4 when Chromium estimates a 2G/3G link); off-screen covers only start once the viewport is done, and in-flight ones that scroll away are cancelled (by removing `src`) and re-queued. While the book modal loads, grid covers are cancelled and wait until every image in the modal has arrived (`cf:modal-busy` from `web/modal.ts`).
- **Scripts, styles and libraries** (`/static/dist/` and `/static/vendor/`, `assets.go`) are held in memory, gzipped once at startup, and served with `Cache-Control: public, max-age=31536000, immutable`. Their URLs change with their content (a content hash, or the library's version), so returning visitors make no requests for them until a new build. A restart picks up a new build.
- **Book pages** use an `ETag` built from the OPF modification time, the groups version and whether the viewer may edit, plus `max-age=30`.

## Writes

- **Guards.**
  - Every endpoint that changes the library is wrapped in `requireWrites` (`main.go`), which requires `-allow-writes` and a signed-in admin.
  - The whole server is wrapped in `csrfProtect`, Go's `http.CrossOriginProtection`.
- **Read-only mode** writes only CalibreFast's own caches (`./pages` and `-cachedir`).

## ISBNs and online lookups

Both are behind `requireWrites` and only add options to the metadata editor (`web/isbn-tools.ts`). They never write.

- **`/metadata/{uuid}/isbns`** (`isbn.go`) finds ISBNs in the book's files. GET reads EPUB (package identifiers, then the first 5 and last 3 spine documents' visible text), MOBI and AZW3 (EXTH record 104). It names the book's PDF, if any.
  - Go has no reliable PDF text extraction, so the editor reads the PDF with pdf.js: the XMP packet and the first and last few pages, with range requests and `disableAutoFetch`. It POSTs them to the same endpoint.
  - All parsing, check digits, ISBN-13 normalisation, labels and ranking happen on the server, so there's one implementation.
- **`/metadata/lookup?source=openlibrary|wikidata|google&isbn=…`** (or `&title=…&author=…`, not for Wikidata) (`lookup.go`) asks one source per request. The editor asks Open Library; Wikidata only if Open Library doesn't know the ISBN; others when the admin clicks **Try …**. The answer is mapped to an `UploadedBookMeta` (Format "Open Library", "Wikidata", "Google Books"), which the editor adds as one more source, with `External` set, once the admin keeps it from the comparison dialog (a rejected answer never reaches the form).
  - **Open Library:** `/isbn/{isbn}.json` (the edition), then its work (description, subjects) and authors. Without an ISBN, `/search.json`.
  - **Wikidata:** two SPARQL queries on `query.wikidata.org`. The first finds the edition by ISBN (P212/P957) and its work (P629). Wikidata stores ISBNs hyphenated, and where the hyphens go depends on the ISBN agency's ranges, so it asks for every possible hyphenation (`isbnHyphenations`, under 60): an exact match is an index lookup, a normalised one a timeout. The second reads both items' values one per row (title, date, authors with P1545 order, publisher, language codes, genre and main subject labels), with the QIDs written in as constants for the same reason.
  - **Google Books:** `/volumes?q=isbn:…`, with `CF_GOOGLE_BOOKS_KEY` if set. No covers.
  - Answers, including "no match", are cached in memory for 24 hours (500 entries). Errors aren't.
  - **Limits** (`serviceLimit`, one per site): a token bucket and a daily cap (Open Library 20 then 1/s, 1,000 a day; Wikidata 10 then 1 per 2s, 500 a day; Google 5 then 1 per 10s, 100 a day). A 429 pauses that site for its `Retry-After`. Over a limit, requests are refused with a message, not queued.
  - Requests carry `User-Agent: CalibreFast/<version> (+https://github.com/iotku/CalibreFast)` and nothing about the library or user. `-no-lookups` refuses both endpoints and hides the lookup; `-no-google-books` refuses Google.
- **`/metadata/lookup/cover?src=openlibrary&id=…`** fetches a lookup's cover for the editor's cover dialog (`web/cover-dialog.ts`), only when the admin clicks **Look up cover online**. That button asks `/metadata/lookup?source=openlibrary` for the ISBN in the top bar's lookup box (cached, so a later lookup of it is free) and adds any cover from a kept lookup. The server builds the URL from a cover id it checks, so it can't be pointed elsewhere. The browser never contacts the third party, and the canvas the dialog re-encodes from isn't cross-origin.

## Accounts and per-user data

- `appdb.go`: CalibreFast's own SQLite database in `-datadir`, with versioned migrations (users, identities, sessions, favorites, reading_status, reading_progress, bookmarks, shelves, shelf_books, login_links, sync_keys, device_progress, book_digests). Migrations are one ordered list: add new ones at the end.
- `auth.go`: passwords, sessions and the `withAuth` middleware that puts the user in the request context. The guards are `requireUser`, `requireAdminUser`, `requireReader` (book contents unless `-public-read`) and `requireWrites`.
- `oauth.go`: GitHub/Google sign-in.
- `userdata.go`: favorites, reading status and the `/me/...` lists. `progress.go`: reader position and bookmarks. `shelves.go`: bookshelves.
- `bookprogress.go`: the newest position per book (web readers or a matched KOReader device; nothing is hashed). It serves `/api/me/reading`, which `web/reading.ts` uses to mark cards in the grid and caches in localStorage, and the book popup's "Page 5 of 25 · synced …" note. PDF page counts come from pdfcpu, which reads only the page tree, cached per file.
- The app database stores book UUIDs; book details for lists come from `metadata.db`.

## KOReader progress sync

[KOREADER.md](KOREADER.md) covers setup. Under the hood:
- `kosync.go`: KOReader's sync API under `/kosync`, the per-user sync keys, and joining device positions with the web readers'.
  - Devices authenticate with headers, so `/kosync/` is a public path to `withAuth`.
  - Device positions are kept raw in `device_progress`, one per (user, document id), last writer wins, as KOReader's reference server does.
  - Nothing is copied between `device_progress` and `reading_progress`. Each read (the web reader's `GET /api/me/progress`, a device's `GET /kosync/syncs/progress/…`) takes the newest of the two for that book format and converts it.
- `kosync_digest.go`: maps KOReader's document ids to library files (`book_uuid`, format).
  - **Binary ids:** KOReader's partial MD5. It's computed lazily, when a file is opened in the web reader or downloaded, and cached in `book_digests` with the file's size and mtime, so a changed file is hashed again.
  - **Filename ids:** the MD5 of the file name. They come from Calibre's `data` table, rebuilt in memory at most once a minute.
  - When a device asks about an id nothing matches, the files the user has read on the web but never hashed are hashed in the background.
- `kosync_position.go`: EPUB position translation.
  - KOReader's `/body/DocFragment[N]/…` XPointer and the web reader's CFI both name the spine item: DocFragment N is the N-th spine itemref with a manifest entry.
  - Inside the chapter, the element path is followed in the chapter's XHTML. It converts to that element when the path resolves, otherwise to the chapter start. Character offsets are dropped.
  - PDF positions are page numbers on both sides.
