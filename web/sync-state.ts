/**
 * What the top bar's sync button shows (nav.ts), shared between the page
 * that downloads books (book-sync.ts, on library pages) and every page's
 * top bar, in this tab and others: localStorage, per user (local-keys.ts).
 *
 *   - book downloads: the one in progress, how many wait, and whether the
 *     user paused them (for a slow connection);
 *   - reading changes not yet sent to the server (positions, bookmarks,
 *     highlights the readers keep until the server has them).
 *
 * Kept small and with no imports but local-keys.ts: nav.ts is on every page.
 */

import { localKey, type Owner } from "./local-keys.js";

export interface SyncState {
    /** Book downloads paused by the user (this device). */
    paused?: boolean;
    /** The download in progress, bytes so far and in all (0: unknown), and when last reported (ms). */
    active?: { uuid: string; title: string; authors: string; format: string; got: number; total: number; at: number } | null;
    /** The titles of the next few books to download. */
    next?: string[];
    /** Books waiting to be downloaded after the active one. */
    waiting?: number;
    /** Waiting for pages to finish loading what's on screen (page-busy.ts). */
    held?: boolean;
}

/** A download not reported for this long was cut off (its page closed). */
export const STALE_MS = 10_000;

export const SYNC_EVENT = "cf:sync";

/**
 * paused is a key of its own: the downloading tab writes its progress
 * (read, merge, write) every half second, and with paused in the same
 * record, a pause set in another tab in between would be written over.
 */
const pausedKey = (owner: Owner): string => localKey(owner, "sync-paused");

export function readSync(owner: Owner): SyncState {
    let s: SyncState = {};
    try {
        s = (JSON.parse(localStorage.getItem(localKey(owner, "sync")) ?? "null") as SyncState | null) ?? {};
        delete s.paused; // from before it had its own key
        if (localStorage.getItem(pausedKey(owner)) === "1") s.paused = true;
    } catch {
        /* blocked: nothing known */
    }
    return s;
}

export function writeSync(owner: Owner, change: Partial<SyncState>): void {
    const { paused, ...rest } = change;
    try {
        if (paused !== undefined) {
            if (paused) localStorage.setItem(pausedKey(owner), "1");
            else localStorage.removeItem(pausedKey(owner));
        }
        if (Object.keys(rest).length) {
            const { paused: _, ...was } = readSync(owner);
            localStorage.setItem(localKey(owner, "sync"), JSON.stringify({ ...was, ...rest }));
        }
    } catch (err) {
        console.warn("sync: couldn't save the download status in localStorage (storage full, or blocked by the browser):", err);
    }
    window.dispatchEvent(new CustomEvent(SYNC_EVENT));
}

/** Call fn when the sync state changes, here or in another tab. */
export function onSyncChange(owner: Owner, fn: () => void): void {
    window.addEventListener(SYNC_EVENT, fn);
    const keys = [localKey(owner, "sync"), pausedKey(owner)];
    window.addEventListener("storage", (e) => {
        if (e.key !== null && keys.includes(e.key)) fn();
    });
}

/** Books with reading changes the server doesn't have yet (the readers send them when they can). */
export function unsentBooks(owner: string): number {
    const prefix = localKey(owner, "");
    const books = new Set<string>();
    let keys: string[];
    try {
        keys = Object.keys(localStorage).filter((k) => k.startsWith(prefix));
    } catch {
        return 0;
    }
    for (const key of keys) {
        const name = key.slice(prefix.length);
        const m = /^(progress|bookmarks-deleted|bookmarks|annotations|mark)-(.+?)(?:-(?:epub|pdf))?$/.exec(name);
        if (!m) continue;
        let v: unknown;
        try {
            v = JSON.parse(localStorage.getItem(key) ?? "null");
        } catch {
            continue;
        }
        const unsent =
            m[1] === "mark"
                ? v !== null
                : m[1] === "progress"
                ? !!(v as { pending?: boolean } | null)?.pending
                : m[1] === "bookmarks-deleted"
                  ? Array.isArray(v) && v.length > 0
                  : Array.isArray(v) && v.some((x: { synced?: boolean } | null) => x && !x.synced);
        if (unsent) books.add(m[2]!);
    }
    return books.size;
}

/** What of one book's reading the server doesn't have yet (the reader's sync indicator). */
export interface UnsentBook {
    position: boolean;
    bookmarks: number;
    highlights: number;
}

export function unsentFor(owner: string, uuid: string): UnsentBook {
    const read = (name: string): unknown => {
        try {
            return JSON.parse(localStorage.getItem(localKey(owner, name)) ?? "null");
        } catch {
            return null;
        }
    };
    const unsynced = (v: unknown): number => (Array.isArray(v) ? v.filter((x: { synced?: boolean } | null) => x && !x.synced).length : 0);
    const deleted = read(`bookmarks-deleted-${uuid}`);
    return {
        position: ["epub", "pdf"].some((f) => !!(read(`progress-${uuid}-${f}`) as { pending?: boolean } | null)?.pending),
        bookmarks: unsynced(read(`bookmarks-${uuid}`)) + (Array.isArray(deleted) ? deleted.length : 0),
        highlights: unsynced(read(`annotations-${uuid}`)),
    };
}
