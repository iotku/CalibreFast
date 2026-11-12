/**
 * How many cover thumbnails a page may download at once (the library grid,
 * web/app-htmx.ts, and the Discover page, web/discover.ts).
 */

// The grid fetches up to 2 page-JSON files at once (MAX_PAGE_FETCHES in
// app-htmx.ts); covers leave room for them. Covers downloading at once:
const MAX_PAGE_FETCHES = 2;
const COVERS_HTTP1 = 6 - MAX_PAGE_FETCHES; // headroom under HTTP/1.1's 6 connections per host
const COVERS_MULTIPLEXED = 10; // HTTP/2 and 3: one connection, no per-host limit
const COVERS_3G = 4;
const COVERS_2G = 2;

let httpVersion = "";
/** "h2", "h3", "http/1.1"…, or "" while the browser hasn't said. */
function pageProtocol(): string {
    if (httpVersion) return httpVersion;
    // The page itself, else anything it fetched from this server (a page from
    // the browser cache may report "").
    const entries = [
        ...performance.getEntriesByType("navigation"),
        ...performance.getEntriesByType("resource").filter((e) => e.name.startsWith(location.origin)),
    ] as PerformanceResourceTiming[];
    httpVersion = entries.find((e) => e.nextHopProtocol)?.nextHopProtocol ?? "";
    return httpVersion;
}

/**
 * How many covers may download at once.
 *   - HTTP/1.1 (or unknown): the browser opens at most 6 connections per
 *     host, and leaves the page JSON and the book modal waiting behind
 *     anything over that.
 *   - HTTP/2 and 3 multiplex one connection: more at once hides round trips.
 *   - On a slow link (Chromium's estimate) more at once only splits the
 *     bandwidth: fewer, so covers still arrive one by one.
 */
export function maxCovers(): number {
    const effective = (navigator as Navigator & { connection?: { effectiveType?: string } }).connection?.effectiveType;
    if (effective?.endsWith("2g")) return COVERS_2G;
    if (effective === "3g") return COVERS_3G;
    return /^h[23]/.test(pageProtocol()) ? COVERS_MULTIPLEXED : COVERS_HTTP1;
}

