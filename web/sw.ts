/**
 * The service worker (served at /sw.js, scope /; registered by nav.ts and the
 * EPUB reader, only where browsers allow it: HTTPS or localhost).
 *
 * It lets books kept in this browser (book-cache.ts) be read with no server:
 *   - scripts and styles (/static/…) come from its cache: their URLs change
 *     with their content, so a cached copy is never stale. The current
 *     build's are fetched when it installs (CF_ASSETS, written by the server
 *     in front of this script).
 *   - the EPUB reader's page for a book is kept once opened, and used when
 *     the server can't be reached. Pages for books no longer kept are dropped
 *     (messages from book-cache.ts).
 *   - any other page, when the server can't be reached, is the offline page
 *     (/offline), which lists the books kept here.
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
interface ExtendableMessageEvent extends ExtendableEvent {
    data: unknown;
}
interface SWScope {
    CF_ASSETS?: string[];
    skipWaiting(): Promise<void>;
    clients: { claim(): Promise<void> };
    registration: { scope: string };
    addEventListener(type: "install" | "activate", fn: (e: ExtendableEvent) => void): void;
    addEventListener(type: "fetch", fn: (e: FetchEvent) => void): void;
    addEventListener(type: "message", fn: (e: ExtendableMessageEvent) => void): void;
}
const sw = self as unknown as SWScope;

const STATIC = "cf-static";
const PAGES = "cf-pages";
const OFFLINE = "/offline";

const assets = (): string[] => sw.CF_ASSETS ?? [];

sw.addEventListener("install", (e) => {
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
                if (new URL(req.url).pathname !== "/read") continue;
                try {
                    const res = await fetch(req, { credentials: "same-origin" });
                    if (res.ok && !res.redirected) await pages.put(req, res);
                } catch {
                    /* offline: keep the old page and its scripts */
                    return;
                }
            }
            const keep = new Set(assets());
            const statics = await caches.open(STATIC);
            for (const req of await statics.keys()) {
                if (!keep.has(new URL(req.url).pathname)) await statics.delete(req);
            }
        })(),
    );
});

sw.addEventListener("message", (e) => {
    const msg = e.data as { type?: string; uuid?: string } | null;
    if (msg?.type === "evict" && msg.uuid) {
        e.waitUntil(caches.open(PAGES).then((c) => c.delete(readerKey(msg.uuid ?? ""))));
    } else if (msg?.type === "clear-books") {
        e.waitUntil(
            caches.open(PAGES).then(async (c) => {
                for (const req of await c.keys()) if (new URL(req.url).pathname === "/read") await c.delete(req);
            }),
        );
    }
});

/** The cache key of a book's reader page. */
function readerKey(uuid: string): string {
    return `/read?uuid=${encodeURIComponent(uuid)}`;
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
    if (url.pathname === "/read" && url.searchParams.get("uuid") && !url.searchParams.get("preview")) {
        e.respondWith(readerPage(req, readerKey(url.searchParams.get("uuid") ?? "")));
        return;
    }
    e.respondWith(fetch(req).catch(async () => (await caches.match(OFFLINE)) ?? Response.error()));
});

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
    try {
        const res = await fetch(req);
        if (res.ok && !res.redirected) {
            const copy = res.clone();
            void caches.open(PAGES).then((c) => c.put(key, copy));
        }
        return res;
    } catch {
        return (await caches.match(key)) ?? (await caches.match(OFFLINE)) ?? Response.error();
    }
}
