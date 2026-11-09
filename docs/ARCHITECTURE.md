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

- **Scroll engine.** `static/app-htmx.js` drives the main library, filtered views and search.
- **Data source.** Each page's HTML sets attributes on `#books`:
  - `data-page-url`: a URL template with `{n}` for the page number
  - `data-total-pages`
  - `data-empty-text`

  The same script therefore works with the static JSON files and with the API endpoints.
- **Book popup** (`static/modal.js`) loads `/book/get/<uuid>` through htmx.

## Caching

- **Cover thumbnails** are generated on first request and stored in `-cachedir`.
- **Static files** (`/static/`) are served with `Cache-Control: max-age=120`, so edits reach browsers within about 2 minutes without constant revalidation.
- **Book pages** use an `ETag` built from the OPF modification time, the groups version and the `-allow-writes` setting, plus `max-age=30`.

## Writes

- **Guards.**
  - Every endpoint that changes the library is wrapped in `requireWrites` (`main.go`), which returns 403 unless the server was started with `-allow-writes`.
  - The whole server is wrapped in `csrfProtect`, Go's `http.CrossOriginProtection`.
- **Read-only mode** writes only CalibreFast's own caches (`./pages` and `-cachedir`).
