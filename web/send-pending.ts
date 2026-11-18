/**
 * Reading changes the server doesn't have yet, sent from any page (nav.ts),
 * not only from that book's reader: positions, bookmarks (and bookmark
 * deletions) and highlights the readers kept here while the server couldn't
 * be reached (reading-sync.ts, annotations.ts). Sent as soon as a page
 * opens, and again the moment the server answers after being down
 * (server-watch.ts). Book downloads are another matter: they wait for quiet
 * pages (book-sync.ts); these never wait.
 *
 * Works on the readers' own records in localStorage, one item at a time,
 * reading each again just before writing it back, so a reader open on the
 * same book in another tab loses nothing (its sends are the same, by id).
 */

import { api } from "./dom.js";
import { localKey } from "./local-keys.js";
import { isUnreachable, onServerBack, serverDown } from "./server-watch.js";

function read<T>(key: string): T | null {
    try {
        return JSON.parse(localStorage.getItem(key) ?? "null") as T | null;
    } catch {
        return null;
    }
}

function write(key: string, v: unknown): void {
    try {
        localStorage.setItem(key, JSON.stringify(v));
    } catch (err) {
        console.warn(`send-pending: couldn't note in localStorage that ${key} reached the server (storage full, or blocked); it's sent again later:`, err);
    }
}

/** The server isn't there: stop for now (server-watch.ts says when it's back). */
class Unreachable extends Error {}

/** Send one request; false if the server refused it for good (it won't take it). */
async function send(method: string, url: string, body?: unknown): Promise<unknown> {
    try {
        return await api(method, url, body);
    } catch (e) {
        if (isUnreachable(e)) {
            serverDown();
            throw new Unreachable();
        }
        throw e;
    }
}

const status = (e: unknown): number | undefined => (e as { status?: number } | null)?.status;

/**
 * The server won't ever take it: the book is gone from the library (404),
 * or it refuses what was sent (400, 409). Retrying would leave it "waiting
 * to be sent" for good, so it's given up on, as annotations.ts does.
 */
export const refusedForGood = (e: unknown): boolean => [400, 404, 409].includes(status(e) ?? 0);

function gaveUp(what: string, e: unknown): void {
    console.warn(`send-pending: the server refused ${what} for good (${status(e)}); it's no longer kept as waiting:`, e);
}

interface Progress {
    position: string;
    percent: number;
    updatedAt: number;
    pending?: boolean;
}
interface Item {
    id: string;
    updated_at?: number;
    deleted?: boolean;
    synced?: boolean;
}

async function sendProgress(key: string, uuid: string, format: string): Promise<void> {
    const p = read<Progress>(key);
    if (!p?.pending) return;
    try {
        await send("PUT", `/api/me/progress/${encodeURIComponent(uuid)}`, { format, position: p.position, percent: p.percent, updated_at: p.updatedAt });
    } catch (e) {
        if (e instanceof Unreachable || !refusedForGood(e)) throw e;
        gaveUp(`the place in ${uuid}`, e);
    }
    const now = read<Progress>(key);
    if (now && now.updatedAt === p.updatedAt) write(key, { ...now, pending: false });
}

async function sendDeletions(key: string, uuid: string): Promise<void> {
    for (const id of read<string[]>(key) ?? []) {
        try {
            await send("DELETE", `/api/me/bookmarks/${encodeURIComponent(uuid)}/${encodeURIComponent(id)}`);
        } catch (e) {
            if (e instanceof Unreachable || status(e) !== 404) throw e; // 404: already gone
        }
        write(key, (read<string[]>(key) ?? []).filter((d) => d !== id));
    }
}

async function sendBookmarks(key: string, uuid: string): Promise<void> {
    for (const b of (read<Item[]>(key) ?? []).filter((x) => !x.synced)) {
        try {
            await send("PUT", `/api/me/bookmarks/${encodeURIComponent(uuid)}/${encodeURIComponent(b.id)}`, { ...b, synced: undefined });
        } catch (e) {
            if (e instanceof Unreachable || !refusedForGood(e)) throw e;
            gaveUp(`a bookmark in ${uuid}`, e);
        }
        write(key, (read<Item[]>(key) ?? []).map((x) => (x.id === b.id ? { ...x, synced: true } : x)));
    }
}

async function sendAnnotations(key: string, uuid: string): Promise<void> {
    const unsent = (read<Item[]>(key) ?? []).filter((x) => !x.synced).sort((x, y) => (x.updated_at ?? 0) - (y.updated_at ?? 0));
    for (const a of unsent) {
        let stored: Item | null = null;
        let refused = false;
        try {
            stored = (await send("PUT", `/api/me/annotations/${encodeURIComponent(uuid)}/${encodeURIComponent(a.id)}`, { ...a, synced: undefined })) as Item;
        } catch (e) {
            // As annotations.ts: refused for good isn't retried forever.
            if (e instanceof Unreachable || ![400, 404, 409].includes(status(e) ?? 0)) throw e;
            refused = true;
        }
        const all = (read<Item[]>(key) ?? [])
            .map((x) => {
                if (x.id !== a.id || x.updated_at !== a.updated_at) return x; // changed again meanwhile: next time
                return refused ? { ...x, synced: true } : { ...x, ...stored, deleted: !!stored?.deleted, synced: true };
            })
            .filter((x) => !(x.deleted && x.synced)); // a delete the server has needs no copy here
        write(key, all);
    }
}

const RECORD = /^(progress|bookmarks-deleted|bookmarks|annotations)-(.+?)(?:-(epub|pdf))?$/;

let sending: Promise<void> | null = null;

/** Send everything of user's that waits, until the server stops answering. */
export function sendPending(user: string): Promise<void> {
    sending ??= run(user).finally(() => {
        sending = null;
        window.dispatchEvent(new CustomEvent("cf:sync")); // the sync button counts what's left
    });
    return sending;
}

async function run(user: string): Promise<void> {
    const prefix = localKey(user, "");
    let keys: string[];
    try {
        keys = Object.keys(localStorage).filter((k) => k.startsWith(prefix));
    } catch {
        return;
    }
    for (const key of keys) {
        const m = RECORD.exec(key.slice(prefix.length));
        if (!m) continue;
        const [, kind, uuid, format] = m;
        try {
            if (kind === "progress" && format) await sendProgress(key, uuid!, format);
            else if (kind === "bookmarks-deleted") await sendDeletions(key, uuid!);
            else if (kind === "bookmarks") await sendBookmarks(key, uuid!);
            else if (kind === "annotations") await sendAnnotations(key, uuid!);
        } catch (e) {
            if (e instanceof Unreachable) return; // the rest when the server is back
            console.warn(`send-pending: the server didn't take ${key.slice(prefix.length)}; it's tried again later:`, e);
        }
    }
}

let watching = "";

/** A signed-in page (nav.ts): send what waits now, and whenever the server is back. */
export function startSendingPending(user: string): void {
    void sendPending(user);
    if (watching === user) return;
    watching = user;
    onServerBack(() => void sendPending(watching));
}
