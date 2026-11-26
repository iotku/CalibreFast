// Online lookups paced to each source's rate limits (Go: lookup.go). Open
// Library allows a burst of requests and then one a second, and one lookup
// takes several. A lookup started without room comes back cut short, so
// lookups here first ask how long until there's room for a whole one
// (/metadata/lookup/room), count that down for the page to show, then ask
// with ?wait=1, which has the server hold the lookup for any room someone
// else took meanwhile. Used by the metadata editor (isbn-tools.ts, the cover
// dialog) and the cover updater (library-covers.ts).

import { api, el } from "./dom.js";
import type { LookupResult } from "./types.js";

/** When a source can take a lookup (Go: lookupRoom in lookup.go). */
interface LookupRoom {
    wait_ms: number;
    day_used_up?: boolean;
    message?: string;
}

/**
 * Told the seconds left while waiting for a source's rate limit, each
 * second, and 0 once the wait is over.
 */
export type OnWait = (secondsLeft: number) => void;

/** Waits longer than this aren't waited out here: the lookup says when to try again. */
const MAX_WAIT_MS = 10 * 60_000;
/** Ask again at most this many times when someone else took the room meanwhile. */
const MAX_TRIES = 4;

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** The wait a source's "try again in 1m5s" asks for, in ms; null if it isn't one. */
export function retryAfter(message: string): number | null {
    const m = /try again in ((?:\d+h)?(?:\d+m)?(?:\d+(?:\.\d+)?s)?)/.exec(message);
    if (!m?.[1]) return null;
    let ms = 0;
    for (const [, n, unit] of m[1].matchAll(/(\d+(?:\.\d+)?)([hms])/g)) {
        ms += Number(n) * (unit === "h" ? 3_600_000 : unit === "m" ? 60_000 : 1000);
    }
    return ms || null;
}

/** Counts ms down through onWait, a second at a time; false if stopped. */
async function countDown(ms: number, onWait: OnWait, stopped: () => boolean): Promise<boolean> {
    const until = Date.now() + ms;
    for (let left = until - Date.now(); left > 0; left = until - Date.now()) {
        if (stopped()) return false;
        onWait(Math.ceil(left / 1000));
        await sleep(Math.min(1000, left));
    }
    onWait(0);
    return true;
}

/**
 * Waits until source has room for a lookup (or a cover), saying so through
 * onWait. Returns null when there's room, else why not: today's requests are
 * used up, the wait is too long, or it was stopped.
 */
export async function waitForRoom(source: string, what: "lookup" | "cover", onWait: OnWait, stopped: () => boolean = () => false): Promise<string | null> {
    const room = await api<LookupRoom>("GET", `/metadata/lookup/room?${new URLSearchParams({ source, what })}`);
    if (room.day_used_up) return room.message || "today's requests are used up; try again tomorrow";
    if (room.wait_ms <= 0) return null;
    if (room.wait_ms > MAX_WAIT_MS) return `too many requests just now; try again in ${Math.ceil(room.wait_ms / 60_000)}m`;
    return (await countDown(room.wait_ms, onWait, stopped)) ? null : "stopped";
}

/**
 * /metadata/lookup, paced: waits for room first (onWait counts it down),
 * and waits again when someone else took the room meanwhile. A source that
 * can't take it today answers with an error, as a lookup would.
 */
export async function pacedLookup(
    source: string,
    params: Record<string, string>,
    onWait: OnWait,
    stopped: () => boolean = () => false,
): Promise<LookupResult> {
    let res: LookupResult = { source, error: "stopped" };
    for (let attempt = 0; attempt < MAX_TRIES; attempt++) {
        const why = await waitForRoom(source, "lookup", onWait, stopped);
        if (why) return { source, error: why };
        res = await api<LookupResult>("GET", `/metadata/lookup?${new URLSearchParams({ source, ...params, wait: "1" })}`);
        const wait = res.error ? retryAfter(res.error) : null;
        if (wait === null || wait > MAX_WAIT_MS) return res;
        // Someone else's lookups took the room: wait it out and ask again.
        if (!(await countDown(wait, onWait, stopped))) return { source, error: "stopped" };
    }
    return res;
}

/**
 * An OnWait that tells the user in a status line: "Waiting for Open
 * Library's rate limit…" once (a screen reader hears it once), with the
 * seconds counting down beside it for the eye only. When the wait is over
 * the line says again what it said before.
 */
export function waitNotice(say: (text: string) => void, status: HTMLElement, name: string): OnWait {
    let before = "";
    let count: HTMLElement | null = null;
    return (secondsLeft) => {
        if (secondsLeft > 0) {
            if (!count?.isConnected) {
                before = status.textContent ?? "";
                say(`Waiting for ${name}'s rate limit…`);
                count = el("span", { className: "lookup-wait-count" });
                count.setAttribute("aria-hidden", "true");
                status.append(count);
            }
            count.textContent = ` ${secondsLeft} s`;
        } else if (count) {
            count = null;
            say(before);
        }
    };
}
