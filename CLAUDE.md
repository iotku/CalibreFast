# CalibreFast: notes for AI assistants

Go server (`*.go`, `html/template` in `templates/`) and TypeScript in `web/`, bundled into the committed `static/dist/` by `go generate`. Read `docs/ARCHITECTURE.md` before larger changes and `docs/SECURITY.md` before touching anything that handles user or book data.

- Run `go generate ./...` after editing `web/`. `TestTypeScriptUpToDate` fails when `static/dist/` is stale. Never edit `static/dist/` by hand.
- Tests: `go test -tags sqlite_fts5 ./...`.

## CSS

- `web/style.css` only `@import`s `web/css/` (tokens, base, components, layout, pages); add a rule to the file for its feature, or a new file in the right folder plus an `@import`.
- Never write a color outside `web/css/tokens.css`: use a semantic token (`var(--text-muted)`, `var(--accent-soft)`, `var(--danger-text)`, …), or add one there. `TestNoRawColorsOutsideTokens` fails otherwise. This is what lets a light theme be one block of values.
- Reuse what's there before adding a variant (`web/css/components/`): `.dialog` (+ `.dialog--danger`, `.dialog-actions`) for any `<dialog>`; `.btn-standard` + `.btn-primary` / `.btn-link` for buttons; `.popover` for anything floating (menus, suggestions, previews), `.card` for a panel on the page, `.pill` for a rounded chip, `.info-tip` for ⓘ help; the chip editor in `chip-editor.css`. A feature's own class can join a shared surface through the selector list at the top of that file instead of carrying a second class. No `<style>` blocks in templates.
- Monospace (`--font-mono`) only for code-like text: identifiers, raw HTML, file names. No web fonts.
- New breakpoints: 480, 640, 768 (the book popup goes full screen), 900 or 1200px; one `@media` block per query per file, at its end.

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
- Trusted Types are **enforced** on every page (`csp.go`): a plain string at an HTML or script sink throws, and the server logs `csp refused: …`. Policies live only in `web/trusted.ts`, and their names must be listed in `trustedTypesPolicyNames` (`TestTrustedTypesPolicyNames`). Don't widen a policy (above all the reader's `default` one) or add a pass-through policy to make an error go away: build the DOM instead. `-trusted-types-report-only` is an emergency switch, not a fix.

## Author names with commas ("|" in metadata.db)

This keeps coming back. Calibre stores a comma in an author's name as `|`: "Ashton, Hugh" is `Ashton| Hugh` in `authors.name`. Calibre itself turns `|` back into `,` whenever it reads a name, and so must we. Everything above the database (the catalog, book popup, links like `/author/Ashton, Hugh`, groups, favorites) uses the comma form.

- **Reading** `authors.name`: never select `a.name` / `t.name` raw. Use `authorNameSQL` (alias `a`) or `authorNameSQLt` (alias `t`, the category pages' `NameSQL`), both in `authors.go`, or `bookAuthorsSQL` for "A & B".
- **Matching** a name from a URL, form or JSON: compare it against that same expression (`WHERE ` + authorNameSQL + ` = ?`), not `a.name = ?`, or the book popup's links lead to empty pages.
- **Writing**: `upsertAuthor` / `authorDBName` store the `|` form. Don't insert into `authors` any other way.
- `authors.sort` and `books.author_sort` keep a real comma; only `authors.name` uses `|`.
- `TestAuthorNamesWithCommas` (authors_test.go) covers the grid, bulk editor, Authors page, `/author/{name}` and Hardcover's book lookup. A new place that reads or matches author names gets a check there. `TestNoRawAuthorNameSQL` fails on a SQL literal that names `authors` and uses `a.name`/`t.name` outside `replace(`; it can't see names built with `fmt.Sprintf`, so those need the behaviour test.

## Server side

- Pages use `html/template`, which escapes by context. **Don't** convert data to `template.HTML`, `template.HTMLAttr`, `template.URL`, `template.JS` or `template.CSS` unless it has just been through a sanitizer (as with `descriptionPolicy`). Don't build HTML with `fmt.Sprintf` or `+` and write it out raw.
- SQL: always `?` placeholders, never string concatenation of values. Identifiers (column names, sort orders) come from fixed allow-lists only.
- File paths: a library path built from metadata must stay inside the library. Use the existing helpers and reject `..`, absolute paths and NUL bytes. Never serve a book's inner files (from inside an EPUB) from our origin as a page.
- Every state-changing handler rejects GET (CSRF protection relies on it) and checks permissions server-side (`requireUser`, `requireEditor`, `requireWrites`, `requireAdminUser`). Routes and their guards are all in `routes.go`; add new ones there. Hiding a button is not access control.
- Responses that echo data as JSON use `encoding/json`, never hand-built JSON strings.
- Don't log secrets: passwords, session tokens, sync keys, one-time links or OAuth codes.

## EPUB reader

Chapters render in epub.js's iframe, sandboxed **without** `allow-scripts`. Never add `allow-scripts`, never serve chapter content from our origin, and keep the book's images and styles as `blob:` URLs, as they are now. See "Book content is untrusted" in `docs/SECURITY.md`.
