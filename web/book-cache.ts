/**
 * Whole .epub files kept in the browser (IndexedDB "cf-books"), so a book
 * read before opens at once, without a download, and reads with no server
 * at all (with the service worker, sw.ts).
 *
 * A book stays until it hasn't been opened for a week, or its status goes
 * back to "not started". Each copy belongs to the user who read it: another
 * user signing in, or signing out, removes it.
 *
 * Each book can be set (offline-control.ts, the book's page) to be kept for
 * good ("keep": never dropped for age or status, and downloaded right away)
 * or never kept ("never": the reader streams it and keeps nothing). The
 * choice is per user and per device, in localStorage.
 */

const DB = "cf-books";
const STORE = "books";
const MAX_AGE_MS = 7 * 24 * 3600 * 1000;

export interface CachedBook {
    uuid: string;
    /** Username, or null for an anonymous reader (-public-read). */
    user: string | null;
    bytes: Blob;
    lastModified: string;
    size: number;
    title: string;
    authors: string;
    /** epub.js locations (page numbers), once generated. */
    locations?: string;
    lastOpened: number;
    /** The last status seen for the book, other than "not started". */
    statusSeen?: string;
}

let dbPromise: Promise<IDBDatabase> | null = null;

function db(): Promise<IDBDatabase> {
    dbPromise ??= new Promise((resolve, reject) => {
        const req = indexedDB.open(DB, 1);
        req.onupgradeneeded = () => req.result.createObjectStore(STORE, { keyPath: "uuid" });
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => reject(req.error);
    });
    return dbPromise;
}

async function tx<T>(mode: IDBTransactionMode, run: (s: IDBObjectStore) => IDBRequest<T> | void): Promise<T | undefined> {
    const d = await db();
    return new Promise((resolve, reject) => {
        const t = d.transaction(STORE, mode);
        const req = run(t.objectStore(STORE));
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

export async function getBook(uuid: string): Promise<CachedBook | null> {
    const b = await tx<CachedBook>("readonly", (s) => s.get(uuid));
    return b && b.user === currentUser() ? b : null;
}

export async function putBook(book: CachedBook): Promise<void> {
    await tx("readwrite", (s) => s.put(book));
    changed(book.uuid);
}

/** Change some fields of a cached book. */
export async function updateBook(uuid: string, change: Partial<CachedBook>): Promise<void> {
    const b = await tx<CachedBook>("readonly", (s) => s.get(uuid));
    if (b) await putBook({ ...b, ...change });
}

/* ------------------------------------------------------------------ */
/* Keep for good, or never                                            */
/* ------------------------------------------------------------------ */

/** auto: kept while it's being read (the week rule above); keep: for good; never: not at all. */
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
 * Set a book's preference. "never" drops a kept copy; "keep" doesn't fetch
 * one by itself (downloadForOffline does).
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

/**
 * Download a book's EPUB and keep it, without opening it (for "keep").
 * onProgress gets the fraction done (0..1) when the size is known.
 */
export async function downloadForOffline(uuid: string, title: string, authors: string, onProgress?: (f: number) => void): Promise<void> {
    const res = await fetch(`/download/${encodeURIComponent(uuid)}/epub`, { credentials: "same-origin" });
    if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
    const total = Number(res.headers.get("content-length")) || 0;
    const chunks: Uint8Array[] = [];
    let got = 0;
    const reader = res.body.getReader();
    for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
        got += value.length;
        if (total) onProgress?.(got / total);
    }
    const had = await getBook(uuid);
    await putBook({
        uuid,
        user: currentUser(),
        bytes: new Blob(chunks as BlobPart[], { type: "application/epub+zip" }),
        lastModified: res.headers.get("last-modified") ?? "",
        size: got,
        title,
        authors,
        locations: had?.locations,
        lastOpened: had?.lastOpened ?? Date.now(),
        statusSeen: had?.statusSeen,
    });
    changed(uuid);
}

/** The uuids of this user's kept books (for the cards' "on this device" mark). */
export async function keptUuids(): Promise<Set<string>> {
    return new Set((await listBooks()).map((b) => b.uuid));
}

/** Every cached book (for the offline page). */
export async function listBooks(): Promise<CachedBook[]> {
    return ((await tx<CachedBook[]>("readonly", (s) => s.getAll())) ?? []).filter((b) => b.user === currentUser());
}

export async function deleteBook(uuid: string): Promise<void> {
    await tx("readwrite", (s) => s.delete(uuid));
    tellWorker({ type: "evict", uuid });
    changed(uuid);
}

/** Remove copies that belong to anyone but this user (sign-out, another user). */
export async function keepOnlyUser(user: string | null): Promise<void> {
    const all = (await tx<CachedBook[]>("readonly", (s) => s.getAll())) ?? [];
    const others = all.filter((b) => b.user !== user);
    for (const b of others) await tx("readwrite", (s) => s.delete(b.uuid));
    if (others.length) tellWorker({ type: "clear-books" });
}

/**
 * Drop books not opened for a week. With statuses (the grid's answer from
 * /api/me/reading), also drop books moved back to "not started", and note
 * the others' statuses for next time. Books set to "keep" stay regardless.
 *
 * Only call this after the server has answered: offline, a kept book may be
 * the only way to read it, however long ago it was opened. keep is never
 * dropped (the book being opened).
 */
export async function sweep(statusOf?: (uuid: string) => string, keep?: string): Promise<void> {
    const now = Date.now();
    for (const b of await listBooks()) {
        if (b.uuid === keep || offlinePref(b.uuid) === "keep") continue;
        if (now - b.lastOpened > MAX_AGE_MS) {
            await deleteBook(b.uuid);
            continue;
        }
        if (!statusOf) continue;
        const status = statusOf(b.uuid);
        if (status && status !== b.statusSeen) await updateBook(b.uuid, { statusSeen: status });
        else if (!status && b.statusSeen) await deleteBook(b.uuid);
    }
}

/** Tell the service worker (sw.ts) to drop cached reader pages. */
function tellWorker(msg: { type: "evict"; uuid: string } | { type: "clear-books" }): void {
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
    navigator.serviceWorker.register("/sw.js").catch((err: unknown) => console.warn("service worker:", err));
}
