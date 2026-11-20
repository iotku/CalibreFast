/**
 * Reading activity for the stats page (stats.go, stats.ts): pages turned
 * and time spent reading, per day and book, recorded by the readers.
 *
 *   - Pages: the reader says how many book pages a move went forward
 *     (pagesRead); jumps (contents, search, the scrubber) aren't reading.
 *   - Time: counted a second at a time while the page is visible and the
 *     reader was used (a turn, a key, a tap) in the last few minutes, so a
 *     book left open doesn't count as read.
 *
 * Kept in localStorage first (`activity`, per user): running totals per
 * local day and book, which every tab adds to. The server gets the totals
 * as they are (with this browser's id), so a send repeated after a lost
 * answer changes nothing there. Sent every minute while reading, when the
 * page is hidden, and from any page when the server is back (send-pending.ts).
 * Nothing is kept for guests, or when the account doesn't keep stats.
 */

import { api } from "./dom.js";
import { localKey, type Owner } from "./local-keys.js";
import { accountPrefs } from "./prefs.js";
import { isUnreachable, onServerBack, serverDown } from "./server-watch.js";

/** Without a turn, key or tap for this long, the reader is taken to be away. */
const IDLE_MS = 3 * 60_000;
const SEND_EVERY_MS = 60_000;
/** Seconds read are saved here this often (pages at once). */
const SAVE_EVERY_S = 15;

interface Totals {
    p: number; // pages
    s: number; // seconds
}

/** localStorage `activity`: day -> book uuid -> totals; and whether the server has them all. */
interface Store {
    days: Record<string, Record<string, Totals>>;
    unsent?: boolean;
}

/** The local day (YYYY-MM-DD) of t: the reader's own, not UTC. */
export function localDay(t = new Date()): string {
    const p = (n: number): string => String(n).padStart(2, "0");
    return `${t.getFullYear()}-${p(t.getMonth() + 1)}-${p(t.getDate())}`;
}

function read(key: string): Store {
    try {
        const v = JSON.parse(localStorage.getItem(key) ?? "null") as Store | null;
        return v && typeof v.days === "object" && v.days ? v : { days: {} };
    } catch {
        return { days: {} };
    }
}

function write(key: string, v: Store): void {
    try {
        localStorage.setItem(key, JSON.stringify(v));
    } catch {
        /* storage full or blocked: what wasn't saved is lost, which is only stats */
    }
}

/** This browser's id for the server (stats from each browser add up there). */
function clientId(): string {
    const key = "cf-activity-client";
    try {
        let id = localStorage.getItem(key);
        if (!id || !/^[A-Za-z0-9_-]{1,64}$/.test(id)) {
            id = crypto.randomUUID();
            localStorage.setItem(key, id);
        }
        return id;
    } catch {
        return "unsaved";
    }
}

/** The unsent activity key's name, for send-pending.ts. */
export const ACTIVITY_RECORD = "activity";

let sending: Promise<void> | null = null;

/**
 * Send user's activity if the server doesn't have it all. Days before
 * yesterday are dropped once sent (nothing adds to them any more). Throws
 * when the server can't be reached.
 */
export function sendActivity(user: string, keepalive = false): Promise<void> {
    sending ??= send(user, keepalive).finally(() => (sending = null));
    return sending;
}

async function send(user: string, keepalive: boolean): Promise<void> {
    const key = localKey(user, ACTIVITY_RECORD);
    const store = read(key);
    if (!store.unsent) return;
    const sent = JSON.stringify(store.days);
    const entries = Object.entries(store.days).flatMap(([day, books]) =>
        Object.entries(books).map(([book_uuid, t]) => ({ day, book_uuid, pages: t.p, seconds: t.s })),
    );
    const done = (): void => {
        const now = read(key);
        const yesterday = localDay(new Date(Date.now() - 86_400_000));
        for (const day of Object.keys(now.days)) if (day < yesterday) delete now.days[day];
        // Still unsent if a tab added to it meanwhile.
        write(key, { days: now.days, unsent: JSON.stringify(now.days) !== sent });
    };
    try {
        // The server takes up to 100 at a time; days come oldest first.
        for (let i = 0; i < entries.length; i += 100) {
            await api("PUT", "/api/me/activity", { client_id: clientId(), entries: entries.slice(i, i + 100) }, { keepalive });
        }
        done();
    } catch (e) {
        if (isUnreachable(e)) {
            serverDown();
            throw e;
        }
        const status = (e as { status?: number } | null)?.status;
        if (status === 400) {
            console.warn("activity: the server refused this browser's reading stats; they're dropped:", e);
            done();
        }
        // Otherwise (signed out, a server error): kept for later.
    }
}

export interface ActivityTracker {
    /** The reader moved forward n book pages by reading (not a jump). */
    pagesRead(n: number): void;
    /** The reader was used: time counts for a few minutes more. */
    touch(): void;
}

const noTracker: ActivityTracker = { pagesRead() {}, touch() {} };

/**
 * Records reading of book uuid for user. onGoal: today's pages just
 * reached the account's daily goal; called only when the user asked to be
 * told (goal_notice): by default nothing interrupts reading.
 */
export function createActivityTracker(user: Owner, uuid: string, onGoal?: (pages: number) => void): ActivityTracker {
    if (user === null) return noTracker;
    const prefs = accountPrefs(user);
    if (!prefs.track_reading) return noTracker;
    const key = localKey(user, ACTIVITY_RECORD);
    let lastInput = Date.now();
    // Not yet saved here: added to the store (which other tabs add to) on save.
    let pages = 0;
    let seconds = 0;
    let day = localDay();

    const todayPages = (s: Store): number => Object.values(s.days[localDay()] ?? {}).reduce((n, t) => n + t.p, 0);

    function save(): void {
        if (pages === 0 && seconds === 0) return;
        const s = read(key);
        const t = ((s.days[day] ??= {})[uuid] ??= { p: 0, s: 0 });
        t.p += pages;
        t.s += seconds;
        s.unsent = true;
        write(key, s);
        pages = seconds = 0;
    }

    function sendSoon(keepalive = false): void {
        save();
        sendActivity(user as string, keepalive).catch(() => {}); // unreachable: sent when it's back
    }

    function rollDay(): void {
        const now = localDay();
        if (now !== day) {
            save(); // what was read before midnight stays on its day
            day = now;
        }
    }

    let ticks = 0;
    setInterval(() => {
        rollDay();
        if (document.visibilityState !== "visible" || Date.now() - lastInput > IDLE_MS) return;
        seconds++;
        if (++ticks % SAVE_EVERY_S === 0) save();
    }, 1000);
    setInterval(() => sendSoon(), SEND_EVERY_MS);
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden") sendSoon(true);
        else lastInput = Date.now();
    });
    window.addEventListener("pagehide", () => sendSoon(true));
    onServerBack(() => sendSoon());
    for (const ev of ["keydown", "pointerdown", "wheel", "touchstart"]) {
        window.addEventListener(ev, () => (lastInput = Date.now()), { passive: true });
    }

    return {
        pagesRead(n) {
            if (n <= 0) return;
            lastInput = Date.now();
            rollDay();
            const goal = prefs.goal_notice ? prefs.daily_pages : 0;
            const before = goal > 0 ? todayPages(read(key)) + pages : 0;
            pages += n;
            save();
            if (goal > 0 && before < goal && before + n >= goal) onGoal?.(before + n);
        },
        touch() {
            lastInput = Date.now();
        },
    };
}
