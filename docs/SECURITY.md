# Security

CalibreFast has accounts, but it's a personal project: the authentication code is new and hasn't been independently reviewed. **Don't expose it to the public internet** without an extra layer in front (a VPN such as WireGuard/Tailscale, or a reverse proxy with its own login), and keep regular backups of your library.

## Who can do what

| | Browse the catalog (covers, metadata, search) | Read and download books | Favorites, progress, bookmarks, own shelves | Change the library (metadata, uploads, covers, groups) | Manage users, global shelves, stop the server |
|---|---|---|---|---|---|
| **Anonymous visitor** | Only with `-public` | Only with `-public-read` | No | **Never** | No |
| **Signed-in user** | Yes | Yes | Yes (their own; progress also from KOReader with a sync key) | **Never** | No |
| **Editor** | Yes | Yes | Yes (their own) | Books and placeholders only, with `-allow-writes`: add, edit, merge, remove. Every change is logged and an admin can undo it | No |
| **Admin** | Yes | Yes | Yes (their own) | Only with `-allow-writes` (removed books go to the Recycling Bin, which only the server can empty) | Yes |

- Library changes always need **both** an editor's or admin's session **and** a server started with `-allow-writes`. Without the flag, nobody (not even admins) can modify `metadata.db` or the library folder: the server opens `metadata.db` read-only (SQLite's `mode=ro`), so even a bug that tried to write would be refused, and it works on a library that's read-only on disk. Trying CalibreFast on your library without `-allow-writes` can't change it. Anyone who may not use a route gets the same 404 as a path that doesn't exist (readers on the editing routes, editors and readers on the admin pages and APIs), so they don't show up to someone probing the server. Unknown paths are 404s too rather than the library page.
- **Editors** can upload books, edit metadata and covers, merge books, add and remove files, remove books, review the import queue, and add, edit and remove placeholders. They can't manage users, sign-in links, groups, featured entries, home page tags or library repairs, and nothing they do destroys anything:
  - every change is a row in the edit log (`edit_log` in `calibrefast.db`): who, when, and what it replaced (a book's metadata before and after, a copy of a replaced cover under `-datadir/edit-log/covers`, a placeholder as it was);
  - removed books and files go to the Recycling Bin; placeholders are only marked removed; a book discarded from the import queue is moved to `imports-discarded` beside the queue's folder; none of these can be emptied from the web;
  - an admin sees every change at **Activity** (`/admin/activity`) and can undo one, or all of one user's since a moment. An undo is skipped where someone else has changed the same thing since, and is itself logged (and undoable).
  - So a stolen editor session can be cleaned up: disable the account (which signs it out), then undo its changes since it was taken.
- Every route and the guard that enforces this (`requireUser`, `requireReader`, `requireEditor`, `requireWrites`, `requireAdminUser`) is listed in one file, `routes.go`, so who may call what can be reviewed in one place.
- Personal data (favorites, reading status, progress, bookmarks, highlights and notes, personal shelves, reading stats) is visible only to its owner. Reading stats (pages and time read per day, when books were started and finished; `stats.go`) can be turned off and deleted by their owner in Preferences. Global shelves are visible to everyone who can browse.

### Modes

- **Default (private):** nothing is visible without signing in, apart from the sign-in page. Anonymous visitors can't see the catalog, covers or book IDs. The sign-in page is only the form: no site name, links, scripts or nav, and its own small stylesheet (`web/auth.css`, shared with sign-in/invitation links) rather than the app's, whose rules would describe every feature. That stylesheet and the app's icons (`/static/icons/`, a generic book) are the only files under `/static/` served without a session. The app's scripts and styles, the service worker (`/sw.js`, whose asset list names them all), its `/offline` page and the web app manifest (which names the library) need one.
- **`-public`:** anyone who can reach the server can browse the catalog (and global shelves). Reading and downloading still need an account.
- **`-public-read`:** public read-only mode. Anyone can also read and download every book. Implies `-public`.
- **Search engines** are kept out by default: `/robots.txt` disallows everything without naming any paths, every response carries `X-Robots-Tag: noindex`, and every page has a matching meta tag. Only `-public-read` together with `-robots-allowed` lets them index the library. The sign-in page and sign-in links are never indexed.

## Accounts

- **Passwords** are hashed with bcrypt (cost 12) and must be 10–72 characters long. A failed sign-in takes about the same time whether or not the username exists, so usernames can't be discovered from response timing.
- **Sessions** last 30 days. The cookie is random (256-bit), `HttpOnly` and `SameSite=Lax`, and `Secure` when served over HTTPS (directly, or when `-public-url` starts with `https://`). Only a SHA-256 hash of the cookie value is stored. Signing out deletes the session.
  - Changing your password signs out your other devices.
  - Disabling a user, or an admin resetting their password, signs them out everywhere.
- **Rate limiting:** sign-in and registration allow 10 attempts per 15 minutes per IP address and per username.
- **Registration** is closed by default. Admins create users, or users are created with `-create-user`.
  - `-allow-registrations` lets anyone who can reach the server create an account from the sign-in page, or by signing in with GitHub/Google. New accounts are never admins.
  - **Bots will try to create accounts.** There's no CAPTCHA or email verification, only rate limiting. Use it only on a trusted network, such as a Tailscale network shared with people you trust, and turn it off once everyone has an account.
- **The last admin** can't be demoted, disabled or deleted.
- **Stopping the server** from the web (`/admin/server`) is for admins only. It's a clean stop, and under systemd also a restart that pulls and rebuilds the latest commit ([DEPLOY.md](DEPLOY.md)), so it can't change the library or the server's options. Like any admin action it needs a signed-in admin's session and a same-origin request.

### Device sync keys (KOReader)

KOReader devices sync reading positions through `/kosync` ([KOREADER.md](KOREADER.md)). They never use the account password:
- **The key:** each user can create one sync key on their account page.
  - It's 16 random lower-case letters, about 73 bits, easy to type on an e-reader.
  - It is shown once. KOReader sends its MD5 with every request, and only a SHA-256 of that MD5 is stored.
  - It is compared in constant time.
- **What it allows:** reading and writing that user's reading positions, and the reading status they update. It can't sign in, read books or reach the catalog.
- **Replacing or revoking** it takes effect at once. Disabled accounts can't sync.
- **Rate limiting:** an address that sends 10 different wrong username/key pairs within 15 minutes is refused (429) until the window passes. A device repeating one stale key counts once, so it can't lock out other devices behind the same address.
- **Registering** from KOReader is refused: accounts only come from CalibreFast.
- **Plain HTTP:** the key's MD5 is sent in a header with every request. **Use HTTPS for anything beyond your own network.** Anyone who copies it can change your reading positions until you replace the key, but can do nothing else.

### One-time links

Admins can create links from **Manage users** to send to someone directly, e.g. in a Discord DM:

- **Sign-in link:** signs in as an existing user. It works like their password until it's used or expires, so only send it privately.
- **Invite link:** lets someone create an account (password, or GitHub/Google) even with registration closed, optionally as an admin.

Safeguards:
- **Tokens:** random 256-bit tokens; only a hash is stored, and the link is shown once.
- **Single use,** even if two people click at the same moment.
- **Expiry:** after 1 hour, 1 day or 7 days. Unused links can be revoked from the same page.
- **Previews don't use them up.** Opening a link (as chat apps do to build previews) only shows a confirmation page; the link is used when the person clicks the button.
- **No leaks or indexing:** the pages aren't cached or indexed and don't send the link to other sites as a referrer.
- **Logging:** creating and using links is logged.

### GitHub and Google sign-in

Setup steps are in [OAUTH.md](OAUTH.md). In short: set `CF_GITHUB_CLIENT_ID` / `CF_GITHUB_CLIENT_SECRET` and/or `CF_GOOGLE_CLIENT_ID` / `CF_GOOGLE_CLIENT_SECRET` as environment variables, so they don't show up in `ps`. Also set `-public-url`. Register `<public-url>/auth/github/callback` and `<public-url>/auth/google/callback` as the callback URLs.

- **Standard flow:** OAuth 2.0 authorization code flow with PKCE, and a single-use `state` value tied to your browser by a cookie. This prevents login CSRF and replayed callbacks.
- **Account matching:** by the provider's permanent user ID, **never by email address**. Someone who registers your email with GitHub or Google can't take over your account.
- **New accounts:** without `-allow-registrations`, a GitHub/Google sign-in only works once you've linked it. Sign in with your password, then use **Account → Linked sign-ins**. With it, an unknown GitHub/Google account gets a new, non-admin user.
- **Unlinking** can't remove your only way to sign in.

## Cross-site requests

Browsers let any website send requests to a server on your network (cross-site request forgery). CalibreFast refuses state-changing requests (`POST`, `PUT`, `DELETE`, `PATCH`) that a browser marks as coming from another site, using Go's `http.CrossOriginProtection`, and logs them. Session cookies are also `SameSite=Lax`.

## Book content is untrusted

EPUBs can contain scripts and arbitrary HTML. The server only ever sends the `.epub` itself, as a download (whole, or byte ranges of it). The reader unpacks it in the browser, and epub.js shows each chapter in a sandboxed iframe with scripts disabled; the book's images and styles reach the chapter as `blob:` URLs, never from our origin. Safari runs none of our listeners in a chapter sandboxed like that, and left alone it would follow a tapped link by navigating the chapter frame itself to a page of our origin; so in Safari a surface of our own covers the chapter and takes its input (`web/chapter-input.ts`), and the browser never activates the chapter's links. Kept books (for offline reading) stay in that browser's storage, belong to the user who read them, and are removed on sign-out. Book descriptions are sanitized on the server (bluemonday) and in the metadata editor (DOMPurify).

Kindle books (AZW3, AZW, MOBI) are read the same way, as an `.epub` the server converts them to (`/book/as-epub/…`, `convert_read.go`, `pkg/mobi`). The converter treats the file as hostile: every offset and length is checked and decompression is capped. It writes the markup out again as well-formed XHTML without scripts, event handlers, frames, objects, forms or `javascript:` URLs, though the reader's sandbox doesn't depend on that. The converted file goes in the cache folder, never in the library.

## Browser safeguards (Trusted Types)

Every response carries `Content-Security-Policy: require-trusted-types-for 'script'; trusted-types …` (`csp.go`). The browser then refuses a plain string at every sink that turns text into markup or loads code (`innerHTML`, `insertAdjacentHTML`, `iframe.srcdoc`, `DOMParser`, script and worker URLs, service-worker registration, …). That blocks client-side XSS from book data even when an escape is missed. Only a few named policies in `web/trusted.ts` can make allowed values, each for one narrow job: our server's own HTML fragments (and htmx's swaps of them, with scripts in swapped markup dropped and `hx-on` disabled), inert parsing of book files, our own script URLs, and, on the reader page only, epub.js parsing the book and setting chapters as its sandboxed iframe's `srcdoc`. The header lists those names, so no other policy can be made. Violations are refused and sent to `/api/csp-report` (signed-in users only, at most 16 KB a report), which logs each distinct one once as `csp refused: …`, up to 500 per run. If a browser feature breaks because of it, start the server with **`-trusted-types-report-only`** to report without refusing until it's fixed. Browsers without Trusted Types ignore the header. It doesn't cover `eval` (that would need a `script-src` policy).

## Online lookups

The metadata editor's lookup bar sends a book's ISBN (**Search ISBN**) or its title and first author (**Search title & author**) to Open Library. Google Books is asked only when an admin clicks **Try Google Books**, which is offered only when the server has a Google Books API key; Hardcover likewise only on **Try Hardcover**, with `CF_HARDCOVER_TOKEN`. A Hardcover cover is fetched by the server from the URL Hardcover's answer gave (kept on the server; the browser sends only a book id), only from `https://…hardcover.app`, never as SVG, and without the token. The cover dialog's **Look up cover online** sends the ISBN to Open Library too. That tells those sites a book is in your library.

- **Only on a click** by an admin, on a server with `-allow-writes`, one site at a time. Opening the editor, uploading, or finding ISBNs in a book's files sends nothing.
- **Only the query** is sent, with a generic User-Agent. No account, library name or cookies.
- **Covers are fetched by the server**, from URLs it builds itself, and only when asked for, so your browser doesn't contact those sites.
- **Limits:** each site has a rate and daily limit, and CalibreFast backs off when a site says to (HTTP 429).
- **`-no-lookups`** turns lookups off entirely; **`-no-google-books`** (or leaving `CF_GOOGLE_BOOKS_KEY` unset) turns off Google only.

### Hardcover series

With `CF_HARDCOVER_TOKEN` set, editors can link a series to Hardcover and review its book list (`internal/hardcoverseries`, `/missing/hardcover`). This tells Hardcover what series are in your library.

- **What is sent:** the series' name (and its group's member names), the names of Hardcover series it may be, up to 50 ISBNs of its books in the library, and Hardcover ids. Nothing about other books, users or the library. Requests go only to `api.hardcover.app`, with the generic User-Agent.
- **Only on a click:** one series, or the series of one author that the editor ticks (at most 25). The whole library is never scanned, and nothing runs on page load. Lists are fetched again only when asked; each shows when it was fetched.
- **Limits:** every request goes through one queue that stays inside Hardcover's free plan: 60 a minute, a burst of 10, and 4,500 of the 5,000 a day (UTC), following Hardcover's own `RateLimit` header. Over a limit, requests wait in `hardcover_jobs` and run when it resets. They are not sent anyway or dropped. The day's use survives restarts.
- **The token** is a personal Hardcover API token. It is read from the environment and sent only in the `Authorization` header. It is never logged, put in an error, stored in a database or sent to the browser. Give it the `read:catalog` scope only. (Users' own Hardcover sign-ins, for reading sync, are separate: see below.)
- **Hardcover's data is untrusted.** Titles, names and dates go through `cleanPlaceholderMeta` before becoming placeholders, and are shown through `html/template` or `textContent`. Only links to `https://hardcover.app/` with a checked slug are made. Applying a review sends only ids from the browser. The server rebuilds what it writes from the list it stored. Tagging books goes through the bulk edit queue, in the edit log, like any other edit.
- **`-no-hardcover`** (or `-no-lookups`, or no token) turns it off.

### Hardcover author profiles

With `CF_HARDCOVER_TOKEN` set, the first time a signed-in user opens an author's page, the server looks the author up on Hardcover once (`internal/hardcoverauthors`) and keeps the answer. This tells Hardcover the names of authors in your library. Only the author's name (or their group's member names) is sent, and only for authors with books in the library, through the same queue and limits as series, after editors' requests and never past 80% of the day's limit. The picture is fetched from `https://…hardcover.app` only, without the token, re-encoded as JPEG and served from our origin. The bio is reduced to plain text and escaped by `html/template`. `-no-hardcover` turns it off too.

### Hardcover reading sync

A user can link their own Hardcover account on the account page (`internal/hardcoversync/link.go`), and then share chosen books with it (`internal/hardcoversync/sync.go`). This tells Hardcover, as that user, what they read in your library, so every step is the user's explicit choice:

- **Linking shares nothing.** A book is sent only once the user turns on **Sync to Hardcover** on it (`hardcover_shared`), or chooses **Every book I mark or read here, from now on** on the account page (`share_all`, off by default; earlier books only with **Also send books I was reading or finished before**), except the books they turned it off on (`hardcover_unshared`). For a book not shared nothing is sent, not even its ISBN to look it up, and no note is kept to send later; a change waiting when a book stops being shared is dropped, and the worker checks again before each send.
- **Only what's chosen.** For a shared book its status is sent. How far along (pages) and the dates a read started and finished are sent only if the user ticks them (`share_progress`, `share_dates`, off by default). Favorites, shelves, highlights, notes, bookmarks and other users' data are never sent.
- **The user can see and undo it.** The account page lists the books shared; unticking one stops it at once. Unlinking forgets every choice. What was sent stays on Hardcover (it's the user's data there), and nothing on Hardcover is ever deleted or read back.

- **Signing in:** Hardcover's OAuth device grant. CalibreFast gets a code, the user enters it at `https://hardcover.app/link` and approves CalibreFast there. No Hardcover password or token passes through the browser, and the user never pastes a token. The OAuth client is public (Hardcover's "Mobile, desktop, or CLI" type, with no secret), so anyone who knows a client ID can start sign-ins under its name. No client ID ships with CalibreFast for that reason: each server's admin registers their own app and sets `CF_HARDCOVER_CLIENT_ID`. Hardcover's approval screen then names the server's own app, and misuse of a client ID points at that server's admin, not at the project or other servers. Scopes asked for: `read:me read:library write:library read:catalog read:social write:social` (`hardcover.SyncScopes`). The last two are for the authors a user follows there, and only used when they ask: reading them when they bring them in, following the authors they star here if they turn that on, and unfollowing one when they tick **Also unfollow on Hardcover** as they take its star off.
- **Tokens at rest:** the access token (a week) and refresh token (six months, replaced on every use) are kept in `hardcover_links`, encrypted with AES-256-GCM (`secretbox.go`). The key is `secrets.key` in `-datadir` (`0600`, made the first time), so a copy of `calibrefast.db` alone holds no usable token. Each value is bound to its column and user, so it can't be moved to another row. Keep `secrets.key` out of the database's backups (see below; the server's own backups do); if it's lost, users link again. Tokens are never logged, put in an error, or sent to the browser (`/api/me/hardcover` gives only the state, the Hardcover username and the code being entered).
- **Renewal:** one renewal at a time per user, and the new pair is saved before the new access token is used (Hardcover revokes the whole sign-in if a refresh token is used twice). If Hardcover refuses the sign-in (revoked under Authorized Apps, or unused for six months), the link is marked broken and nothing more is sent until the user links again.
- **What is sent, for a shared book:** its ISBN-13s and Hardcover identifiers (to find the book; with the server's `CF_HARDCOVER_TOKEN` when set, so the lookup isn't tied to the user, else the user's), then its status, and, if chosen, progress as pages of the edition and the read's dates. Requests go only to `api.hardcover.app`, with the generic User-Agent. A status cleared here is not sent.
- **Choosing the book (any user, not only editors):** when Hardcover can't find a shared book, or finds the wrong one, the book page lets the user search Hardcover and pick the right book (`internal/hardcoversync/book.go`). The search (what they type, or a pasted `hardcover.app/books/…` link) goes to Hardcover only for a book they share, with the server's token when set. Only the chosen book's id is taken from the browser; its edition, pages and title come from Hardcover. The choice is kept per user (`hardcover_user_books`), changes only what's sent to their own account, never the library's identifiers or other users' sync, and is deleted when they unlink. Titles and errors are shown with `textContent`; links to Hardcover are built from a checked slug.
- **Limits:** each user's requests use their own token and stay inside Hardcover's free plan for it. Over a limit, that user's changes wait; failures are retried with backoff, up to a day apart.
- **Who can use the tokens:** encryption protects a copy of the database on its own. Whoever has both `calibrefast.db` and `secrets.key` (the server's operator, root on the host, or a backup of the whole data folder) can act on every linked user's Hardcover library, with the scopes above, until the refresh tokens lapse (six months unused) or users unlink. The account page says so before a user links. Back up `secrets.key` apart from the database, or not at all: without it, users only have to link again.
- **Unlinking** always works, even with sync turned off on the server. It revokes the sign-in at Hardcover, then deletes the tokens, anything waiting to be sent and the sharing choices here, whatever Hardcover answers. If Hardcover couldn't be reached, the user is told to remove CalibreFast under Authorized Apps in Hardcover's settings. Deleting a user revokes their sign-in the same way.
- **A broken link keeps no tokens:** when Hardcover refuses a renewal, or the stored sign-in can't be read or saved, the tokens are deleted and only the Hardcover username and the reason stay, until the user links again or unlinks.
- **No redirects:** OAuth requests (which carry codes and refresh tokens in their bodies) never follow a redirect.
- **`-no-hardcover-sync`** (or `-no-hardcover`) turns it off: nothing is sent and nobody can link. Users linked before see that on their account page and can still unlink, which revokes the sign-in at Hardcover (as long as the client id is still set).

## Writes can conflict with Calibre itself

CalibreFast writes to the same `metadata.db` and book folders that the Calibre desktop app uses. Editing in both at once can lose changes or confuse Calibre. **Close Calibre while editing through CalibreFast.**

Changes made to the library outside CalibreFast (in Calibre, `calibre-server` or by hand) show up after CalibreFast restarts: at startup it notices that `metadata.db` changed and reads the library again. Until then, pages show the library as it was.

## Where data is stored

- **`-datadir`** (default `./data`) holds `calibrefast.db`: accounts, sessions, sync key hashes, favorites, progress (including KOReader devices' positions and names), bookmarks, highlights and notes, shelves, and users' encrypted Hardcover sign-ins. It also caches the fingerprints KOReader uses for library files. `secrets.key` beside it (`0600`) is the key those sign-ins are encrypted with: back it up apart from the database, or not at all.
  - The directory is created `0700` and the database file is `0600`, because it holds password hashes and session tokens.
  - **Keep it out of version control** (`/data/` is in `.gitignore`) and back it up separately from the library.
- **`-datadir/imports`** (or `-import-dir`) holds books from the watch folder (`-watch-dir`) until an editor or admin adds or discards them: the book files themselves, moved out of the watch folder. Only editors and admins on a server with `-allow-writes` can list, download, add or discard them. A discarded book's folder is moved to `imports-discarded` beside it (`<import-dir>-discarded`), never deleted; clear it on the server. The server refuses an `-import-dir` that overlaps the library or holds `-datadir`, and a `-watch-dir` that overlaps the queue.
- **`-datadir/edit-log/covers`** holds a copy of every book cover an edit replaced, for undoing it. Clear old ones on the server if it grows.
  - The watcher runs with the server's own file permissions and **moves** files, so point `-watch-dir` at a folder meant for it. The server refuses one that overlaps the library or `-datadir`.
  - Waiting files are untrusted like any other book: they are only parsed for metadata, and shown to the admin reviewing them.
- **Backups** (`-backup-dir`, default `-datadir/backups`, `0700`, files `0600`): copies of `calibrefast.db` (daily) and `metadata.db` (weekly), as private as the originals. Only admins can list, make or download them (`/admin/server`; anyone else gets a 404). `secrets.key` is never among them: its copies stay in `-datadir/secrets.key.d/`, so a backup or a download holds no usable Hardcover sign-in. See [DEPLOY.md](DEPLOY.md#backups-and-restoring).
- **The Recycling Bin** (`-recycle-bin`, default `-datadir/recycle-bin`, created `0700`) holds removed books' folders, a copy of `metadata.db` from before each removal, and readers' bookmarks, highlights, notes and progress for those books (`reader-data.json`, private data like `calibrefast.db`). The web can add to it (removing books) and take books out of it (restoring them), but never delete it: there's no endpoint for that. Emptying it is `-empty-recycle-bin` or deleting folders on the server.
  - So a stolen admin or editor session can remove books but not destroy them; restore them from the page, or roll back by hand ([FEATURES.md](FEATURES.md#removing-books-and-the-recycling-bin)).
  - Each removal copies `metadata.db`. Someone removing and restoring books over and over could fill the disk, so keep an eye on its size, and empty old batches.
  - Put it somewhere admins of the server's host would notice, and include it in backups.
- **`calibrefast-groups.json`** in the library folder holds groups. It's the only file CalibreFast adds to a library.
- **Read-only caches:** `./pages` and `-cachedir` (cover thumbnails). Nothing else is written unless an editor or admin edits with `-allow-writes`.

## Recommendations

- **Don't expose CalibreFast directly to the internet.** Put it behind a VPN or a reverse proxy with its own login, and serve it over HTTPS (set `-public-url https://…`).
- **Leave `-allow-writes` off normally.** Turn it on while you're editing or uploading.
- **Back up** `metadata.db` (ideally the whole library) and your `-datadir` before enabling writes.
- **Prefer closed registration.** Create accounts as an admin and have people link GitHub/Google afterwards. If you do use `-allow-registrations`, only on a trusted network, and only for as long as you need it.
- **Leave the library private** unless you mean to share it. `-public` exposes your whole catalog to anyone who can reach the server.
- **Use an up-to-date browser.** Cross-site protection relies on headers all current browsers send.
