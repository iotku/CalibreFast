# CalibreFast

A fast, minimal web frontend for your [Calibre](https://calibre-ebook.com/) library.

> ⚠️ **Experimental.** CalibreFast is not feature complete and comes with no security guarantees. **Do not expose it to the public internet**. It is intended for personal use on a trusted local network (e.g. private wireguard/tailscale network).
>
> 🔒 **There is no authentication.** The server starts **read-only**. With `-allow-writes`, **anyone who can reach it can change your library**. Read [SECURITY.md](docs/SECURITY.md) before turning that on.

![Search Screenshot](image-3.png)

![Book Info](image-2.png)
---

## Is this AI Slop?

Perhaps. This project is built with AI assistance, and I audit and test much of the non-boilerplate code by hand. See [AI-DISCLOSURE.md](docs/AI-DISCLOSURE.md) for the full disclosure.

## What it is

CalibreFast is a personal project of mine, and I built it to scratch my own itch. I wanted a fast, simple way to browse my longstanding Calibre library without the slow performance of interfaces like Calibre-web or high resource usage of Booklore. 

CalibreFast reads directly from your Calibre `metadata.db` and serves your library as a snappy, paginated grid. Pages are pre-generated as static JSON at startup so browsing feels instant — no database round-trips while you scroll.

## Features

- **Fast browsing.** Scroll your whole library in one grid, or jump straight to any page.
- **Book details.** A popup for each book with read and download buttons for every format (EPUB, PDF, MOBI, AZW3, DJVU).
- **Read in the browser:** EPUB and PDF readers.
- **Search** by title or author.
- **Browse by author, publisher, tag or language,** with language codes shown as names (`eng` → English).
- **Groups** merge values that mean the same thing, like `en`/`eng`/`en-US` → English, without editing any books.
- **Metadata editor:** tag and language chips, autocomplete from your library, a formatted description editor, side-by-side comparison of your changes, and safeguards so curated metadata isn't lost.
- **Uploads:** EPUB/PDF metadata is read automatically, and multiple formats of one book are combined.

Editing metadata, uploads and creating groups require starting the server with `-allow-writes`.

See [docs/FEATURES.md](docs/FEATURES.md) for the details.

## What it is not

- Not production ready
- Not authenticated or access controlled in any way — see [SECURITY.md](docs/SECURITY.md)
- Not a full Calibre replacement or sync tool

## Getting started

Requires Go 1.25+ and a Calibre library with a `metadata.db`.

```bash
git clone https://github.com/iotku/CalibreFast.git
cd CalibreFast
go run . -basedir /path/to/calibre/library -cachedir /path/to/cache
```

Then open [http://localhost:8080](http://localhost:8080).

| Flag | Description |
|------|-------------|
| `-basedir` | Path to your Calibre library root (required) |
| `-cachedir` | Where to store cover thumbnails (default: the current directory) |
| `-port` | Port to listen on (default `8080`) |
| `-allow-writes` | Allow editing, uploads, cover changes and groups. **Off by default.** With no authentication, anyone who can reach the server can then modify your library — read [SECURITY.md](docs/SECURITY.md) first. |

## Documentation

- [SECURITY.md](docs/SECURITY.md): the risks, and how to run CalibreFast safely
- [FEATURES.md](docs/FEATURES.md): how everything behaves, in detail
- [ARCHITECTURE.md](docs/ARCHITECTURE.md): how it works under the hood
- [notes/Performance.md](docs/notes/Performance.md): performance notes
- [AI-DISCLOSURE.md](docs/AI-DISCLOSURE.md): how AI is used in this project

## License

MIT
