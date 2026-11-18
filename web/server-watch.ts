/**
 * Notices when the server can be reached again, for the readers' sync
 * (reading-sync.ts, annotations.ts).
 *
 * The browser's "online" event only covers this device's own network: a
 * reader opened while the server was down (or restarting) is "online" the
 * whole time and would never hear that the server is back. So once a sync
 * fails for want of the server, this asks /api/me now and then (backing off,
 * and only while the page is shown) and tells whoever is waiting when it
 * answers.
 */

// Asked often: what waits to be sent goes as soon as the server is back.
const FIRST_DELAY = 3_000;
const MAX_DELAY = 15_000;

let down = false;
let delay = FIRST_DELAY;
let timer: ReturnType<typeof setTimeout> | undefined;
let probing = false;
const waiting = new Set<() => void>();

/** Whether an error from api() (dom.ts) or fetch means the server wasn't there. */
export function isUnreachable(e: unknown): boolean {
    const status = (e as { status?: number } | null)?.status;
    // No status: the request never got an answer. 502–504: a proxy in front
    // of a server that isn't answering.
    return status === undefined || status === 502 || status === 503 || status === 504;
}

/** Whether a request failed for want of the server, and it hasn't answered since. */
export function isServerDown(): boolean {
    return down;
}

/** Call fn each time the server answers again after serverDown(). */
export function onServerBack(fn: () => void): void {
    waiting.add(fn);
}

/** A request just failed for want of the server: start asking until it answers. */
export function serverDown(): void {
    if (down) return;
    down = true;
    delay = FIRST_DELAY;
    schedule();
}

function schedule(): void {
    clearTimeout(timer);
    if (!down || document.visibilityState === "hidden") return; // visibilitychange picks it up again
    timer = setTimeout(() => void probe(), delay);
}

async function probe(): Promise<void> {
    if (!down || probing) return;
    probing = true;
    clearTimeout(timer);
    let ok = false;
    try {
        const res = await fetch("/api/me", { credentials: "same-origin", cache: "no-store" });
        // Any answer of the server's own counts, a 401 included (signed out
        // meanwhile): what failed for that reason won't fail for want of it.
        ok = !isUnreachable({ status: res.status });
    } catch {
        /* still down */
    }
    probing = false;
    if (!ok) {
        delay = Math.min(delay * 2, MAX_DELAY);
        schedule();
        return;
    }
    down = false;
    for (const fn of waiting) {
        try {
            fn();
        } catch {
            /* one listener's trouble isn't the others' */
        }
    }
}

// Coming back to the page, or the device's network coming back: ask at once.
document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && down) void probe();
    else schedule();
});
window.addEventListener("online", () => {
    if (down) {
        delay = FIRST_DELAY;
        void probe();
    }
});
