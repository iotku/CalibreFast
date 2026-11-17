# kosync

`import "github.com/iotku/CalibreFast/pkg/kosync"`

Building blocks for a server that speaks KOReader's progress sync protocol
("kosync"): the document-id hashes, sync-key helpers, a key-guessing limiter,
and the protocol's error format. It doesn't store anything or route
requests; CalibreFast's `kosync_server.go` shows one way to put them
together.

References: KOReader's `plugins/kosync.koplugin`,
[koreader-sync-server](https://github.com/koreader/koreader-sync-server)
(the reference server), and
[kosync-conformance](https://github.com/pid1/kosync-conformance).

## The protocol

The user enters a "custom sync server" URL in KOReader (CalibreFast uses
`https://<host>/kosync`, see `Prefix`); paths below are relative to it.
Requests carry `Accept: application/vnd.koreader.v1+json`, and JSON bodies.

| Method and path | Auth | Answer |
| --- | --- | --- |
| `GET /healthcheck` | no | `200 {"state": "OK"}` |
| `POST /users/create` `{"username", "password"}` | no | `201 {"username"}`; `402` + code 2002/2005 when refused |
| `GET /users/auth` | yes | `200 {"authorized": "OK"}` |
| `PUT /syncs/progress` | yes | `200 {"document", "timestamp"}` |
| `GET /syncs/progress/{document}` | yes | `200 {"document", "progress", "percentage", "device", "device_id", "timestamp"}`, or `200 {}` for an unknown document |

**Authentication.** Every authenticated request has `x-auth-user` (the user
name) and `x-auth-key`, which is the lower-case hex MD5 of the password the
user typed (`MD5Hex`). A failure is `401` with code 2001. Only a 401 makes
KOReader drop a failed push; on other errors it keeps the position and
retries.

CalibreFast doesn't use account passwords here: each user generates a
**sync key** (`RandomKey`: 16 letters, no `l` or `o`, easy to type on an
e-reader) and enters it as the password. Store only
`KeyHash(MD5Hex(key))` (a SHA-256 of what the device sends), compare in
constant time, and compare against `DummyKeyHash` for unknown users so they
take as long.

**A position** (`PUT /syncs/progress`):

```json
{"document": "0b2c…", "progress": "/body/DocFragment[12]/body/p[3]/text().0",
 "percentage": 0.4213, "device": "Kobo Libra", "device_id": "A1B2…"}
```

- `document`: validate with `DocumentRe` (KOReader's are 32 hex digits).
- `progress`: a string, returned exactly as stored. For EPUBs it's a
  crengine XPointer (see `pkg/koreader` to convert to and from EPUB CFI);
  for PDFs and other paged formats, a page number. `JSONString` also
  accepts a number.
- `percentage`: 0–1; `JSONFloat` accepts a number or a numeric string, as
  the reference server does.
- `device`, `device_id`: free text (bound them, e.g. `MaxDeviceNameLen`).
- The server sets `timestamp` (Unix seconds); last writer wins.

**Errors** are `{"code": …, "message": …}` (`WriteError`); KOReader shows the
message.

| Code | Constant | Used for |
| --- | --- | --- |
| 2000 | `ErrUnknown` | server error |
| 2001 | `ErrUnauthorized` | bad credentials (401) |
| 2003 | `ErrInvalidRequest` | bad body or fields |
| 2004 | `ErrDocument` | missing or bad document id |
| 2005 | `ErrNoRegistration` | registration refused (402) |

## Document ids

KOReader names a book one of two ways (its "document matching method"):

- **Binary** (the default): `PartialMD5` — the MD5 of 1 KiB samples at
  offsets 0, 1 KiB, 4 KiB, 16 KiB, … 1 GiB, stopping at the end of the file.
  KOReader computes the offsets as `bit.lshift(1024, 2*i)` for `i = -1..10`;
  LuaJIT masks the shift to five bits, so the first offset is 0, not 256.
  Any change to the file (Calibre rewriting it on "send to device") changes
  this id.
- **Filename**: `FileNameDigest(name)` — the MD5 of the file's name.

## Limiting guesses

`GuessLimiter` blocks an address after `max` *different* wrong
(user, key) pairs within a window. A device that keeps pushing with a stale
key (KOReader pushes about every 25 seconds while reading) counts once, so it
can't lock out other devices behind the same address. Answer a blocked
address with `429` (not 401, so the device keeps its position).

```go
var guesses = kosync.NewGuessLimiter(10, 15*time.Minute)

if guesses.Blocked(addr) {
	kosync.WriteError(w, http.StatusTooManyRequests, kosync.ErrUnauthorized, "Too many failed sign-ins.")
	return
}
if !checkKey(user, r.Header.Get("x-auth-key")) {
	guesses.Fail(addr, user+"\x00"+kosync.KeyHash(r.Header.Get("x-auth-key")))
	kosync.WriteError(w, http.StatusUnauthorized, kosync.ErrUnauthorized, "Wrong user name or key.")
	return
}
```

## Tests

`go test ./pkg/kosync` checks `PartialMD5` against the conformance suite's
vectors. With `KOREADER_TEST_DATA` pointing at a checkout of
[koreader/test-data](https://github.com/koreader/test-data), it also checks
the values KOReader's own tests pin.
