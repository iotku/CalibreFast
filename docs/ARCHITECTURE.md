# How CalibreFast works

See also: [FEATURES.md](FEATURES.md) for behaviour, [SECURITY.md](SECURITY.md) for the access model, and [notes/Performance.md](notes/Performance.md) for performance notes.

## Data sources

- **`metadata.db`** (Calibre's SQLite database) is the index for listing, searching and filtering.
- **`metadata.opf`** in each book folder is what the book page and the metadata editor read. Edits write both.
- **`calibrefast-groups.json`** in the library root holds user-defined groups. It's the only file CalibreFast adds to a library.

## Main library pages

On startup CalibreFast queries `metadata.db` and writes paginated JSON (`pages/page1.json`, `page2.json`, …, `booksPerPage` = 60 books each). The frontend fetches these directly as static files, so browsing never hits the database. `pages/page1.json` is regenerated after uploads and edits.

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
  - Shared code: `web/dom.ts` (typed element lookups, `api()` fetch helper) and `web/types.ts` (shapes of the JSON the Go server sends; keep in sync with the Go structs named there). `web/globals.d.ts` describes the parts of htmx, epub.js and pdf.js the app uses.
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
- **Scripts and styles** (`/static/dist/`, `assets.go`) are held in memory, gzipped once at startup, and served with `Cache-Control: public, max-age=31536000, immutable`. Their names change with their content, so returning visitors make no requests for them until a new build. A restart picks up a new build.
- **Book pages** use an `ETag` built from the OPF modification time, the groups version and whether the viewer may edit, plus `max-age=30`.

## Writes

- **Guards.**
  - Every endpoint that changes the library is wrapped in `requireWrites` (`main.go`), which requires `-allow-writes` and a signed-in admin.
  - The whole server is wrapped in `csrfProtect`, Go's `http.CrossOriginProtection`.
- **Read-only mode** writes only CalibreFast's own caches (`./pages` and `-cachedir`).

## Accounts and per-user data

- `appdb.go`: CalibreFast's own SQLite database in `-datadir`, with versioned migrations (users, identities, sessions, favorites, reading_status, reading_progress, bookmarks, shelves, shelf_books).
- `auth.go`: passwords, sessions and the `withAuth` middleware that puts the user in the request context. The guards are `requireUser`, `requireAdminUser`, `requireReader` (book contents unless `-public-read`) and `requireWrites`.
- `oauth.go`: GitHub/Google sign-in.
- `userdata.go`: favorites, reading status and the `/me/...` lists. `progress.go`: reader position and bookmarks. `shelves.go`: bookshelves.
- The app database stores book UUIDs; book details for lists come from `metadata.db`.
