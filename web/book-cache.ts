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

import { localKey } from "./local-keys.js";
import { scriptUrl } from "./trusted.js";

const DB = "cf-books";
/** The kept files of each format, keyed by the book's uuid. */
const STORES = { epub: "books", pdf: "pdfs" } as const;

/** The formats the readers open, and so the ones kept. */
export type KeptFormat = keyof typeof STORES;
export const KEPT_FORMATS = Object.keys(STORES) as KeptFormat[];

/** The reader for a book's format (formats.ts). */
export { readerUrl } from "./formats.js";

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
        const req = indexedDB.open(DB, 4);
        req.onupgradeneeded = (e) => {
            // Version 1 had only "books" (EPUBs); 2 adds "pdfs"; 3 "partials";
            // 4 keeps a partial's bytes in "partial-chunks", a record per piece.
            const d = req.result;
            for (const name of Object.values(STORES)) {
                if (!d.objectStoreNames.contains(name)) d.createObjectStore(name, { keyPath: "uuid" });
            }
            if (!d.objectStoreNames.contains(PARTIALS)) d.createObjectStore(PARTIALS, { keyPath: "key" });
            // Version 3's partials held their bytes in one record: start those over.
            else if (e.oldVersion < 4) req.transaction?.objectStore(PARTIALS).clear();
            if (!d.objectStoreNames.contains(CHUNKS)) d.createObjectStore(CHUNKS); // keys: [partial key, piece number]
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
    forgetPinDownload(book.uuid);
    changed(book.uuid);
}

/* ------------------------------------------------------------------ */
/* Downloads cut off part way                                         */
/* ------------------------------------------------------------------ */

const PARTIALS = "partials";
/**
 * A partial's bytes, a record per piece as it arrived (keyed [partial key,
 * piece number]): saving what arrived writes only the new piece, not the
 * whole file so far again.
 */
const CHUNKS = "partial-chunks";

/** What's kept of a download cut off part way, and what's needed to ask for the rest. */
export interface PartialInfo {
    key: string;
    uuid: string;
    format: KeptFormat;
    user: string | null;
    /** For the preferences page (none on partials from before they were kept). */
    title?: string;
    authors?: string;
    /** Bytes kept (the pieces' sizes added up), and how many pieces. */
    got: number;
    chunks: number;
    /** The whole file's size, and its Last-Modified (If-Range: the rest only if it's the same file). */
    total: number;
    lastModified: string;
}

/** A partial with its bytes, to go on from. */
export interface PartialBook extends PartialInfo {
    bytes: Blob;
}

const partialKey = (uuid: string, format: KeptFormat): string => `${format}:${uuid}`;
const chunkRange = (key: string): IDBKeyRange => IDBKeyRange.bound([key, 0], [key, Infinity]);

/** Run one transaction over the partial stores. */
async function partialTx<T>(mode: IDBTransactionMode, run: (parts: IDBObjectStore, chunks: IDBObjectStore) => IDBRequest<T> | void): Promise<T | undefined> {
    const d = await db();
    return new Promise((resolve, reject) => {
        const t = d.transaction([PARTIALS, CHUNKS], mode);
        const req = run(t.objectStore(PARTIALS), t.objectStore(CHUNKS));
        t.oncomplete = () => resolve(req ? req.result : undefined);
        t.onerror = () => reject(t.error);
        t.onabort = () => reject(t.error);
    });
}

/** This user's partial download of a book's file, with its bytes, if any. */
export async function getPartial(uuid: string, format: KeptFormat): Promise<PartialBook | null> {
    const key = partialKey(uuid, format);
    let info: PartialInfo | undefined;
    let pieces: Blob[] = [];
    await partialTx("readonly", (parts, chunks) => {
        const a = parts.get(key);
        a.onsuccess = () => (info = a.result as PartialInfo | undefined);
        const b = chunks.getAll(chunkRange(key));
        b.onsuccess = () => (pieces = b.result as Blob[]);
    });
    if (!info || info.user !== currentUser()) return null;
    const bytes = new Blob(pieces);
    // Pieces missing (or more than the record says): not to be trusted.
    if (pieces.length !== info.chunks || bytes.size !== info.got) {
        await deletePartial(uuid, format).catch(() => {});
        return null;
    }
    return { ...info, bytes };
}

/**
 * Start a partial over: drop what's kept and record the new download
 * (nothing of it yet).
 */
export async function startPartial(p: Omit<PartialInfo, "key" | "got" | "chunks">): Promise<PartialInfo> {
    const info: PartialInfo = { ...p, key: partialKey(p.uuid, p.format), got: 0, chunks: 0 };
    await partialTx("readwrite", (parts, chunks) => {
        chunks.delete(chunkRange(info.key));
        parts.put(info);
    });
    return info;
}

/** Keep the next piece of a partial (the record and the piece together, or neither). */
export async function appendPartial(info: PartialInfo, piece: Blob): Promise<PartialInfo> {
    const next = { ...info, got: info.got + piece.size, chunks: info.chunks + 1 };
    await partialTx("readwrite", (parts, chunks) => {
        chunks.put(piece, [info.key, info.chunks]);
        parts.put(next);
    });
    return next;
}

export async function deletePartial(uuid: string, format: KeptFormat): Promise<void> {
    const key = partialKey(uuid, format);
    await partialTx("readwrite", (parts, chunks) => {
        parts.delete(key);
        chunks.delete(chunkRange(key));
    });
}

/** All of this user's partial downloads, without their bytes (which count against the storage limit). */
export async function listPartials(): Promise<PartialInfo[]> {
    const all = (await txIn<PartialInfo[]>(PARTIALS, "readonly", (s) => s.getAll())) ?? [];
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
    if (pref !== "keep") forgetPinDownload(uuid);
    if (pref === "never") await deleteBook(uuid);
    changed(uuid);
}

/**
 * A pinned book still to download, with what the queue needs to show it
 * (book-sync.ts downloads these first, until one is kept): a pin whose
 * download was cut off (the page closed, the connection lost) is finished
 * later, not left half there.
 */
export interface PinDownload {
    uuid: string;
    title: string;
    authors: string;
    format: KeptFormat;
    /** The status it was pinned with (retention.ts drops it once taken off every list). */
    status?: string;
}

const pinKey = (): string => localKey(currentUser(), "pin-downloads");

export function pinDownloads(): PinDownload[] {
    try {
        const list = JSON.parse(localStorage.getItem(pinKey()) ?? "[]") as PinDownload[] | null;
        return Array.isArray(list) ? list : [];
    } catch {
        return [];
    }
}

function writePinDownloads(list: PinDownload[]): void {
    try {
        if (list.length) localStorage.setItem(pinKey(), JSON.stringify(list));
        else localStorage.removeItem(pinKey());
    } catch {
        /* storage full or blocked: the pin is downloaded again only when chosen again */
    }
}

export function addPinDownload(pin: PinDownload): void {
    writePinDownloads([...pinDownloads().filter((p) => p.uuid !== pin.uuid), pin]);
}

function forgetPinDownload(uuid: string): void {
    const list = pinDownloads();
    if (list.some((p) => p.uuid === uuid)) writePinDownloads(list.filter((p) => p.uuid !== uuid));
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
    // No file of it left: its popup goes too.
    if (!(await getBookFiles(uuid).catch(() => [])).length) tellWorker({ type: "evict-book", uuid });
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
    const parts = (await txIn<PartialInfo[]>(PARTIALS, "readonly", (s) => s.getAll()).catch(() => [])) ?? [];
    for (const p of parts.filter((x) => x.user !== user)) await deletePartial(p.uuid, p.format).catch(() => {});
    if (dropped) tellWorker({ type: "clear-books" });
}

/**
 * Have the service worker (sw.ts) keep a book's reader page and its popup,
 * so a book downloaded ahead of time opens offline though its reader (or
 * its popup) was never opened in this browser. Sent to the active worker, not the page's controller: a
 * page loaded before the worker first installed has none.
 */
export function keepReaderPage(uuid: string, format: KeptFormat): void {
    if (!isSecureContext || !("serviceWorker" in navigator)) return;
    navigator.serviceWorker.ready
        .then((reg) => reg.active?.postMessage({ type: "keep", uuid, format }))
        .catch(() => {
            /* no service worker */
        });
}

/** Tell the service worker (sw.ts) to drop cached reader pages and book popups. */
function tellWorker(
    msg: { type: "evict"; uuid: string; format: KeptFormat } | { type: "evict-book"; uuid: string } | { type: "clear-books" },
): void {
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
