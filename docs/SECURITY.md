# Security

CalibreFast has accounts, but it's a personal project: the authentication code is new and hasn't been independently reviewed. **Don't expose it to the public internet** without an extra layer in front (a VPN such as WireGuard/Tailscale, or a reverse proxy with its own login), and keep regular backups of your library.

## Who can do what

| | Browse the catalog (covers, metadata, search) | Read and download books | Favorites, progress, bookmarks, own shelves | Change the library (metadata, uploads, covers, groups) | Manage users, global shelves |
|---|---|---|---|---|---|
| **Anonymous visitor** | Only with `-public` | Only with `-public-read` | No | **Never** | No |
| **Signed-in user** | Yes | Yes | Yes (their own; progress also from KOReader with a sync key) | **Never** | No |
| **Admin** | Yes | Yes | Yes (their own) | Only with `-allow-writes` (removed books go to the Recycling Bin, which only the server can empty) | Yes |

- Library changes always need **both** an admin session **and** a server started with `-allow-writes`. Without the flag, nobody (not even admins) can modify `metadata.db` or the library folder: the server opens `metadata.db` read-only (SQLite's `mode=ro`), so even a bug that tried to write would be refused, and it works on a library that's read-only on disk. Trying CalibreFast on your library without `-allow-writes` can't change it. Anyone but an admin gets the same 404 as a path that doesn't exist, on the editing routes and the admin pages and APIs alike, so they don't show up to someone probing the server. Unknown paths are 404s too rather than the library page.
- Personal data (favorites, reading status, progress, bookmarks, highlights and notes, personal shelves) is visible only to its owner. Global shelves are visible to everyone who can browse.

### Modes

- **Default (private):** nothing is visible without signing in, apart from the sign-in page. Anonymous visitors can't see the catalog, covers or book IDs.
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

EPUBs can contain scripts and arbitrary HTML. The server only ever sends the `.epub` itself, as a download (whole, or byte ranges of it). The reader unpacks it in the browser, and epub.js shows each chapter in a sandboxed iframe with scripts disabled; the book's images and styles reach the chapter as `blob:` URLs, never from our origin. Kept books (for offline reading) stay in that browser's storage, belong to the user who read them, and are removed on sign-out. Book descriptions are sanitized on the server (bluemonday) and in the metadata editor (DOMPurify).

## Online lookups

The metadata editor's lookup bar sends a book's ISBN (**Search ISBN**) or its title and first author (**Search title & author**) to Open Library. Google Books is asked only when an admin clicks **Try Google Books**, which is offered only when the server has a Google Books API key. The cover dialog's **Look up cover online** sends the ISBN to Open Library too. That tells those sites a book is in your library.

- **Only on a click** by an admin, on a server with `-allow-writes`, one site at a time. Opening the editor, uploading, or finding ISBNs in a book's files sends nothing.
- **Only the query** is sent, with a generic User-Agent. No account, library name or cookies.
- **Covers are fetched by the server**, from URLs it builds itself, and only when asked for, so your browser doesn't contact those sites.
- **Limits:** each site has a rate and daily limit, and CalibreFast backs off when a site says to (HTTP 429).
- **`-no-lookups`** turns lookups off entirely; **`-no-google-books`** (or leaving `CF_GOOGLE_BOOKS_KEY` unset) turns off Google only.

## Writes can conflict with Calibre itself

CalibreFast writes to the same `metadata.db` and book folders that the Calibre desktop app uses. Editing in both at once can lose changes or confuse Calibre. **Close Calibre while editing through CalibreFast.**

## Where data is stored

- **`-datadir`** (default `./data`) holds `calibrefast.db`: accounts, sessions, sync key hashes, favorites, progress (including KOReader devices' positions and names), bookmarks, highlights and notes, and shelves. It also caches the fingerprints KOReader uses for library files.
  - The directory is created `0700` and the database file is `0600`, because it holds password hashes and session tokens.
  - **Keep it out of version control** (`/data/` is in `.gitignore`) and back it up separately from the library.
- **`-datadir/imports`** holds books from the watch folder (`-watch-dir`) until an admin adds or discards them: the book files themselves, moved out of the watch folder. Only admins on a server with `-allow-writes` can list, download, add or discard them.
  - The watcher runs with the server's own file permissions and **moves** files, so point `-watch-dir` at a folder meant for it. The server refuses one that overlaps the library or `-datadir`.
  - Waiting files are untrusted like any other book: they are only parsed for metadata, and shown to the admin reviewing them.
- **The Recycling Bin** (`-recycle-bin`, default `-datadir/recycle-bin`, created `0700`) holds removed books' folders, a copy of `metadata.db` from before each removal, and readers' bookmarks, highlights, notes and progress for those books (`reader-data.json`, private data like `calibrefast.db`). The web can add to it (removing books) and take books out of it (restoring them), but never delete it: there's no endpoint for that. Emptying it is `-empty-recycle-bin` or deleting folders on the server.
  - So a stolen admin session can remove books but not destroy them; restore them from the page, or roll back by hand ([FEATURES.md](FEATURES.md#removing-books-and-the-recycling-bin)).
  - Each removal copies `metadata.db`. Someone removing and restoring books over and over could fill the disk, so keep an eye on its size, and empty old batches.
  - Put it somewhere admins of the server's host would notice, and include it in backups.
- **`calibrefast-groups.json`** in the library folder holds groups. It's the only file CalibreFast adds to a library.
- **Read-only caches:** `./pages` and `-cachedir` (cover thumbnails). Nothing else is written unless an admin edits with `-allow-writes`.

## Recommendations

- **Don't expose CalibreFast directly to the internet.** Put it behind a VPN or a reverse proxy with its own login, and serve it over HTTPS (set `-public-url https://…`).
- **Leave `-allow-writes` off normally.** Turn it on while you're editing or uploading.
- **Back up** `metadata.db` (ideally the whole library) and your `-datadir` before enabling writes.
- **Prefer closed registration.** Create accounts as an admin and have people link GitHub/Google afterwards. If you do use `-allow-registrations`, only on a trusted network, and only for as long as you need it.
- **Leave the library private** unless you mean to share it. `-public` exposes your whole catalog to anyone who can reach the server.
- **Use an up-to-date browser.** Cross-site protection relies on headers all current browsers send.
