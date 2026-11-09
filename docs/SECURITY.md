# Security

**CalibreFast has no authentication or access control of any kind.** Please read this before running it anywhere other than your own machine.

## Read-only mode (the default)

Anyone who can reach the server can **browse, search, read and download every book** in your library.

Nothing can be changed:
- every write endpoint (uploads, metadata edits, covers, groups) returns HTTP 403
- the edit and group controls are hidden

## With `-allow-writes`

Anyone who can reach the server can **also change your library**, with no password and no record of who did it. They can:

- **Upload any files.** Uploads are written into your library folder and added to `metadata.db`.
- **Overwrite metadata** on any book (titles, authors, descriptions, tags, identifiers, dates and languages), in both `metadata.db` and `metadata.opf`.
- **Replace or revert covers.**
- **Create, change or delete groups** (stored in your library folder).

### Cross-site requests are refused, but that's not authentication

Browsers will let any website send requests to a server on your network. This is called cross-site request forgery.

CalibreFast uses Go's `http.CrossOriginProtection` to reject state-changing requests (`POST`, `PUT`, `DELETE`, `PATCH`) that a browser marks as coming from another site, and logs them. This protects you from websites open in your own browser.

It does **nothing** against people who can reach the server directly. Scripts and tools like `curl` don't send those browser headers, so they're let through.

### Writes can conflict with Calibre itself

CalibreFast writes to the same `metadata.db` and book folders that the Calibre desktop app uses. Editing in both at once can lose changes or confuse Calibre. **Close Calibre while making changes through CalibreFast.**

## Recommendations

- **Never expose CalibreFast to the public internet,** in either mode. Use it on localhost or a private network you trust (e.g. WireGuard/Tailscale), and preferably behind a reverse proxy with its own authentication.
- **Leave `-allow-writes` off normally.** Restart with it only while you're editing or uploading, then restart without it.
- **Back up your library** before enabling writes: at least `metadata.db`, and ideally the whole folder. Metadata is easy to lose and hard to recreate.
- **Use an up-to-date browser.** Cross-site protection relies on headers all current browsers send (`Sec-Fetch-Site`, `Origin`).

## What read-only mode still writes

CalibreFast's own caches:
- the page JSON in `./pages`
- cover thumbnails in `-cachedir`

It never writes to your library folder or `metadata.db` in read-only mode.
