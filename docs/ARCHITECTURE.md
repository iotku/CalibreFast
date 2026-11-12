# How CalibreFast works

See also: [FEATURES.md](FEATURES.md) for behaviour, [SECURITY.md](SECURITY.md) for the access model, and [notes/Performance.md](notes/Performance.md) for performance notes.

## Data sources

- **`metadata.db`** (Calibre's SQLite database) is the index for listing, searching and filtering.
- **`metadata.opf`** in each book folder is what the book page and the metadata editor read. Edits write both. The editor never sees library identifiers (`calibre`, `uuid`, a scheme-less `urn:uuid:`: `isLibraryIdentifier`), and a save keeps the book's own as they are (`updateBookInCalibreDB`).
- **`calibrefast-groups.json`** in the library root holds user-defined groups. It's the only file CalibreFast adds to a library. (The Recycling Bin is outside the library, in `-datadir` by default.)

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
  - pdf.js fetches what a document needs, a file at a time: WebAssembly image decoders (JPEG 2000, JBIG2) from `wasm/`, character maps for CJK fonts from `cmaps/`, the 14 standard fonts (which PDFs may use without embedding) from `standard_fonts/`, and a CMYK profile from `iccs/`.
  - To upgrade a library: download `https://registry.npmjs.org/<package>/-/<package>-<version>.tgz`, check it against `dist.integrity` from `https://registry.npmjs.org/<package>/<version>`, copy the same files into a new `<package>@<version>/`, update the paths, and delete the old directory.
  - Shared code: `web/dom.ts` (typed element lookups, `api()` fetch helper) and `web/types.ts` (shapes of the JSON the Go server sends; keep in sync with the Go structs named there). `web/globals.d.ts` describes the parts of htmx, epub.js and pdf.js the app uses.
- **Readers** (`/read`, `/read-pdf`). `templates/reader-parts.html` holds what both pages share; `web/reader.css` is their only stylesheet.
  - `web/reader-chrome.ts`: the top and bottom bars (they slide away together while reading; the page never reflows), the sidebar (Contents, Bookmarks, Search) and the settings sheet, and the theme.
  - **Preview** (`web/reader-preview.ts`, `?preview=1`, or `?import=<id>` for a book in the import queue): the metadata editor's **Read** button (`previewReadLink`). Nothing is saved: no position (which would set the reading status), bookmarks, highlights or kept copy, and the service worker doesn't keep the page. `ReaderPageData.Preview` marks the page (`data-preview`), which hides the bookmark and notes controls. Back only ever closes the tab (`leaveReader`): the editor opens it with `window.open` so a script may close it, and in-book jumps add no history entries (`keepsHistory`).
  - `web/reader-settings.ts`: reading settings, per device, in `localStorage` (`cf-reader-settings`). A control binds to a setting by attribute (`data-setting`, `data-value` or `data-step`), so a new setting is a default there plus markup.
  - **Themes** are sets of CSS variables on `:root[data-theme]`: `--page-*` for the book, `--chrome-*` for the bars. An inline script sets `data-theme` before the first paint. EPUB chapters are iframes, so `web/reader.ts` adds a stylesheet of its own to each (`contentCss`: colours read back from the variables, font, size, line height, alignment, hyphenation) through epub.js's content hook; settings that change how much fits on a page lay the chapter out again at the same place (`rendition.resize()`).
  - **Opening an EPUB** (`web/reader.ts`):
    - **In parts:** `web/epub-source.ts` reads the `.epub`'s zip directory from its end with an HTTP Range request to `/download/{uuid}/epub` (`http.ServeFile` handles ranges), then each file epub.js asks for with one more range, inflated by the browser's `DecompressionStream`. `If-Range` keeps the reads to one version of the file.
    - **epub.js glue** (`web/epub-open.ts`): epub.js is handed a book root that is never fetched (`/epub-virtual/{uuid}/`) and a `requestMethod` that reads from the source. Its own resource replacement is off, because it would read every image before the first page. A serialize hook points each chapter's images, media, stylesheets and their fonts at `blob:` URLs as the chapter is rendered.
    - **The rest of the book** downloads in the background: at once for books under 3 MB, otherwise after the first page. From then on every read is from memory, and page numbers (`locations.generate`) and search, which need every chapter, run.
    - **Kept in the browser** (`web/book-cache.ts`, IndexedDB `cf-books`): the whole file, its `Last-Modified` and the generated page numbers, so reopening needs no download (a `HEAD` checks for a newer file). Kept books are dropped when they haven't been opened for 7 days, or when their status goes back to "not reading" (the grid's `/api/me/reading` answer; the last status seen is kept with the book). They're also dropped on sign-out and when someone else signs in.
    - **Offline** (`web/sw.ts`, served at `/sw.js` by `sw.go` with this build's asset list in front, so each build installs a new one; HTTPS or localhost only). It keeps `/static/` files and opened books' `/read` pages, and shows `/offline` (the kept books) when a page can't be fetched. API calls aren't cached: the readers keep their own copies of positions, bookmarks and notes.
    - Browsers without `DecompressionStream` download the whole book first and open it with JSZip, as before.
  - **Input** on an EPUB page happens inside the chapter's iframe, so taps, swipes, the wheel and keys are listened for on each chapter's document (`rendition.on("rendered")`).
- **PDF reader** (`web/reader-pdf.ts`). One page or a pair is on screen at a time; `scale` is CSS pixels per PDF point.
  - `web/pdf-view.ts` draws. Each page is a box sized in CSS pixels, snapped to whole device pixels, holding:
    - a **base canvas** of the whole page at `scale × devicePixelRatio`, one canvas pixel per screen pixel. Past 8 million pixels it stays at the largest size that fits and is stretched;
    - only then, a **detail canvas**: the part of the page on screen (plus a margin) at full resolution, drawn through pdf.js's `transform` render option. Its size depends on the window, not the zoom, so any zoom stays sharp and under browsers' canvas limits;
    - pdf.js's **text layer** (`TextLayer`; its CSS is in `reader.css`) and the **links** (`getAnnotations`, drawn as plain `<a>`s). Both are placed in percentages and sized by `--total-scale-factor`, so zooming never rebuilds them.
  - A zoom gesture only changes CSS sizes (the canvases stretch); 140 ms after the last change the canvases are drawn again. Scrolling a zoomed page redraws the detail canvas the same way. New canvases are drawn off screen and swapped in, so nothing blanks.
  - `web/pdf-gestures.ts`: pinch (touch events; `touch-action: pan-x pan-y` leaves one-finger panning to the browser), Ctrl/⌘+wheel and Safari's gesture events, mouse drag, swipe. Zoom is anchored: the point under the fingers is put back under them by scrolling.
  - `web/pdf-loupe.ts`: the magnifier. It asks `pdf-view` to draw a region of the page at the lens's own resolution, as a tile three times the lens's size that slides under it, and draws a new tile near the edge.
  - **No dark page mode.** PDF pages keep their own colours in every theme; only the surroundings and bars change. pdf.js's `pageColors` render option was tried for it and turned down: it remaps images too, so a colour photo comes out as flat greys.
- **Highlights and notes.** `web/annotations.ts` is the data and its sync, `web/annotation-ui.ts` the toolbar, note editor and Notes tab; each reader finds selections and draws highlights itself.
  - **Anchors** survive zoom and reflow. EPUB: a CFI range (epub.js's `selected` event gives it, and draws the highlight back from it). PDF: `{"page":N,"rects":[[x0,y0,x1,y1],…]}` in PDF points from the page's top left, one rectangle per line, drawn in percentages of the page under the text layer.
  - **Sync** (`annotations.go`, `GET`/`PUT /api/me/annotations/{uuid}[/{id}]`): ids are made in the browser; every change carries the device's clock and the newest wins. A delete is a change too: the row stays as a tombstone with its text cleared, so a device that was offline drops its copy instead of sending it back. Where an annotation is never changes after it is made.
  - **KOReader** (`koreader_annotations.go`; [KOREADER.md](KOREADER.md) has the steps). Its sync protocol has no annotations, but it has a file of its own for moving them between devices, `<book file>.annotations.lua`, which it merges in when a book is opened. `GET`/`POST /api/me/annotations/{uuid}/koreader?format=` write and read that file.
    - `koreader_lua.go` reads the file as data: a parser for tables, strings, numbers and booleans. Nothing is executed; anything else is a syntax error.
    - `koreader_text.go` converts exact positions in an EPUB. A CFI counts UTF-16 units of the text as it is in the file; crengine's XPointer counts characters of the text as crengine stores it (line breaks and tabs become spaces, runs of spaces collapse, white space that opens a block is dropped). A chapter is parsed once and a place is a text node and offset, from which either form is written.
    - **Import** uses the CFI newer KOReader saves beside its XPointers; otherwise the XPointers, checked against the highlighted text, and failing that a search for the text near them. Ids come from KOReader's identity for a highlight (place and creation time), so a second import changes nothing, and a tombstone keeps a highlight deleted here from coming back.
    - **Export** dates the file 1970, because KOReader's import removes its own annotations that are older than the file and not in it.
    - A PDF's rectangles are in points from the page's top left on both sides.
- **Top bar.** Every page except the readers uses the shared bar in `templates/topbar.html`: `{{ template "topbar-start" (topbar …) }}`, the page's own tools (pager, Group button), then `{{ template "topbar-end" … }}`. The `topbar` template function (main.go) takes key/value pairs: `Active` nav item, `Title`, `Query`, and so on.
  - The signed-in part (Want to read, ★ Favorites, Reading, user menu) is filled by `web/nav.ts` from `/api/me`, so pages stay identical for everyone and cacheable. `nav.ts` also opens and closes the ☰ menu, user menu and mobile search.
  - Wide screens show everything inline. At 1000px or less the browse links move into ☰; at 640px or less search becomes an icon and Want to read/Favorites/Reading move into the user menu.
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
- **Book pages** use an `ETag` built from the OPF modification time, the groups version and whether the viewer may edit and the cover's modification time, with `no-cache`: every open revalidates (a cheap 304), so an edit or new cover shows at once.
- **Full covers** (`/cover/`) are revalidated on every request; the book page links them with `?t=` set from `cover.jpg`'s modification time, so a new cover gets a new URL. A new or reverted cover also sets `last_modified` and `has_cover` in `metadata.db` (`coverChanged` in `covers.go`), as Calibre does.

## Writes

- **Guards.**
  - Every endpoint that changes the library is wrapped in `requireWrites` (`main.go`), which requires `-allow-writes` and a signed-in admin.
  - The whole server is wrapped in `csrfProtect`, Go's `http.CrossOriginProtection`.
- **Read-only mode** writes only CalibreFast's own caches (`./pages` and `-cachedir`). `-watch-dir` is ignored without `-allow-writes`: taking files out of a folder is a change too.

## Watch folder and import queue

`imports.go`. `-watch-dir` books wait outside the library until an admin adds them.

- **Watching** (`folderWatcher.run`): [fsnotify](https://github.com/fsnotify/fsnotify) (inotify, kqueue, ReadDirectoryChangesW) on the folder and each subfolder, since watches aren't recursive; a scan finds new subfolders and they are watched after it. An event schedules a scan (`watchDebounce`), and scans repeat every `watchSettle` only while files are settling, so an idle folder is never read. If a watch can't be set up or kept (`poll`), the folder is scanned every `watchPollInterval` (30 s) instead. `-watch-dir-force-polling` goes straight to that, for mounts whose watches succeed but stay silent, which can't be detected.
- **Scanning** (`folderWatcher.scan`): a walk of the folder. Events only say when to look. A file is finished when its size and modification time have been the same for `watchSettle` (and in at least two scans). Files are grouped by folder and name without extension; a group is taken when all of it is finished. A file that can't be taken is logged once and left until it changes.
- **The queue** (`importInbox`) is the folders under `<datadir>/imports/<id>/`: the book's files under their own names, `item.json` (a `pendingImport`: what each file says, and their `mergeMeta` as the starting metadata) and `cover.jpg`. Not in the app database: a waiting book is its folder. A folder without a valid `item.json` is logged and skipped, never deleted.
- **Review** is the metadata editor. `templates/book-edit.html` renders for a waiting book when `EditBookPageData.Import` is set, and the browser code takes a `BookTarget` (`web/book-target.ts`): the addresses for a library book (`libraryBook`) or a waiting one (`pendingImport`). `web/metadata.ts`, `isbn-tools.ts`, `cover-dialog.ts` and `cover-sources.ts` only use those, so both kinds share one editor.
  - `/import/{id}/metadata`, `/isbns`, `/formats`, `/file/{format}`, `/cover`, `/cover/status`, `/cover/revert` mirror the library's endpoints, on the queue's folder. Cover saving is shared (`readCoverUpload`, `saveCoverJPEG`, `revertCoverIn` in `cover_upload.go`).
  - **Covers from the files** (`import_covers.go`): `stage` calls `extractImportCover`, and `backfillImportCovers` runs at startup for books staged without one. The EPUB's declared cover (EPUB 3 `cover-image`, EPUB 2 `<meta name="cover">`, or an image item with id `cover`), else the PDF's first page when its largest picture (pdfcpu `ExtractImagesRaw`) is big and has the page's shape (`pdfImageIsPage`). Go can't render PDF pages, so pages of text and vectors are left to the browser. Saved at most 1200px wide and only if there's no `cover.jpg` (`O_EXCL`), so a cover chosen meanwhile is never replaced.
  - `mountCoverDialog`'s `autoDefault` gives a waiting book the server found no cover for the EPUB's cover or the PDF's first page, rendered in the browser. A book with a cover isn't loaded until the cover dialog opens.
- **Adding** (`POST /import/{id}/apply` → `importInbox.approve`): `addBookToLibrary` (`upload.go`, shared with uploads: the row in `metadata.db`, `metadata.opf`, the files moved in; undone if a step fails, with the files moved back), then the cover, then `updateBookInCalibreDB`, the same write as the editor's Apply. The book is taken out of the queue first, so two requests can't both add it, and put back if adding fails.
- `/api/me` reports `pending_imports` to admins, for the top bar (`web/nav.ts`). `/import` is `templates/imports.html` and `web/imports.ts`, which polls `/api/imports`.

## Removing books and the Recycling Bin

`recycle.go`; behaviour in [FEATURES.md](FEATURES.md#removing-books-and-the-recycling-bin).

- **Removing** (`POST /api/library/delete` → `recycleBooks`): under `recycleMu`, a new batch folder; `VACUUM INTO` copies `metadata.db` there first (no copy, no removal). Then each book's folder is moved in (`moveDir`: a rename, or copy and remove across filesystems), then one transaction deletes the rows (`deleteBooksFromCalibreDB`). A failed move or delete moves the folders back and drops the batch.
  - **Rows:** every table with a `book` column (`tablesReferringToBooks`, from `sqlite_master`, skipping virtual tables), so custom columns and tables newer Calibre versions add go too; then authors, publishers, series and tags nothing links to.
  - `books.path` is checked to be strictly inside the library (`libraryBookDir`): an empty path would be the whole library.
- **Restoring** (`POST /api/recycle-bin/{batch}/restore` → `restoreRecycled`): the folder moves back, then `restoreBookRows` attaches the batch's `metadata.db` as `bak` on a connection of its own and copies the book's rows in one transaction: the `books` row (its old id unless taken; then the uuid set back, since Calibre's insert trigger replaces it), plain tables with `book` changed, and link tables through `linkedEntityOf`, which maps each linked item to the library's by name (`restoreEntity`), copying it from `bak` when missing. Restored books are marked in `batch.json`.
- **Afterwards** both rewrite every `pages/*.json` (`writeLibraryPages`; a removal can move every book's page), clear the per-book caches (`forgetBook`) and refresh the overviews.
- **No web endpoint deletes from the bin.** `-empty-recycle-bin` (`emptyRecycleBin`) runs from the command line and exits.
- **Browser:** `web/delete-books.ts` is the confirmation (type `delete`) and Undo; `web/select-books.ts` adds **Select** to the grid's top bar once `nav.ts` says the viewer can edit (`data-can-edit` on `<html>`, `cf:me` event), and marks cards as `app-htmx.ts` renders them; `web/user.ts` wires the book page's button; `web/recycle-bin.ts` is `/recycle-bin`.

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

- `appdb.go`: CalibreFast's own SQLite database in `-datadir`, with versioned migrations (users, identities, sessions, favorites, reading_status, reading_progress, bookmarks, annotations, shelves, shelf_books, login_links, sync_keys, device_progress, book_digests). Migrations are one ordered list: add new ones at the end.
- `auth.go`: passwords, sessions and the `withAuth` middleware that puts the user in the request context. The guards are `requireUser`, `requireAdminUser`, `requireReader` (book contents unless `-public-read`) and `requireWrites`.
- `oauth.go`: GitHub/Google sign-in.
- `userdata.go`: favorites, reading status and the `/me/...` lists. `progress.go`: reader position and bookmarks. `annotations.go`: highlights and notes. `shelves.go`: bookshelves.
- `bookprogress.go`: the newest position per book (web readers or a matched KOReader device; nothing is hashed). It serves `/api/me/reading` (books being read and where to resume them, plus the user's favorite, wanted and finished books), which `web/reading.ts` uses to mark cards in the grid and draw the buttons over their covers, and caches in localStorage, and the book popup's "Page 5 of 25 · synced …" note. PDF page counts come from pdfcpu, which reads only the page tree, cached per file.
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
