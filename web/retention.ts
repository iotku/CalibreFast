/**
 * What stays in this browser's book store (book-cache.ts), and for how long,
 * by the user's preferences for this device (prefs.ts, /preferences):
 *
 *   - Reading: kept once opened, until unopened for readingKeepDays (or for
 *     good); the most recently read keepReadingN stay regardless.
 *   - Saved: only the first prefetchSavedN of On deck (the next book in a
 *     series, which counts as Saved here) then Saved (oldest first),
 *     downloaded ahead of time (book-sync.ts) and dropped once no longer
 *     among them. No age rule. Oldest, so saving more books doesn't swap
 *     one download for another.
 *     One opened here is being read, whatever its status says yet: as
 *     Reading. (A copy downloaded ahead of time has lastOpened 0 until then.)
 *   - Finished: until unopened for FINISHED_KEEP_DAYS (a day: there to go
 *     back to straight away, not to keep).
 *   - Not Reading (a status taken away): dropped at once, pin and all.
 *   - Pinned ("Always keep", offline-control.ts): for good, while the book
 *     has a status (a guest's books have none: for good).
 *
 * On top of that, all kept books stay under the storage limit (storageLimit:
 * the user's, else half of what the browser allows): room is
 * made by dropping Finished books (oldest opened first), then Saved copies
 * never opened here (most recently saved first), then other books not being read
 * (oldest opened first). Books being read, pinned ones and the one being
 * opened are never dropped for room. And no file over maxBookBytes is kept
 * unless pinned.
 *
 * Storage that fails (full, or blocked by the browser) is logged to the
 * console with what it means, since it otherwise shows only as books that
 * aren't there.
 *
 * Statuses come from the reading marks (reading.ts): passed in fresh (the
 * grid, book-sync.ts), otherwise the last ones cached. Cached ones may be
 * older than a status set since, so they only order and group: dropping a
 * book for being Not Reading needs fresh ones. A guest has none: every book
 * counts as being read.
 */

import {
    currentUser,
    deleteBook,
    getBook,
    getPartial,
    keepReaderPage,
    listBooks,
    listPartials,
    offlinePref,
    putBook,
    setOfflinePref,
    updateBook,
    type CachedBook,
} from "./book-cache.js";
import { localKey, type Owner } from "./local-keys.js";
import { readingSnapshot } from "./offline-books.js";
import { AUTO_LIMIT, devicePrefs, FALLBACK_LIMIT, FINISHED_KEEP_DAYS, type DevicePrefs } from "./prefs.js";
import { cachedMarks, fetchMarks, statusOf as statusIn } from "./reading.js";

const DAY_MS = 24 * 3600 * 1000;

/** A book's status ("", "wanted", "reading", "finished"); undefined: not known. */
type StatusFn = (uuid: string) => string | undefined;

/**
 * Statuses: given, else the cached marks if they're this user's; none for a
 * guest. An On deck book downloaded ahead of time (the next in a series,
 * with no status of its own) counts as Saved while the list has it.
 */
function statuses(given?: (uuid: string) => string): StatusFn {
    const base = given ?? marksStatus();
    const deck = new Set((savedList(currentUser())?.books ?? []).filter((b) => b.on_deck).map((b) => b.uuid));
    if (!deck.size) return base;
    return (u) => {
        const s = base(u);
        return s === "" && deck.has(u) ? "wanted" : s;
    };
}

function marksStatus(): StatusFn {
    const user = currentUser();
    const m = cachedMarks();
    if (user !== null && m.signedIn && m.user === user) return (u) => statusIn(m, u);
    return () => undefined;
}

/* ------------------------------------------------------------------ */
/* The Saved list, in its order (GET /api/me/saved-books)            */
/* ------------------------------------------------------------------ */

/**
 * A book to download ahead of time, as /api/me/saved-books lists them: the
 * On deck books (the next in a series), then Saved (oldest saved first),
 * as Discover's "On deck / Saved for later" row shows them.
 */
export interface SavedBook {
    uuid: string;
    title: string;
    authors: string;
    /** When it was saved (ms); none for an On deck book. */
    saved_at?: number;
    /** The next book in a series the user finished one of: no status of its own, kept as Saved. */
    on_deck?: boolean;
}

export interface SavedList {
    at: number;
    books: SavedBook[];
    /** How many were asked for, and the answer's tag (listtag.go): the same again is answered 304. */
    limit?: number;
    tag?: string;
}

export const SAVED_REFRESH_MS = 10 * 60_000;

/** The list as last fetched for this device (book-sync.ts): the first N, On deck then Saved. */
export function savedList(owner: Owner): SavedList | null {
    if (owner === null) return null;
    try {
        return JSON.parse(localStorage.getItem(localKey(owner, "saved-books")) ?? "null") as SavedList | null;
    } catch (err) {
        console.warn("retention: the Saved list kept in localStorage can't be read (blocked, or corrupt); Saved copies are left alone until it's fetched again:", err);
        return null;
    }
}

export function writeSavedList(owner: string, list: SavedList): void {
    try {
        localStorage.setItem(localKey(owner, "saved-books"), JSON.stringify(list));
    } catch (err) {
        console.warn(
            "retention: couldn't keep the Saved list in localStorage (storage full, or blocked by the browser). " +
                "Saved books downloaded ahead of time can't be told apart from older ones, so they're left alone, and the list is fetched again next time:",
            err,
        );
    }
}

/* ------------------------------------------------------------------ */
/* Limits                                                             */
/* ------------------------------------------------------------------ */

let quotaAsked: Promise<number> | null = null;

/** What the browser lets this site store (bytes; 0: it doesn't say). Asked once per page. */
function browserQuota(): Promise<number> {
    quotaAsked ??= (navigator.storage?.estimate?.() ?? Promise.resolve({} as StorageEstimate)).then((e) => e.quota ?? 0).catch(() => 0);
    return quotaAsked;
}

/**
 * The storage limit for kept books (bytes; 0: none): the user's choice, or
 * by default half of what the browser lets this site store, so books never
 * crowd out the rest or push the browser to clear the site (1 GB when it
 * doesn't say). Decided here, as books are kept: most people never open
 * /preferences.
 */
export async function storageLimit(p: DevicePrefs = devicePrefs()): Promise<number> {
    if (p.maxBytes !== AUTO_LIMIT) return p.maxBytes;
    const half = Math.floor((await browserQuota()) / 2);
    return half || FALLBACK_LIMIT;
}

/** The On deck and Saved books to keep here (the first N); null: the list isn't known yet (keep what's here). */
function savedToKeep(owner: Owner, prefs: DevicePrefs): Set<string> | null {
    const list = savedList(owner);
    return list ? new Set(list.books.slice(0, prefs.prefetchSavedN).map((b) => b.uuid)) : null;
}

/* ------------------------------------------------------------------ */
/* What to keep                                                       */
/* ------------------------------------------------------------------ */

/** Being read, as far as retention goes: Reading, or no status known, or none yet. */
const readingClass = (s: string | undefined, b: CachedBook): boolean => s === undefined || s === "reading" || (s === "" && !b.statusSeen);

/** The most recently read keepReadingN books being read: kept however long unopened, never dropped for room. */
function recentlyRead(books: CachedBook[], status: StatusFn, prefs: DevicePrefs): Set<string> {
    if (!prefs.keepReadingN) return new Set();
    const byUuid = new Map(books.map((b) => [b.uuid, b]));
    const order = [
        ...(readingSnapshot()?.books.map((b) => b.uuid) ?? []),
        ...[...books].sort((a, b) => b.lastOpened - a.lastOpened).map((b) => b.uuid),
    ];
    const out = new Set<string>();
    for (const uuid of order) {
        const b = byUuid.get(uuid);
        if (!b || out.has(uuid) || !readingClass(status(uuid), b)) continue;
        out.add(uuid);
        if (out.size >= prefs.keepReadingN) break;
    }
    return out;
}

/**
 * Whether a reader should keep the file it has (size bytes) of this book:
 * pinned, always; set to never, or automatic keeping off, or over the size
 * limit, no.
 */
export function shouldKeep(uuid: string, size: number): boolean {
    const pref = offlinePref(uuid);
    if (pref === "keep") return true;
    if (pref === "never") return false;
    const p = devicePrefs();
    return p.readingKeepDays !== -1 && fitsBookLimit(size, p);
}

export function fitsBookLimit(size: number, p: DevicePrefs = devicePrefs()): boolean {
    return !p.maxBookBytes || size <= p.maxBookBytes;
}

/**
 * Apply the rules above to every kept book (but protect, the one being
 * opened), then keep under the storage limit.
 *
 * Only call this after the server has answered: offline, a kept book may be
 * the only way to read it, however long ago it was opened.
 */
export async function sweep(statusOf?: (uuid: string) => string, protect?: string): Promise<void> {
    const user = currentUser();
    const prefs = devicePrefs(user);
    const status = statuses(statusOf);
    const now = Date.now();
    const books = await listBooks();
    const recent = recentlyRead(books, status, prefs);
    const saved = savedToKeep(user, prefs);
    for (const b of books) {
        if (b.uuid === protect) continue;
        const format = b.format ?? "epub";
        const s = status(b.uuid);
        // Taken off every list (Not Reading): gone, pin and all. Only on the
        // server's word: cached marks may predate the status this copy saw.
        if (statusOf && s === "" && b.statusSeen) {
            if (offlinePref(b.uuid) === "keep") await setOfflinePref(b.uuid, "auto");
            await deleteBook(b.uuid, format);
            continue;
        }
        if (statusOf && s && s !== b.statusSeen) await updateBook(b.uuid, format, { statusSeen: s });
        if (offlinePref(b.uuid) === "keep") continue;
        // Since it was opened here (or, never opened, downloaded for its status).
        const age = now - (b.lastOpened || b.savedAt || now);
        const asReading = (): boolean =>
            prefs.readingKeepDays !== -1 && (recent.has(b.uuid) || prefs.readingKeepDays === 0 || age <= prefs.readingKeepDays * DAY_MS);
        let keep: boolean;
        if (!fitsBookLimit(b.size, prefs)) keep = false;
        else if (s === "wanted") keep = saved === null || saved.has(b.uuid) || (b.lastOpened > 0 && asReading());
        else if (s === "finished") keep = age <= FINISHED_KEEP_DAYS * DAY_MS;
        else keep = asReading();
        if (!keep) await deleteBook(b.uuid, format);
    }
    await makeRoom(0, protect);
}

/**
 * Drop kept books until need more bytes fit under the storage limit:
 * Finished ones first (oldest opened first), then Saved ones never opened
 * here (oldest saved first), then others not being read (oldest opened
 * first). Never pinned books, books being read, or protect. Returns whether
 * it fits now.
 */
export async function makeRoom(need: number, protect?: string): Promise<boolean> {
    const prefs = devicePrefs();
    const limit = await storageLimit(prefs);
    if (!limit) return true;
    const books = await listBooks();
    let total = (await usage()).used + need;
    if (total <= limit) return true;
    const status = statuses();
    const recent = recentlyRead(books, status, prefs);
    const savedAt = new Map((savedList(currentUser())?.books ?? []).map((b) => [b.uuid, b.saved_at]));
    const when = (b: CachedBook): number => savedAt.get(b.uuid) ?? b.savedAt ?? b.lastOpened;
    const candidates = books.filter((b) => b.uuid !== protect && offlinePref(b.uuid) !== "keep" && !recent.has(b.uuid));
    const byOpened = (a: CachedBook, b: CachedBook): number => a.lastOpened - b.lastOpened;
    const finished = candidates.filter((b) => status(b.uuid) === "finished").sort(byOpened);
    // Saved copies never opened here next, most recently saved first: the oldest Saved
    // are the ones chosen to keep (book-sync.ts), so these would only come back.
    // One opened is someone's reading, with the rest.
    const savedFirst = candidates.filter((b) => status(b.uuid) === "wanted" && !b.lastOpened).sort((a, b) => when(b) - when(a));
    const notReading = candidates
        .filter((b) => {
            const s = status(b.uuid);
            return s === "wanted" ? b.lastOpened > 0 : s !== "finished" && !readingClass(s, b);
        })
        .sort(byOpened);
    // A guest's books have no status: the least recently opened go.
    const guest = currentUser() === null ? [...candidates].sort(byOpened) : [];
    const dropped: string[] = [];
    for (const b of [...finished, ...savedFirst, ...notReading, ...guest]) {
        if (total <= limit) break;
        await deleteBook(b.uuid, b.format);
        dropped.push(b.title);
        total -= b.size;
    }
    if (dropped.length) console.info(`retention: dropped ${dropped.length} kept book(s) to stay under the ${mb(limit)} storage limit:`, dropped);
    if (total > limit) {
        console.warn(
            `retention: kept books use ${mb(total)}, over the ${mb(limit)} storage limit, ` +
                "and what's left can't be dropped for room (books being read, pinned ones, the one being opened).",
        );
    }
    return total <= limit;
}

const mb = (n: number): string => `${(n / 1048576).toFixed(1)} MB`;

/** Keep a file, making room for it first (a reader's copy, a download). */
export async function storeBook(book: CachedBook): Promise<void> {
    const format = book.format ?? "epub";
    const had = await getBook(book.uuid, format).catch(() => null);
    // A download finishing: its partial (counted in usage) becomes this book, so don't count it twice.
    const part = await getPartial(book.uuid, format).catch(() => null);
    await makeRoom(book.size - (had?.size ?? 0) - (part?.got ?? 0), book.uuid);
    try {
        await putBook(book);
    } catch (err) {
        // Otherwise only seen as a book that isn't there next time, offline.
        const est = await navigator.storage?.estimate?.().catch(() => null);
        const quota = (err as { name?: string } | null)?.name === "QuotaExceededError";
        console.warn(
            `retention: couldn't keep "${book.title}" (${mb(book.size)}) in this browser: ` +
                (quota
                    ? "the browser's storage for this site is full"
                    : "IndexedDB failed (storage blocked: private browsing, site data turned off, or the disk full)") +
                (est?.quota ? `; the site uses ${mb(est.usage ?? 0)} of the ${mb(est.quota)} the browser allows` : "") +
                ". It still reads from the server.",
            err,
        );
        throw err;
    }
    keepReaderPage(book.uuid, format);
    requestPersist();
}

/** How much the kept books use now, and the limit (0: none). */
export async function usage(): Promise<{ used: number; limit: number }> {
    const books = await listBooks();
    const partial = await listPartials().catch(() => []);
    const used = books.reduce((n, b) => n + b.size, 0) + partial.reduce((n, p) => n + p.got, 0);
    return { used, limit: await storageLimit() };
}

/** sweep with the server's statuses when it answers (else the cached ones, which drop nothing for status). */
export async function sweepFresh(protect?: string): Promise<void> {
    const m = await fetchMarks();
    await sweep(m?.signedIn ? (u) => statusIn(m, u) : undefined, protect);
}

/* ------------------------------------------------------------------ */
/* Persistent storage                                                 */
/* ------------------------------------------------------------------ */

let persistAsked = false;

/**
 * Ask the browser not to clear this site's storage when space runs low
 * (it may say no, or ask the user). Once per page.
 */
export function requestPersist(): void {
    if (persistAsked) return;
    persistAsked = true;
    void navigator.storage
        ?.persisted?.()
        .then((yes) => (yes ? true : navigator.storage.persist()))
        .catch(() => false);
}
