# Syncing reading positions with KOReader

[KOReader](https://koreader.rocks) runs on Kobo, Kindle, PocketBook and Android devices. Its **Progress sync** plugin can use CalibreFast as its sync server:
- Your place in a book follows you between your e-readers and CalibreFast's web readers.
- Whichever position was saved last wins.

CalibreFast speaks KOReader's own sync protocol, so nothing needs installing on the device, and no outside service is involved.

## Set it up

1. **In CalibreFast:** open **Account → Device sync** and click **Create sync key**. The page shows:
   - the **server address**, e.g. `https://books.example.com/kosync`
   - your **username**
   - the **sync key**, 16 letters. It is shown only once, so note it down or keep the page open.
2. **In KOReader:** open any book (the menu only appears in a book), then go to **Tools** (the wrench) → **Progress sync**.
   1. **Custom sync server:** enter the server address, including `/kosync` at the end.
   2. **Register / Login:** enter your CalibreFast username, and the sync key as the password. Press **Login**, not Register.
   3. Optional: turn on **Automatically keep documents in sync**. Under **Sync behavior**, choose whether KOReader jumps to a newer position silently or asks first.

Repeat step 2 on each device, using the same key. You can't create accounts from KOReader: Register answers with a message pointing you to Login.

**The sync key is not your password.** It only lets a device read and write your reading positions; it can't sign in to CalibreFast. **Replace** it to stop all devices using the old one, or **Revoke** it to turn device sync off. Positions already synced are kept either way.

## Which books are matched

KOReader identifies a book by a fingerprint of the file. CalibreFast works out the same fingerprint for its own files, so it knows which library book a device is reading. The **Document matching method** setting (in Progress sync) picks the fingerprint:

- **Binary** (the default) fingerprints the file's contents. Files **downloaded from CalibreFast** match exactly.
  - Files that **Calibre sent to the device** usually don't match. Calibre rewrites a book's metadata and cover as it sends it, and the Kobo driver may convert it to KePub.
- **Filename** fingerprints the file's name. Calibre normally keeps the library's file name (`Title - Author.epub`) when it sends a book, so **use Filename for books sent by Calibre**.
  - Renamed copies don't match. Nor do two library books with the same file name.

All your devices must use the same method; KOReader can't tell them apart.

Books that match nothing in the library still sync between your devices: CalibreFast stores every position it is sent.

## What syncs

- **EPUB and PDF** positions are shared with the web readers.
  - **PDF:** the page number.
  - **EPUB:** KOReader and the web reader describe positions differently, so CalibreFast translates them. You land on the same chapter, and usually the same paragraph; the exact word within it isn't kept.
  - A device reading back its own position gets exactly what it saved.
- **Other formats** (MOBI, AZW3, DjVu, …) sync between devices only.
- **Highlights and notes don't sync with KOReader.** Its Progress sync protocol carries one thing, the reading position: the plugin's client (`KOSyncClient.lua`) has only register, authorize, `update_progress` and `get_progress`, and the reference server has no other routes. KOReader keeps highlights in a file next to each book on the device. Highlights made in CalibreFast's web readers sync between browsers.
- **Reading status:** reading a library book on a device marks it as *reading*, and 97% or more as *finished*, as the web readers do.

When the web reader holds the newest position, KOReader shows it as coming from the device **CalibreFast**.

## HTTPS

KOReader sends your username and an MD5 of the sync key with every request.
- On your home network, plain HTTP is fine.
- **Anywhere else, use HTTPS:** serve CalibreFast through a reverse proxy with a certificate, set `-public-url https://…`, and enter the `https://` address in KOReader.

Over plain HTTP, anyone who can watch the traffic can copy the key and change your reading positions. They still can't sign in or reach the library. If that may have happened, replace the key.

Behind a reverse proxy with its own login, let `/kosync/` through: KOReader can only send its own credentials.

## Troubleshooting

- **"Wrong username or sync key":** use your CalibreFast username, and the current sync key as the password (not your account password). The key is 16 lower-case letters.
- **"This is CalibreFast: set the custom sync server to …":** the address is missing `/kosync` at the end.
- **"Too many failed sign-ins":** after 10 different wrong keys from one address in 15 minutes, that address is blocked for the rest of the window. A device retrying the same wrong key counts only once.
- **"No progress found for this document":** nothing has been saved for this file yet. Or the book matched nothing in the library, and no other device has opened it.
  - Check the Document matching method on each device (see above).
  - Open the book once in CalibreFast's web reader, or download it from CalibreFast. Fingerprints are worked out when a book is opened or downloaded, not for the whole library up front.
- **The newer position isn't offered:** KOReader compares the server's clock with the device's own. Check the device's date and time.

## For developers

The protocol is that of KOReader's `kosync.koplugin` and the [reference sync server](https://github.com/koreader/koreader-sync-server), also described by [kosync-conformance](https://github.com/pid1/kosync-conformance). CalibreFast serves it under `/kosync`:

| | |
|---|---|
| `GET /kosync/healthcheck` | `{"state":"OK"}` |
| `POST /kosync/users/create` | always refused: 402, code 2005 |
| `GET /kosync/users/auth` | 200 `{"authorized":"OK"}`, or 401, code 2001 |
| `PUT /kosync/syncs/progress` | `{document, progress, percentage, device, device_id}` → `{document, timestamp}` |
| `GET /kosync/syncs/progress/{document}` | the latest position, or `{}` |

- Authenticated requests carry `x-auth-user` (the username) and `x-auth-key` (the MD5 of the sync key).
- The `Accept: application/vnd.koreader.v1+json` header is accepted but not required.
- The implementation is in `kosync.go` (API and keys), `kosync_digest.go` (fingerprints) and `kosync_position.go` (EPUB position translation).

To check the fingerprint against KOReader's own test files, point `KOREADER_TEST_DATA` at a checkout of [koreader/test-data](https://github.com/koreader/test-data) and run `go test -run Fixture`.
