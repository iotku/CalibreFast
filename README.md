# CalibreFast

A fast, minimal web frontend for your [Calibre](https://calibre-ebook.com/) library. Built in 2 days with AI assistance — quick to make, quick to use.

> ⚠️ **Experimental.** CalibreFast is not feature complete and comes with no security guarantees. Do not expose it to the public internet. It is intended for personal use on a trusted local network.

---

## What it is

CalibreFast reads directly from your Calibre `metadata.db` and serves your library as a snappy, paginated grid. Pages are pre-generated as static JSON at startup so browsing feels instant — no database round-trips while you scroll. Search hits the database directly with full title and author support.

- Paginated book grid with infinite scroll
- Fast cover thumbnails with lazy loading and a prefetch queue
- Search by title or author
- Download books in any format Calibre has (EPUB, PDF, MOBI, AZW3, DJVU)
- Book detail popup on click
- Keyboard and touch friendly (?)

## What it is not

- Not production ready
- Not authenticated or access controlled in any way
- Not a Calibre replacement or sync tool
- Not tested beyond a single-user local setup

## Getting started

### Prerequisites

- Go 1.21+
- A Calibre library with a `metadata.db`

### Run

```bash
git clone https://github.com/yourname/calibrefast
cd calibrefast
go run . -basedir /path/to/calibre/library -cachedir /path/to/cache
```

Then open [http://localhost:8080](http://localhost:8080).

### Flags

| Flag | Description |
|------|-------------|
| `-basedir` | Path to your Calibre library root (required) |
| `-cachedir` | Path to store generated cover thumbnails |

## How it works

On startup, CalibreFast queries `metadata.db` and writes paginated JSON files (`pages/page1.json`, `page2.json`, …) to disk. The frontend fetches these directly as static files — no database hit per page. Cover thumbnails are generated on first request and cached to disk.

Search is the one exception: it queries SQLite directly, joining the `books` and `authors` tables with a `LIKE` filter.

## Security

There is no authentication. Anyone who can reach the server can browse and download your entire library. Run it on localhost or a private network only. You have been warned.

## License

MIT