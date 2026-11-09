# Security

CalibreFast has accounts, but it's a personal project: the authentication code is new and hasn't been independently reviewed. **Don't expose it to the public internet** without an extra layer in front (a VPN such as WireGuard/Tailscale, or a reverse proxy with its own login), and keep regular backups of your library.

## Who can do what

| | Browse the catalog (covers, metadata, search) | Read and download books | Favorites, progress, bookmarks, own shelves | Change the library (metadata, uploads, covers, groups) | Manage users, global shelves |
|---|---|---|---|---|---|
| **Anonymous visitor** | Only with `-public` | Only with `-public-read` | No | **Never** | No |
| **Signed-in user** | Yes | Yes | Yes (their own) | **Never** | No |
| **Admin** | Yes | Yes | Yes (their own) | Only with `-allow-writes` | Yes |

- Library changes always need **both** an admin session **and** a server started with `-allow-writes`. Without the flag, nobody (not even admins) can modify `metadata.db` or the library folder. Anonymous visitors get 401 and regular users 403.
- Personal data (favorites, reading status, progress, bookmarks, personal shelves) is visible only to its owner. Global shelves are visible to everyone who can browse.

### Modes

- **Default (private):** nothing is visible without signing in, apart from the sign-in page. Anonymous visitors can't see the catalog, covers or book IDs.
- **`-public`:** anyone who can reach the server can browse the catalog (and global shelves). Reading and downloading still need an account.
- **`-public-read`:** public read-only mode. Anyone can also read and download every book. Implies `-public`.

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

EPUBs can contain scripts and arbitrary HTML. The server only ever sends the whole `.epub` as a download. The reader unpacks it in the browser, and epub.js shows each chapter in a sandboxed iframe with scripts disabled. Book descriptions are sanitized on the server (bluemonday) and in the metadata editor (DOMPurify).

## Writes can conflict with Calibre itself

CalibreFast writes to the same `metadata.db` and book folders that the Calibre desktop app uses. Editing in both at once can lose changes or confuse Calibre. **Close Calibre while editing through CalibreFast.**

## Where data is stored

- **`-datadir`** (default `./data`) holds `calibrefast.db`: accounts, sessions, favorites, progress, bookmarks and shelves.
  - The directory is created `0700` and the database file is `0600`, because it holds password hashes and session tokens.
  - **Keep it out of version control** (`/data/` is in `.gitignore`) and back it up separately from the library.
- **`calibrefast-groups.json`** in the library folder holds groups. It's the only file CalibreFast adds to a library.
- **Read-only caches:** `./pages` and `-cachedir` (cover thumbnails). Nothing else is written unless an admin edits with `-allow-writes`.

## Recommendations

- **Don't expose CalibreFast directly to the internet.** Put it behind a VPN or a reverse proxy with its own login, and serve it over HTTPS (set `-public-url https://…`).
- **Leave `-allow-writes` off normally.** Turn it on while you're editing or uploading.
- **Back up** `metadata.db` (ideally the whole library) and your `-datadir` before enabling writes.
- **Prefer closed registration.** Create accounts as an admin and have people link GitHub/Google afterwards. If you do use `-allow-registrations`, only on a trusted network, and only for as long as you need it.
- **Leave the library private** unless you mean to share it. `-public` exposes your whole catalog to anyone who can reach the server.
- **Use an up-to-date browser.** Cross-site protection relies on headers all current browsers send.
