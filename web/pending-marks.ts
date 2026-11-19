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
 * Changes to one field coalesce: favorite, unfavorite, favorite sends one
 * request, the last. Each keeps when it was made, sent as how long ago
 * (age_ms, so the devices' clocks needn't agree): the server keeps the
 * newest change (mark_changes, userdata.go), so one made earlier offline
 * doesn't undo a later one made elsewhere meanwhile. That's also why a
 * change back to what the server had is still sent: the server may have
 * changed since. Marks fetched from the server (reading.ts) and controls it
 * renders get what waits laid over them, so nothing jumps back while it's
 * on its way; the server's answer, applied or not, is what then shows.
 */

import { localKey } from "./local-keys.js";
import type { BookChange, Marks } from "./reading.js";

export interface MarkFields {
    favorite?: boolean;
    status?: string;
}

/** A book's changes waiting to be sent, and when each was made (Date.now()). */
export interface PendingMark extends MarkFields {
    at: { favorite?: number; status?: number };
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

/** Note a change made here, now; it replaces one to the same field still waiting. */
export function noteMark(user: string, uuid: string, change: MarkFields): void {
    const key = localKey(user, PREFIX + uuid);
    const p: PendingMark = read(key) ?? { at: {} };
    p.at ??= {}; // a record from before change times were kept
    const now = Date.now();
    for (const f of FIELDS) {
        const v = change[f];
        if (v === undefined) continue;
        (p as unknown as Record<string, unknown>)[f] = v;
        p.at[f] = now;
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
    // Done with a field, unless it changed again while its request was out.
    const done = (f: (typeof FIELDS)[number], at: number | undefined): void => {
        const now = read(key);
        if (!now || now.at?.[f] !== at) return;
        delete now[f];
        delete now.at[f];
        write(key, now);
    };
    const age = (at: number | undefined): number => Math.max(0, Date.now() - (at ?? Date.now()));
    const told = (change: BookChange): void => void document.dispatchEvent(new CustomEvent<BookChange>("cf:status", { detail: change }));
    // The answer is the field as it stands: this change, or a later one made elsewhere.
    if (p.favorite !== undefined) {
        const at = p.at?.favorite;
        const res = (await send("PUT", `${path}/favorite`, { favorite: p.favorite, age_ms: age(at) })) as { favorite?: boolean } | null;
        done("favorite", at);
        told({ uuid, favorite: res?.favorite ?? p.favorite });
    }
    if (p.status !== undefined) {
        const at = p.at?.status;
        const res = (await send("PUT", `${path}/status`, { status: p.status, age_ms: age(at) })) as { status?: string } | null;
        done("status", at);
        told({ uuid, status: res?.status ?? p.status });
    }
}

/** A change the server refused for good (book gone): drop it. */
export function dropMark(user: string, uuid: string): void {
    write(localKey(user, PREFIX + uuid), null);
}
