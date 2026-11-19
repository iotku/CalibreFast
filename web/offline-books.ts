/**
 * What the offline Reading page (offline.ts) needs, saved while online:
 *
 *   - the user's Reading list (titles and authors from /me/reading) and
 *     On deck / Saved for later (/api/me/saved-books), in localStorage, so
 *     books not kept on this device can still be shown (as placeholders)
 *     with no server;
 *   - the cover thumbnails of those books and of the books kept here
 *     (book-cache.ts), in the Cache Storage cache "cf-covers", which pages
 *     read directly (the service worker passes covers by, sw.ts).
 *
 * nav.ts refreshes them now and then on signed-in pages, the Reading page
 * every time, and the reader adds a book's cover when it keeps the book.
 * The readers also move a book to the top of the saved list as they save a
 * position (noteReadingHere), or take it off once it's finished: the server
 * does the same to its list, and the next page offline mustn't wait for a
 * refresh to show a book just started (reading it, then going offline, is
 * the usual way to get there).
 */

import { currentUser, listBooks } from "./book-cache.js";
import { LOCAL_FINISHED } from "./local-reading.js";

export interface ListedBook {
    uuid: string;
    title: string;
    authors: string;
    /** The book's last_modified: its cover changes with it. */
    stamp: string;
}

export interface ReadingSnapshot {
    user: string;
    at: number;
    books: ListedBook[];
    /** The answer's tag (listtag.go): sent back, an unchanged list is answered 304. */
    tag?: string;
    /** On deck, then Saved, as Discover's row has them (none: not fetched yet), and that answer's tag. */
    saved?: ListedBook[];
    savedTag?: string;
}

const KEY = "cf-offline-reading";
const COVERS = "cf-covers";
/** How often (at most) other pages refresh it; the Reading page always does. */
const REFRESH_MS = 10 * 60_000;
/** Covers kept, at most: the Reading list's first ones, then On deck / Saved, then kept books'. */
const MAX_COVERS = 80;
/** On deck / Saved books listed, at most. */
const MAX_SAVED = 100;

/** The Reading list last saved for whoever is signed in here (null if none, or someone else's). */
export function readingSnapshot(): ReadingSnapshot | null {
    try {
        const s = JSON.parse(localStorage.getItem(KEY) ?? "null") as ReadingSnapshot | null;
        return s && s.user === currentUser() ? s : null;
    } catch {
        return null;
    }
}

/** Whose list is saved, whoever is signed in now. */
function savedFor(): string | null {
    try {
        return (JSON.parse(localStorage.getItem(KEY) ?? "null") as ReadingSnapshot | null)?.user ?? null;
    } catch {
        return null;
    }
}

/** The cache key of a book's kept cover. */
export const coverKey = (uuid: string): string => `/cover-thumb/${encodeURIComponent(uuid)}`;

/** A kept cover, as an object URL (null when none is kept or there's no Cache Storage). */
export async function keptCover(uuid: string): Promise<string | null> {
    try {
        const res = await (await caches.open(COVERS)).match(coverKey(uuid));
        return res ? URL.createObjectURL(await res.blob()) : null;
    } catch {
        return null;
    }
}

/** Keep a book's cover (the reader, when it keeps the book). */
export async function keepCover(uuid: string, stamp = ""): Promise<void> {
    if (typeof caches === "undefined") return;
    const cache = await caches.open(COVERS);
    const key = coverKey(uuid);
    const had = await cache.match(key);
    if (had && (!stamp || had.headers.get("x-cf-stamp") === stamp)) return;
    const res = await fetch(`${key}?t=${encodeURIComponent(stamp)}`, { credentials: "same-origin" });
    if (!res.ok || !(res.headers.get("content-type") ?? "").startsWith("image/")) return;
    // Kept with its stamp, to fetch it again when the book (and so maybe its cover) changes.
    const headers = new Headers(res.headers);
    headers.set("x-cf-stamp", stamp);
    await cache.put(key, new Response(await res.blob(), { headers }));
}

let refreshing: Promise<void> | null = null;

/**
 * Save the Reading list and the covers the offline page shows. Without
 * force, only when the last save is older than REFRESH_MS. Signed-in only.
 */
export function refreshOfflineReading(force = false): Promise<void> {
    const user = currentUser();
    if (!user) return Promise.resolve();
    const last = readingSnapshot();
    if (!force && last && Date.now() - last.at < REFRESH_MS) return Promise.resolve();
    refreshing ??= save(user).finally(() => (refreshing = null));
    return refreshing;
}

async function save(user: string): Promise<void> {
    if (savedFor() !== user) await forgetOfflineReading(); // another user's covers aren't this one's
    const last = readingSnapshot();
    const [books, saved] = await Promise.all([
        fetchList("/me/reading?api=1&all=1", last?.tag, last?.books, (raw) =>
            (raw as { uuid: string; title: string; authors: string; last_modified: string; placeholder?: unknown }[])
                .filter((b) => !b.placeholder)
                .map((b) => ({ uuid: b.uuid, title: b.title, authors: b.authors, stamp: b.last_modified })),
        ),
        fetchList(`/api/me/saved-books?limit=${MAX_SAVED}`, last?.savedTag, last?.saved, (raw) =>
            (raw as { uuid: string; title: string; authors: string }[]).map((b) => ({ uuid: b.uuid, title: b.title, authors: b.authors, stamp: "" })),
        ),
    ]);
    if (!books) return; // signed out, or not answering
    const changed = books.changed || !!saved?.changed;
    writeSnapshot({
        user,
        at: Date.now(),
        books: books.list,
        tag: books.tag,
        ...(saved ? { saved: saved.list, savedTag: saved.tag } : last?.saved ? { saved: last.saved, savedTag: last.savedTag } : {}),
    });
    if (changed) await saveCovers([...books.list, ...(saved?.list ?? last?.saved ?? [])]);
}

/** A list, fetched again unless unchanged (304: the copy had). Null when it can't be had. */
async function fetchList(
    url: string,
    tag: string | undefined,
    had: ListedBook[] | undefined,
    parse: (raw: unknown) => ListedBook[],
): Promise<{ list: ListedBook[]; tag: string; changed: boolean } | null> {
    const res = await fetch(url, {
        credentials: "same-origin",
        headers: { accept: "application/json", ...(tag && had ? { "if-none-match": tag } : {}) },
    }).catch(() => null);
    if (!res) return null;
    if (res.status === 304 && had) return { list: had, tag: tag ?? "", changed: false };
    if (!res.ok || !(res.headers.get("content-type") ?? "").includes("json")) return null;
    return { list: parse(await res.json()), tag: res.headers.get("etag") ?? "", changed: true };
}

function writeSnapshot(snap: ReadingSnapshot): void {
    try {
        localStorage.setItem(KEY, JSON.stringify(snap));
    } catch (err) {
        console.warn("offline: couldn't keep the Reading list for the offline page in localStorage (storage full, or blocked by the browser):", err);
    }
}

/** Keep the covers of the lists' books and of the books kept here; drop the rest. */
async function saveCovers(listed: ListedBook[]): Promise<void> {
    if (typeof caches === "undefined") return;
    const kept = await listBooks().catch(() => []);
    const want = new Map<string, string>();
    for (const b of listed) if (!want.has(b.uuid)) want.set(b.uuid, b.stamp);
    for (const b of kept) if (!want.has(b.uuid)) want.set(b.uuid, "");
    const keep = [...want].slice(0, MAX_COVERS);
    for (const [uuid, stamp] of keep) {
        try {
            await keepCover(uuid, stamp);
        } catch {
            /* that one later */
        }
    }
    const wanted = new Set(keep.map(([uuid]) => coverKey(uuid)));
    const cache = await caches.open(COVERS);
    for (const req of await cache.keys()) {
        if (!wanted.has(new URL(req.url).pathname)) await cache.delete(req);
    }
}

/**
 * A reader saved a position (reading-sync.ts, signed in): the book is being
 * read, the most recently of all, unless it's now finished. Updates the
 * saved list in place, keeping its age, so the next refresh still comes on
 * time and replaces it with the server's own.
 */
export function noteReadingHere(user: string, uuid: string, percent: number, title: string): void {
    let snap: ReadingSnapshot | null;
    try {
        snap = JSON.parse(localStorage.getItem(KEY) ?? "null") as ReadingSnapshot | null;
    } catch {
        snap = null;
    }
    // None yet (or someone else's): a new one, old enough that the next page online refreshes it.
    if (!snap || snap.user !== user) snap = { user, at: 0, books: [] };
    const i = snap.books.findIndex((b) => b.uuid === uuid);
    const entry = i >= 0 ? snap.books.splice(i, 1)[0]! : { uuid, title, authors: "", stamp: "" };
    if (percent < LOCAL_FINISHED) snap.books.unshift(entry);
    else if (i < 0) return; // finished, and wasn't listed: nothing changes
    delete snap.tag; // no longer the server's answer: the next refresh fetches it whole
    try {
        localStorage.setItem(KEY, JSON.stringify(snap));
    } catch {
        /* storage full or blocked */
    }
}

/** Sign-out or another user: their list and covers aren't this one's. */
export async function forgetOfflineReading(): Promise<void> {
    try {
        localStorage.removeItem(KEY);
    } catch {
        /* blocked */
    }
    if (typeof caches !== "undefined") await caches.delete(COVERS).catch(() => false);
}
