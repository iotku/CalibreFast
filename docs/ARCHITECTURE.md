# How CalibreFast works

See also: [FEATURES.md](FEATURES.md) for behaviour, [SECURITY.md](SECURITY.md) for the access model, and [notes/Performance.md](notes/Performance.md) for performance notes.

## Data sources

- **`metadata.db`** (Calibre's SQLite database) is the index for listing, searching and filtering.
- **`metadata.opf`** in each book folder is what the book page and the metadata editor read. Edits write both.
- **`calibrefast-groups.json`** in the library root holds user-defined groups. It's the only file CalibreFast adds to a library.

## Main library pages

On startup CalibreFast queries `metadata.db` and writes paginated JSON (`pages/page1.json`, `page2.json`, …, 120 books each). The frontend fetches these directly as static files, so browsing never hits the database. `pages/page1.json` is regenerated after uploads and edits.

## Filtered views, overview pages and search

- These query SQLite directly.
- **Pre-computed at startup and refreshed after uploads, edits and group changes:**
  - the overview lists (with groups collapsed and distinct book counts)
  - each filtered view's page count, cached in memory
- **Categories.** Authors, publishers, tags and languages are described in one table (`categories` in `aggregate.go`), so all four share the same pages and endpoints. Languages differ only in their column names (`lang_code`).

## Frontend

- **TypeScript, compiled by Go.** Browser code is written in TypeScript in `web/` and compiled one-to-one into `static/` (same file names, so templates and URLs don't change).
  - The compiler is `tsgo`, the Go-native TypeScript compiler. There's no Node, npm or `package.json`.
    - It's pinned as a Go tool in its own module, `tools/go.mod`, so the server itself still builds with Go 1.25 and none of the compiler's dependencies.
    - Working on the TypeScript needs Go 1.26+, which `tsgo` requires.
  - Run `go generate` (or `go tool -modfile=tools/go.mod tsgo -p tsconfig.json`) after editing a `.ts` file. Type errors stop the build, and no JS is written.
  - To update `tsgo`: `cd tools && go get -tool github.com/microsoft/typescript-go/cmd/tsgo@latest`, then `go generate` and run the tests.
  - The compiled `static/*.js` is committed, so `go run .` works without a build step. **Don't edit it by hand.** `TestTypeScriptUpToDate` type-checks `web/` and fails if the committed JS doesn't match the sources.
  - Settings are in `tsconfig.json`: strict mode, no unused locals/parameters, every file is a module.
  - Shared code: `web/dom.ts` (typed element lookups, `api()` fetch helper) and `web/types.ts` (shapes of the JSON the Go server sends; keep in sync with the Go structs named there). `web/globals.d.ts` describes the parts of htmx, epub.js and pdf.js the app uses.
- **Top bar.** Every page except the readers uses the shared bar in `templates/topbar.html`: `{{ template "topbar-start" (topbar …) }}`, the page's own tools (pager, Group button), then `{{ template "topbar-end" … }}`. The `topbar` template function (main.go) takes key/value pairs: `Active` nav item, `Title`, `Query`, and so on.
  - The signed-in part (★ Favorites, Reading, user menu) is filled by `web/nav.ts` from `/api/me`, so pages stay identical for everyone and cacheable. `nav.ts` also opens and closes the ☰ menu, user menu and mobile search.
  - Wide screens show everything inline. At 1000px or less the browse links move into ☰; at 640px or less search becomes an icon and Favorites/Reading move into the user menu.
  - A book page opened directly (no `HX-Request` header) gets the bar; the modal's copy doesn't (`Vary: HX-Request`).
- **Scroll engine.** `web/app-htmx.ts` drives the main library, filtered views and search.
- **Data source.** Each page's HTML sets attributes on `#books`:
  - `data-page-url`: a URL template with `{n}` for the page number
  - `data-total-pages`
  - `data-empty-text`

  The same script therefore works with the static JSON files and with the API endpoints.
- **Book popup** (`web/modal.ts`) loads `/book/get/<uuid>` through htmx.

## Caching

- **Cover thumbnails** are generated on first request and stored in `-cachedir`.
- **Cover loading in the browser** (`pumpCovers` in `web/app-htmx.ts`): covers on screen first, a few at a time (`maxCovers`: 4 over HTTP/1.1, which leaves room for page JSON under the browser's 6 connections per host; 10 over HTTP/2 or 3; 2–4 when Chromium estimates a 2G/3G link); off-screen covers only start once the viewport is done, and in-flight ones that scroll away are cancelled (by removing `src`) and re-queued. While the book modal loads, grid covers are cancelled and wait until every image in the modal has arrived (`cf:modal-busy` from `web/modal.ts`).
- **Static files** (`/static/`) are served with `Cache-Control: max-age=120`, so edits reach browsers within about 2 minutes without constant revalidation.
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
