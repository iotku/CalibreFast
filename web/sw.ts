/**
 * The service worker (served at /sw.js, scope /; registered by nav.ts and the
 * EPUB reader, only where browsers allow it: HTTPS or localhost).
 *
 * It lets books kept in this browser (book-cache.ts) be read with no server:
 *   - scripts and styles (/static/…) come from its cache: their URLs change
 *     with their content, so a cached copy is never stale. The current
 *     build's are fetched when it installs (CF_ASSETS, written by the server
 *     in front of this script).
 *   - the readers' pages for a book (EPUB and PDF) are kept once opened, and
 *     used when the server can't be reached. Pages for books no longer kept
 *     are dropped (messages from book-cache.ts).
 *   - files pdf.js fetches when a PDF needs them (fonts, image decoders,
 *     under /static/vendor/) are kept once fetched, like the scripts.
 *   - any other page, when the server can't be reached (or a proxy in front
 *     says it's down: 502-504, as while it restarts), is the offline page
 *     (/offline, web/offline.ts): the Reading list, with the books kept here
 *     to read. It loads the real page once the server is back. That's also
 *     what the installed app (/manifest.webmanifest) opens on with no
 *     connection.
 * Nothing else is cached: API calls go to the network, and the readers keep
 * their own copies of positions, bookmarks and notes.
 */

// Minimal service worker types (the DOM library doesn't have them).
interface ExtendableEvent extends Event {
    waitUntil(p: Promise<unknown>): void;
}
interface FetchEvent extends ExtendableEvent {
    request: Request;
    respondWith(r: Response | Promise<Response>): void;
}
// Chrome 123+: requests matching a route skip this worker altogether.
interface InstallEvent extends ExtendableEvent {
    addRoutes?(rules: { condition: { urlPattern: string }; source: "network" }[]): Promise<void>;
}
interface ExtendableMessageEvent extends ExtendableEvent {
    data: unknown;
}
interface SWScope {
    CF_ASSETS?: string[];
    skipWaiting(): Promise<void>;
    clients: { claim(): Promise<void> };
    registration: { scope: string };
    addEventListener(type: "install", fn: (e: InstallEvent) => void): void;
    addEventListener(type: "activate", fn: (e: ExtendableEvent) => void): void;
    addEventListener(type: "fetch", fn: (e: FetchEvent) => void): void;
    addEventListener(type: "message", fn: (e: ExtendableMessageEvent) => void): void;
}
const sw = self as unknown as SWScope;

const STATIC = "cf-static";
const PAGES = "cf-pages";
const OFFLINE = "/offline";

const assets = (): string[] => sw.CF_ASSETS ?? [];

/**
 * What this worker never handles, sent straight to the browser's own cache
 * and the network. Without these routes every cover on a page wakes the
 * worker first (starting it, if it was idle) only to be passed on, which
 * delays covers the browser already has.
 */
const BYPASS = ["/cover-thumb/*", "/cover/*", "/pages/*", "/api/*"];

sw.addEventListener("install", (e) => {
    if (e.addRoutes) {
        e.waitUntil(
            e.addRoutes(BYPASS.map((p) => ({ condition: { urlPattern: p }, source: "network" }))).catch(() => {
                /* routes not supported: the fetch handler passes them on as before */
            }),
        );
    }
    e.waitUntil(
        (async () => {
            const cache = await caches.open(STATIC);
            const have = new Set((await cache.keys()).map((r) => new URL(r.url).pathname));
            await cache.addAll(assets().filter((a) => !have.has(a)));
            await (await caches.open(PAGES)).add(OFFLINE);
            await sw.skipWaiting();
        })(),
    );
});

sw.addEventListener("activate", (e) => {
    e.waitUntil(
        (async () => {
            await sw.clients.claim();
            // Kept reader pages name the old build's scripts: fetch them again
            // (we're online, having just updated), then drop scripts no page uses.
            const pages = await caches.open(PAGES);
            for (const req of await pages.keys()) {
                if (!READERS.has(new URL(req.url).pathname)) continue;
                try {
                    const res = await fetch(req, { credentials: "same-origin" });
                    if (res.ok && !res.redirected) await pages.put(req, res);
                } catch {
                    /* offline: keep the old page and its scripts */
                    return;
                }
            }
            const keep = new Set(assets());
            // A library's other files (pdf.js's fonts…) stay while its version is in use.
            const vendorDir = (path: string): string | null => /^\/static\/vendor\/[^/]+\//.exec(path)?.[0] ?? null;
            const libs = new Set(assets().map(vendorDir).filter((d) => d !== null));
            const statics = await caches.open(STATIC);
            for (const req of await statics.keys()) {
                const path = new URL(req.url).pathname;
                const dir = vendorDir(path);
                if (!keep.has(path) && !(dir && libs.has(dir))) await statics.delete(req);
            }
        })(),
    );
});

sw.addEventListener("message", (e) => {
    const msg = e.data as { type?: string; uuid?: string; format?: string } | null;
    if (msg?.type === "evict" && msg.uuid) {
        const path = msg.format === "pdf" ? "/read-pdf" : "/read";
        e.waitUntil(caches.open(PAGES).then((c) => c.delete(readerKey(path, msg.uuid ?? ""))));
    } else if (msg?.type === "clear-books") {
        e.waitUntil(
            caches.open(PAGES).then(async (c) => {
                for (const req of await c.keys()) if (READERS.has(new URL(req.url).pathname)) await c.delete(req);
            }),
        );
    }
});

/** The readers' pages: EPUB, PDF. */
const READERS = new Set(["/read", "/read-pdf"]);

/** The cache key of a book's reader page. */
function readerKey(path: string, uuid: string): string {
    return `${path}?uuid=${encodeURIComponent(uuid)}`;
}

sw.addEventListener("fetch", (e) => {
    const req = e.request;
    if (req.method !== "GET") return;
    const url = new URL(req.url);
    if (url.origin !== location.origin) return;

    // epub.js's book root (web/epub-open.ts) is never a real address.
    if (url.pathname.startsWith("/epub-virtual/")) {
        e.respondWith(new Response("", { status: 404 }));
        return;
    }
    if (url.pathname.startsWith("/static/")) {
        e.respondWith(cacheFirst(req));
        return;
    }
    if (req.mode !== "navigate") return;
    // A preview (web/reader-preview.ts) isn't kept: the book isn't either.
    if (READERS.has(url.pathname) && url.searchParams.get("uuid") && !url.searchParams.get("preview")) {
        e.respondWith(readerPage(req, readerKey(url.pathname, url.searchParams.get("uuid") ?? "")));
        return;
    }
    e.respondWith(page(req));
});

/** A gateway's answer for a server that's down (a reverse proxy while CalibreFast restarts). */
const serverDown = (res: Response): boolean => res.status === 502 || res.status === 503 || res.status === 504;

/** Any other page: from the server, else the offline page. */
async function page(req: Request): Promise<Response> {
    try {
        const res = await fetch(req);
        if (!serverDown(res)) return res;
        return (await caches.match(OFFLINE)) ?? res;
    } catch {
        return (await caches.match(OFFLINE)) ?? Response.error();
    }
}

async function cacheFirst(req: Request): Promise<Response> {
    const hit = await caches.match(req);
    if (hit) return hit;
    const res = await fetch(req);
    if (res.ok) {
        const copy = res.clone();
        void caches.open(STATIC).then((c) => c.put(req, copy));
    }
    return res;
}

/** The reader: from the server (kept when it's a book page), else the kept copy. */
async function readerPage(req: Request, key: string): Promise<Response> {
    const kept = async (): Promise<Response | undefined> => (await caches.match(key)) ?? (await caches.match(OFFLINE));
    try {
        const res = await fetch(req);
        if (res.ok && !res.redirected) {
            const copy = res.clone();
            void caches.open(PAGES).then((c) => c.put(key, copy));
        }
        if (serverDown(res)) return (await kept()) ?? res;
        return res;
    } catch {
        return (await kept()) ?? Response.error();
    }
}
