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
    /** The download in progress: title, bytes so far and in all (0: unknown), and when last reported (ms). */
    active?: { title: string; got: number; total: number; at: number } | null;
    /** Books waiting to be downloaded after the active one. */
    waiting?: number;
    /** Waiting for pages to finish loading what's on screen (page-busy.ts). */
    held?: boolean;
}

/** A download not reported for this long was cut off (its page closed). */
export const STALE_MS = 10_000;

export const SYNC_EVENT = "cf:sync";

export function readSync(owner: Owner): SyncState {
    try {
        return (JSON.parse(localStorage.getItem(localKey(owner, "sync")) ?? "null") as SyncState | null) ?? {};
    } catch {
        return {};
    }
}

export function writeSync(owner: Owner, change: Partial<SyncState>): void {
    try {
        localStorage.setItem(localKey(owner, "sync"), JSON.stringify({ ...readSync(owner), ...change }));
    } catch (err) {
        console.warn("sync: couldn't save the download status in localStorage (storage full, or blocked by the browser):", err);
    }
    window.dispatchEvent(new CustomEvent(SYNC_EVENT));
}

/** Call fn when the sync state changes, here or in another tab. */
export function onSyncChange(owner: Owner, fn: () => void): void {
    window.addEventListener(SYNC_EVENT, fn);
    const key = localKey(owner, "sync");
    window.addEventListener("storage", (e) => {
        if (e.key === key) fn();
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
        const m = /^(progress|bookmarks-deleted|bookmarks|annotations)-(.+?)(?:-(?:epub|pdf))?$/.exec(name);
        if (!m) continue;
        let v: unknown;
        try {
            v = JSON.parse(localStorage.getItem(key) ?? "null");
        } catch {
            continue;
        }
        const unsent =
            m[1] === "progress"
                ? !!(v as { pending?: boolean } | null)?.pending
                : m[1] === "bookmarks-deleted"
                  ? Array.isArray(v) && v.length > 0
                  : Array.isArray(v) && v.some((x: { synced?: boolean } | null) => x && !x.synced);
        if (unsent) books.add(m[2]!);
    }
    return books.size;
}
