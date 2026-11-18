# CalibreFast: notes for AI assistants

Go server (`*.go`, `html/template` in `templates/`) and TypeScript in `web/`, bundled into the committed `static/dist/` by `go generate`. Read `docs/ARCHITECTURE.md` before larger changes and `docs/SECURITY.md` before touching anything that handles user or book data.

- Run `go generate ./...` after editing `web/`. `TestTypeScriptUpToDate` fails when `static/dist/` is stale. Never edit `static/dist/` by hand.
- Tests: `go test -tags sqlite_fts5 ./...`.

## Treat this data as hostile

Book files (EPUB/PDF contents and their OPF metadata), every metadata field (title, author, series, tag, publisher, description, identifiers), shelf names, usernames, annotations and anything from online lookups are all **attacker-controlled**. Anyone who can upload a book, or get one into the library, controls them. The rules below are about keeping that data out of places where the browser parses it as code.

## Building HTML in the browser (web/*.ts)

This is where past bugs came from (code scanning alert 23: a tag name with `"` broke out of an `aria-label`).

- **Prefer DOM APIs over HTML strings.** Use `el()` from `web/dom.ts`, `textContent`, `setAttribute`, `append` or `replaceChildren`. Text set this way is never parsed, so there's nothing to escape and nothing to forget. Icons: `icon()` / `lineIcon()` in `web/dom.ts` (built with `createElementNS`, not parsed). URLs from data: `safeUrl()`.
- `TestNoRawHTMLSinks` (html_sinks_test.go) fails on `innerHTML =`, `insertAdjacentHTML`, `parseFromString` and the like in `web/*.ts` or `templates/*.html`. The only exception is `web/trusted.ts`, whose named Trusted Types policies cover server-rendered fragments (`serverHtml`), inert parsing of book content (`parseInert`) and vendored script URLs. Don't add to it lightly.
- **If you must build an HTML string**, every interpolated value goes through `escapeHtml()` from `web/utils.ts`. That includes numbers you "know" are numbers if they came from JSON. Always quote attribute values with `"`; never leave an attribute unquoted.
- **Escaping isn't enough for URLs.** `escapeHtml` doesn't stop `javascript:` or `data:` URLs. A URL built from data goes in `href` or `src` only if it's a path we built (`/book/get/${encodeURIComponent(uuid)}`) or has been checked to start with `/` (not `//`) or `https:`. Use `encodeURIComponent` for each path segment or query value.
- **Never** put data into `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `iframe.srcdoc`, `DOMParser.parseFromString`, `setAttribute("on…")`, `eval`, `new Function`, `setTimeout(string)`, or `el(tag, { innerHTML })`. Fixed markup we wrote ourselves (icon SVG constants) is fine.
- **Rich HTML from a book** (descriptions) is sanitized: by bluemonday on the server (`descriptionPolicy`, book.go) and by DOMPurify in the editor (`web/description.ts`). Never show it any other way, and never loosen those policies to allow `style`, `on*`, `<script>`, `<iframe>`, `<form>` or `javascript:` URLs.
- Elements with `hx-*` attributes added from script need `htmx.process(container)` after insertion.
- Trusted Types run in report-only mode on every page (`csp.go`). A new HTML-string sink shows up in the server log as `csp report-only: require-trusted-types-for …`. When adding front-end code, check the log stays free of new lines. The goal is to switch to enforcing, so don't add sinks that would make that harder.

## Server side

- Pages use `html/template`, which escapes by context. **Don't** convert data to `template.HTML`, `template.HTMLAttr`, `template.URL`, `template.JS` or `template.CSS` unless it has just been through a sanitizer (as with `descriptionPolicy`). Don't build HTML with `fmt.Sprintf` or `+` and write it out raw.
- SQL: always `?` placeholders, never string concatenation of values. Identifiers (column names, sort orders) come from fixed allow-lists only.
- File paths: a library path built from metadata must stay inside the library. Use the existing helpers and reject `..`, absolute paths and NUL bytes. Never serve a book's inner files (from inside an EPUB) from our origin as a page.
- Every state-changing handler rejects GET (CSRF protection relies on it) and checks permissions server-side (`requireUser`, `requireEditor`, `requireWrites`, `requireAdminUser`). Hiding a button is not access control.
- Responses that echo data as JSON use `encoding/json`, never hand-built JSON strings.
- Don't log secrets: passwords, session tokens, sync keys, one-time links or OAuth codes.

## EPUB reader

Chapters render in epub.js's iframe, sandboxed **without** `allow-scripts`. Never add `allow-scripts`, never serve chapter content from our origin, and keep the book's images and styles as `blob:` URLs, as they are now. See "Book content is untrusted" in `docs/SECURITY.md`.
