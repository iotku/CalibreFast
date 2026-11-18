/**
 * Whether the pages open in this browser are loading something the reader
 * is waiting for: covers on screen, a book's popup, the next page of the
 * grid, a book opening in the reader. Book downloads in the background
 * (book-sync.ts) hold off while they are, and go on once all is quiet, in
 * this tab or another (a busy page says so in localStorage).
 *
 * A library page watches itself (watchPage); the readers say when they're
 * loading their book (holdBackgroundDownloads).
 */

const KEY = "cf-page-busy";
/** A busy page says so this often; the word lasts BUSY_MS. */
const BEAT_MS = 500;
const BUSY_MS = 1500;
/** Requests and popups that never say they're done stop counting after this long. */
const GIVE_UP_MS = 30_000;

let saidAt = 0;
function sayBusy(): void {
    if (Date.now() - saidAt < BEAT_MS) return;
    saidAt = Date.now();
    try {
        localStorage.setItem(KEY, String(saidAt + BUSY_MS));
    } catch {
        /* storage blocked: this tab's own downloads still see it (busyHere) */
    }
}

/** Another page (or this one) said it's busy, recently. */
function saidBusy(): boolean {
    try {
        return Number(localStorage.getItem(KEY) ?? 0) > Date.now();
    } catch {
        return false;
    }
}

/**
 * Hold background book downloads, in every tab, until release is called
 * (a reader loading its book). Returns release.
 */
export function holdBackgroundDownloads(): () => void {
    sayBusy();
    const beat = setInterval(sayBusy, BEAT_MS);
    const stop = setTimeout(() => clearInterval(beat), 10 * 60_000); // a release never called
    return () => {
        clearInterval(beat);
        clearTimeout(stop);
    };
}

/* ------------------------------------------------------------------ */
/* A library page watching itself                                     */
/* ------------------------------------------------------------------ */

/** Requests in flight (fetch and htmx), with when each started. */
const inFlight = new Map<number, number>();
let nextId = 0;
const begin = (): number => {
    inFlight.set(++nextId, Date.now());
    return nextId;
};
const end = (id: number): void => void inFlight.delete(id);

/** fetch as it was, for the downloads' own requests (they don't make the page busy). */
export let quietFetch: typeof fetch = (...args) => fetch(...args);

/** Covers on screen or about to be (a screen above or below) still loading. */
function coversLoading(): boolean {
    const h = window.innerHeight;
    for (const img of Array.from(document.images)) {
        if (img.complete) continue;
        const r = img.getBoundingClientRect();
        if (r.width && r.bottom > -h && r.top < 2 * h) return true;
    }
    return false;
}

/** This page is loading something the reader waits for (a hidden page isn't: nobody's waiting). */
function busyHere(): boolean {
    if (!watching || document.visibilityState === "hidden") return false;
    const now = Date.now();
    for (const [id, at] of inFlight) if (now - at > GIVE_UP_MS) inFlight.delete(id);
    return document.readyState !== "complete" || inFlight.size > 0 || coversLoading();
}

/** This page or another is busy: background downloads wait. */
export function pageBusy(): boolean {
    return busyHere() || saidBusy();
}

let watching = false;

/**
 * Watch this page's own loading (book-sync.ts calls it on library pages):
 * its fetches and htmx requests (the book popup, the grid's next page) and
 * covers near the screen. While any are going, other tabs are told too.
 */
export function watchPage(): void {
    if (watching) return;
    watching = true;
    const original = window.fetch.bind(window);
    quietFetch = original;
    window.fetch = (...args: Parameters<typeof fetch>): Promise<Response> => {
        const id = begin();
        return original(...args).finally(() => end(id));
    };
    const htmxIds = new WeakMap<EventTarget, number>();
    document.addEventListener("htmx:beforeRequest", (e) => {
        if (e.target) htmxIds.set(e.target, begin());
    });
    document.addEventListener("htmx:afterRequest", (e) => {
        const id = e.target ? htmxIds.get(e.target) : undefined;
        if (id !== undefined) end(id);
    });
    setInterval(() => {
        if (busyHere()) sayBusy();
    }, BEAT_MS);
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** Quiet this long before a download starts or goes on. */
const QUIET_MS = 1000;

/**
 * Resolve once every page has been quiet for QUIET_MS, or stop() says to
 * give up waiting (downloads paused).
 */
export async function untilQuiet(stop: () => boolean = () => false): Promise<void> {
    let since = 0;
    for (;;) {
        if (stop()) return;
        if (pageBusy()) since = 0;
        else if (!since) since = Date.now();
        else if (Date.now() - since >= QUIET_MS) return;
        await sleep(250);
    }
}
