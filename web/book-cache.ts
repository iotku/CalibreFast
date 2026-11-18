/**
 * Whole book files kept in the browser (IndexedDB "cf-books": .epub files in
 * "books", .pdf files in "pdfs"), so a book read before opens at once,
 * without a download, and reads with no server at all (with the service
 * worker, sw.ts). A book with both formats may have either or both kept.
 *
 * This is the store itself. What is kept and for how long (the readers'
 * copies, Saved books downloaded ahead of time, the user's storage limits)
 * is retention.ts, by the preferences in prefs.ts. Each copy belongs to the
 * user who read it: another user signing in, or signing out, removes it.
 *
 * Downloads cut off part way (the page left, downloads paused) keep what
 * arrived in "partials", so the next one continues from there (HTTP Range,
 * book-sync.ts) instead of starting over on a slow connection.
 *
 * Each book can be set (offline-control.ts, the book's page) to be kept for
 * good ("keep") or never kept ("never": the reader streams it and keeps
 * nothing). The choice is per user and per device, in localStorage.
 */

import { scriptUrl } from "./trusted.js";

const DB = "cf-books";
/** The kept files of each format, keyed by the book's uuid. */
const STORES = { epub: "books", pdf: "pdfs" } as const;

/** The formats the readers open, and so the ones kept. */
export type KeptFormat = keyof typeof STORES;
export const KEPT_FORMATS = Object.keys(STORES) as KeptFormat[];

/** The reader for a book's format. */
export function readerUrl(uuid: string, format: KeptFormat): string {
    return `${format === "pdf" ? "/read-pdf" : "/read"}?uuid=${encodeURIComponent(uuid)}`;
}

export interface CachedBook {
    uuid: string;
    /** Which file this is (records from before PDFs were kept have none: EPUB). */
    format?: KeptFormat;
    /** Username, or null for an anonymous reader (-public-read). */
    user: string | null;
    bytes: Blob;
    lastModified: string;
    size: number;
    title: string;
    authors: string;
    /** epub.js locations (page numbers), once generated (EPUB only). */
    locations?: string;
    lastOpened: number;
    /** The last status seen for the book, other than Not Reading. */
    statusSeen?: string;
    /** When it was saved (ms), for a Saved book downloaded ahead of time (retention.ts). */
    savedAt?: number;
}

let dbPromise: Promise<IDBDatabase> | null = null;
/** The upgrade waits for a page of an older build to let go of the database. */
let blocked = false;

function db(): Promise<IDBDatabase> {
    // Until then every request would queue behind it: do without a cache
    // (the readers read from the server) instead of waiting.
    if (blocked) return Promise.reject(new Error("the book cache is in use by an older page"));
    dbPromise ??= new Promise((resolve, reject) => {
        const req = indexedDB.open(DB, 3);
        req.onupgradeneeded = () => {
            // Version 1 had only "books" (EPUBs); 2 adds "pdfs"; 3 "partials".
            for (const name of Object.values(STORES)) {
                if (!req.result.objectStoreNames.contains(name)) req.result.createObjectStore(name, { keyPath: "uuid" });
            }
            if (!req.result.objectStoreNames.contains(PARTIALS)) req.result.createObjectStore(PARTIALS, { keyPath: "key" });
        };
        req.onsuccess = () => {
            const d = req.result;
            // A newer build in another tab needs to upgrade it: let go.
            d.onversionchange = () => {
                d.close();
                dbPromise = null;
            };
            blocked = false;
            dbPromise = Promise.resolve(d); // also after giving up on it while blocked
            resolve(d);
        };
        req.onerror = () => {
            blocked = false;
            dbPromise = null;
            reject(req.error);
        };
        req.onblocked = () => {
            blocked = true;
            reject(new Error("the book cache is in use by an older page"));
        };
    });
    return dbPromise;
}

async function tx<T>(format: KeptFormat, mode: IDBTransactionMode, run: (s: IDBObjectStore) => IDBRequest<T> | void): Promise<T | undefined> {
    return txIn(STORES[format], mode, run);
}

async function txIn<T>(store: string, mode: IDBTransactionMode, run: (s: IDBObjectStore) => IDBRequest<T> | void): Promise<T | undefined> {
    const d = await db();
    return new Promise((resolve, reject) => {
        const t = d.transaction(store, mode);
        const req = run(t.objectStore(store));
        t.oncomplete = () => resolve(req ? req.result : undefined);
        t.onerror = () => reject(t.error);
        t.onabort = () => reject(t.error);
    });
}

/** Who is signed in, as nav.ts last saw it (null: nobody, or unknown). */
export function currentUser(): string | null {
    try {
        const me = localStorage.getItem("cf-me");
        return me ? ((JSON.parse(me) as { username: string | null }).username ?? null) : null;
    } catch {
        return null;
    }
}

/** All records of one format, whoever they belong to, with their format set. */
async function allOf(format: KeptFormat): Promise<CachedBook[]> {
    return ((await tx<CachedBook[]>(format, "readonly", (s) => s.getAll())) ?? []).map((b) => ({ ...b, format }));
}

/** This user's kept file of a book in a format. */
export async function getBook(uuid: string, format: KeptFormat = "epub"): Promise<CachedBook | null> {
    const b = await tx<CachedBook>(format, "readonly", (s) => s.get(uuid));
    return b && b.user === currentUser() ? { ...b, format } : null;
}

/** This user's kept files of a book, in every format. */
export async function getBookFiles(uuid: string): Promise<CachedBook[]> {
    const found = await Promise.all(KEPT_FORMATS.map((f) => getBook(uuid, f)));
    return found.filter((b): b is CachedBook => b !== null);
}

export async function putBook(book: CachedBook): Promise<void> {
    const format = book.format ?? "epub";
    await tx(format, "readwrite", (s) => s.put({ ...book, format }));
    await deletePartial(book.uuid, format).catch(() => {}); // whole now, however it arrived
    changed(book.uuid);
}

/* ------------------------------------------------------------------ */
/* Downloads cut off part way                                         */
/* ------------------------------------------------------------------ */

const PARTIALS = "partials";

/** The first got bytes of a book's file, and what's needed to ask for the rest. */
export interface PartialBook {
    key: string;
    uuid: string;
    format: KeptFormat;
    user: string | null;
    bytes: Blob;
    got: number;
    /** The whole file's size, and its Last-Modified (If-Range: the rest only if it's the same file). */
    total: number;
    lastModified: string;
}

const partialKey = (uuid: string, format: KeptFormat): string => `${format}:${uuid}`;

/** This user's partial download of a book's file, if any. */
export async function getPartial(uuid: string, format: KeptFormat): Promise<PartialBook | null> {
    const p = await txIn<PartialBook>(PARTIALS, "readonly", (s) => s.get(partialKey(uuid, format)));
    return p && p.user === currentUser() ? p : null;
}

export async function putPartial(p: Omit<PartialBook, "key">): Promise<void> {
    await txIn(PARTIALS, "readwrite", (s) => s.put({ ...p, key: partialKey(p.uuid, p.format) }));
}

export async function deletePartial(uuid: string, format: KeptFormat): Promise<void> {
    await txIn(PARTIALS, "readwrite", (s) => s.delete(partialKey(uuid, format)));
}

/** All of this user's partial downloads (their bytes count against the storage limit). */
export async function listPartials(): Promise<PartialBook[]> {
    const all = (await txIn<PartialBook[]>(PARTIALS, "readonly", (s) => s.getAll())) ?? [];
    return all.filter((p) => p.user === currentUser());
}

/** Change some fields of a cached book's file. */
export async function updateBook(uuid: string, format: KeptFormat, change: Partial<CachedBook>): Promise<void> {
    const b = await tx<CachedBook>(format, "readonly", (s) => s.get(uuid));
    if (b) await putBook({ ...b, ...change, format });
}

/* ------------------------------------------------------------------ */
/* Keep for good, or never                                            */
/* ------------------------------------------------------------------ */

/** auto: kept by the rules in retention.ts; keep: for good; never: not at all. */
export type OfflinePref = "auto" | "keep" | "never";

const PREFS = "cf-offline-prefs";

type Prefs = Record<string, Record<string, Exclude<OfflinePref, "auto">>>; // user ("" signed out) -> uuid -> pref

function readPrefs(): Prefs {
    try {
        return (JSON.parse(localStorage.getItem(PREFS) ?? "null") as Prefs | null) ?? {};
    } catch {
        return {};
    }
}

export function offlinePref(uuid: string): OfflinePref {
    return readPrefs()[currentUser() ?? ""]?.[uuid] ?? "auto";
}

/** Tells pages showing the book (cards, its popup) that its copy or setting changed. */
function changed(uuid: string): void {
    window.dispatchEvent(new CustomEvent<string>("cf:offline-changed", { detail: uuid }));
}

/**
 * Set a book's preference (for all its formats). "never" drops kept copies;
 * "keep" doesn't fetch one by itself (book-sync.ts's downloadForOffline does).
 */
export async function setOfflinePref(uuid: string, pref: OfflinePref): Promise<void> {
    const prefs = readPrefs();
    const who = currentUser() ?? "";
    const mine = (prefs[who] ??= {});
    if (pref === "auto") delete mine[uuid];
    else mine[uuid] = pref;
    try {
        localStorage.setItem(PREFS, JSON.stringify(prefs));
    } catch {
        /* storage full or blocked */
    }
    if (pref === "never") await deleteBook(uuid);
    changed(uuid);
}

/** The uuids of this user's kept books (for the cards' "on this device" mark). */
export async function keptUuids(): Promise<Set<string>> {
    return new Set((await listBooks()).map((b) => b.uuid));
}

/** Every cached file, each format of a book on its own (for the offline page). */
export async function listBooks(): Promise<CachedBook[]> {
    const all = (await Promise.all(KEPT_FORMATS.map(allOf))).flat();
    return all.filter((b) => b.user === currentUser());
}

/** Drop a book's kept file in a format, or in every format. */
export async function deleteBook(uuid: string, format?: KeptFormat): Promise<void> {
    for (const f of format ? [format] : KEPT_FORMATS) {
        await tx(f, "readwrite", (s) => s.delete(uuid));
        await deletePartial(uuid, f).catch(() => {});
        tellWorker({ type: "evict", uuid, format: f });
    }
    changed(uuid);
}

/** Remove copies that belong to anyone but this user (sign-out, another user). */
export async function keepOnlyUser(user: string | null): Promise<void> {
    let dropped = 0;
    for (const format of KEPT_FORMATS) {
        const others = (await allOf(format)).filter((b) => b.user !== user);
        for (const b of others) await tx(format, "readwrite", (s) => s.delete(b.uuid));
        dropped += others.length;
    }
    const parts = (await txIn<PartialBook[]>(PARTIALS, "readonly", (s) => s.getAll()).catch(() => [])) ?? [];
    for (const p of parts.filter((x) => x.user !== user)) await txIn(PARTIALS, "readwrite", (s) => s.delete(p.key));
    if (dropped) tellWorker({ type: "clear-books" });
}

/** Tell the service worker (sw.ts) to drop cached reader pages. */
function tellWorker(msg: { type: "evict"; uuid: string; format: KeptFormat } | { type: "clear-books" }): void {
    try {
        navigator.serviceWorker?.controller?.postMessage(msg);
    } catch {
        /* no service worker */
    }
}

/**
 * Register the service worker (sw.ts), which lets kept books open with no
 * server. Browsers only allow it on HTTPS and localhost.
 */
export function registerWorker(): void {
    if (!isSecureContext || !("serviceWorker" in navigator)) return;
    navigator.serviceWorker.register(scriptUrl("/sw.js")).catch((err: unknown) => console.warn("service worker:", err));
}
