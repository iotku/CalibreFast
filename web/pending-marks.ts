/**
 * Favorite and reading-status changes the server doesn't have yet.
 *
 * The book popup's controls (user.ts) and the cover buttons (reading.ts)
 * show a change at once and note it here, per user and per book
 * (localStorage `cf-u/<user>/mark-<uuid>`); send-pending.ts sends it, now
 * or once the server can be reached again (server-watch.ts). So a slow or
 * missing connection never undoes a click, and what waits survives a
 * reload.
 *
 * Each record keeps what the server had before the first change ("was"):
 * a change back to it (favorite, then unfavorite) drops that field instead
 * of sending two requests that cancel out. Marks fetched from the server
 * (reading.ts) and controls it renders get what waits laid over them, so
 * nothing jumps back while it's on its way.
 */

import { localKey } from "./local-keys.js";
import type { BookChange, Marks } from "./reading.js";

export interface MarkFields {
    favorite?: boolean;
    status?: string;
}

/** A book's changes waiting to be sent, and the server's values before them. */
export interface PendingMark extends MarkFields {
    was: MarkFields;
}

const PREFIX = "mark-";
const FIELDS = ["favorite", "status"] as const;

function read(key: string): PendingMark | null {
    try {
        return JSON.parse(localStorage.getItem(key) ?? "null") as PendingMark | null;
    } catch {
        return null;
    }
}

function write(key: string, p: PendingMark | null): void {
    try {
        if (p && FIELDS.some((f) => p[f] !== undefined)) localStorage.setItem(key, JSON.stringify(p));
        else localStorage.removeItem(key);
    } catch (err) {
        console.warn("pending-marks: couldn't keep a favorite or status change in localStorage (storage full, or blocked); it's sent only if the server answers now:", err);
    }
}

/** The changes waiting for one book of user's. */
export function pendingMark(user: string | null, uuid: string): PendingMark | null {
    return user ? read(localKey(user, PREFIX + uuid)) : null;
}

/** Every book of user's with changes waiting. */
export function pendingMarks(user: string | null): Map<string, PendingMark> {
    const out = new Map<string, PendingMark>();
    if (!user) return out;
    const prefix = localKey(user, PREFIX);
    let keys: string[];
    try {
        keys = Object.keys(localStorage).filter((k) => k.startsWith(prefix));
    } catch {
        return out;
    }
    for (const k of keys) {
        const p = read(k);
        if (p) out.set(k.slice(prefix.length), p);
    }
    return out;
}

/**
 * Note a change made here. was: what was showing before it, which is what
 * the server has when nothing waits for that field yet. A change back to
 * the server's value drops the field: nothing to send.
 */
export function noteMark(user: string, uuid: string, change: MarkFields, was: MarkFields): void {
    const key = localKey(user, PREFIX + uuid);
    const p: PendingMark = read(key) ?? { was: {} };
    for (const f of FIELDS) {
        const v = change[f];
        if (v === undefined) continue;
        if (p[f] === undefined) (p.was as Record<string, unknown>)[f] = was[f];
        (p as unknown as Record<string, unknown>)[f] = v;
        if (p[f] === p.was[f]) {
            delete p[f];
            delete p.was[f];
        }
    }
    write(key, p);
    window.dispatchEvent(new CustomEvent("cf:sync")); // the sync button counts what's left
}

/** Lay what waits over marks from the server (in place). */
export function overlayPending(m: Marks): Marks {
    for (const [uuid, p] of pendingMarks(m.user)) {
        if (p.favorite !== undefined) {
            if (p.favorite) m.favorites.add(uuid);
            else m.favorites.delete(uuid);
        }
        if (p.status === undefined) continue;
        m.wanted.delete(uuid);
        m.finished.delete(uuid);
        if (p.status !== "reading") {
            m.reading.delete(uuid);
            m.resume.delete(uuid);
        } else if (!m.reading.has(uuid)) m.reading.set(uuid, null);
        if (p.status === "wanted") m.wanted.add(uuid);
        else if (p.status === "finished") m.finished.add(uuid);
    }
    return m;
}

/**
 * Send one book's waiting changes (send-pending.ts calls it with its own
 * send, which stops the run when the server isn't there). A field changed
 * again while its request was out stays, for the next run. Each change the
 * server takes is told to the page ("cf:status", as the popup's controls
 * always have), for the grid and Discover to apply: only then, as their
 * rows come from the server.
 */
export async function sendMark(
    user: string,
    uuid: string,
    send: (method: string, url: string, body: unknown) => Promise<unknown>,
): Promise<void> {
    const key = localKey(user, PREFIX + uuid);
    const p = read(key);
    if (!p) return;
    const path = `/api/me/books/${encodeURIComponent(uuid)}`;
    const done = (f: (typeof FIELDS)[number], v: unknown): void => {
        const now = read(key);
        if (!now || now[f] !== v) return;
        delete now[f];
        delete now.was[f];
        write(key, now);
    };
    const told = (change: BookChange): void => void document.dispatchEvent(new CustomEvent<BookChange>("cf:status", { detail: change }));
    if (p.favorite !== undefined) {
        await send("PUT", `${path}/favorite`, { favorite: p.favorite });
        done("favorite", p.favorite);
        told({ uuid, favorite: p.favorite });
    }
    if (p.status !== undefined) {
        const res = (await send("PUT", `${path}/status`, { status: p.status })) as { status?: string } | null;
        done("status", p.status);
        told({ uuid, status: res?.status ?? p.status });
    }
}

/** A change the server refused for good (book gone): drop it. */
export function dropMark(user: string, uuid: string): void {
    write(localKey(user, PREFIX + uuid), null);
}
