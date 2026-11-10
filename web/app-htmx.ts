/**
 * CalibreFast library grid: the main library, filtered views and search.
 *
 * The books are one continuous grid, addressed by index. Pages (the
 * server's data-page-size books per JSON request) are only how the data
 * arrives, and the number the page box shows.
 *
 * Every row is the same height (cards have a fixed aspect ratio), so where
 * any book sits is arithmetic. Only the rows around the viewport are in the
 * DOM, between two spacers that stand in for the rest. The spacers are
 * painted as empty cards (a CSS mask), so a scroll that outruns the DOM
 * still shows the grid, never a blank. Books whose page hasn't arrived yet
 * are skeleton cards, filled in place when it does. Nothing on screen ever
 * moves, so there's no scroll compensation.
 *
 * Configured on #books:
 *   data-page-url    URL template, "{n}" -> page number
 *   data-page-size   books per page
 *   data-total-books
 *   data-empty-text
 * Page nav controls (#page-input, #prev-page, #next-page) are optional.
 *
 * The URL keeps ?page=N, the page at the top of the screen; history.state
 * keeps the exact book, so a reload or back/forward returns to the same row.
 */

import { byId, maybeId } from "./dom.js";
import type { Book } from "./types.js";
import { coverUrl, renderBookCard } from "./utils.js";
import { setupBookClickHandlers, loadBook, HASH_PREFIX } from "./modal.js";
import { cachedReading, fetchReading, markCard, type Reading } from "./reading.js";

const RENDER_AHEAD = 1; // screens of rows in the DOM past the viewport, in the scroll direction
const RENDER_BEHIND = 0.5; // ...and the other way
const FETCH_AHEAD = 2; // screens of book data fetched ahead, at least
const FETCH_LEAD = 2; // ...and at speed, how many fetch-times of scrolling ahead
const MAX_PAGE_FETCHES = 2; // concurrent page-JSON requests
const RETRY_FAILED_MS = 5000; // a page that failed isn't asked for again sooner
const READING_REFRESH_MS = 15_000; // coming back to the tab re-checks reading progress, at most this often
const HISTORY_DEBOUNCE_MS = 300; // browsers throttle rapid history updates
const COVERS_WAIT_SCREENS_PER_SEC = 4; // faster than this, covers wait until it slows
const SETTLE_MS = 120; // no scrolling for this long: stopped
// Covers downloading at once (see maxCovers):
const COVERS_HTTP1 = 6 - MAX_PAGE_FETCHES; // headroom under HTTP/1.1's 6 connections per host
const COVERS_MULTIPLEXED = 10; // HTTP/2 and 3: one connection, no per-host limit
const COVERS_3G = 4;
const COVERS_2G = 2;

const SKELETON = `<div class="book skeleton" aria-hidden="true"></div>`;
// A page that came back shorter than the count said (rewritten since).
const MISSING = `<div class="book missing" aria-hidden="true"></div>`;

interface PageLoad {
    promise: Promise<Book[] | null>;
    ctrl: AbortController;
}

/** A place in the grid: a book, and how far (in rows) the screen's top is past its row's top. */
interface Anchor {
    index: number;
    frac: number;
}

const coversInflight = new Set<HTMLImageElement>();
const loadedCovers = new Set<string>(); // cover URLs the browser has: shown at once when re-rendered
const pageCache = new Map<number, Book[]>(); // page -> books
const loading = new Map<number, PageLoad>(); // page -> in-flight fetch
const failedAt = new Map<number, number>(); // page -> when its fetch last failed
const cards = new Map<number, HTMLElement>(); // book index -> its card in the DOM
let reading: Reading = cachedReading(); // books the user is reading -> how far (reading.ts)
let readingFetchedAt = 0;

// Elements. #books is required; the page-nav controls are optional.
const booksEl = byId("books");
const pageInputEl = maybeId("page-input", HTMLInputElement);
const prevBtn = maybeId("prev-page", HTMLButtonElement);
const nextBtn = maybeId("next-page", HTMLButtonElement);
const totalPagesEl = maybeId("total-pages");
const badgeEl = maybeId("scroll-badge"); // in index.html
const topSpacer = makeSpacer();
const bottomSpacer = makeSpacer();

const cfg = booksEl.dataset;
const total = Math.max(0, parseInt(cfg["totalBooks"] ?? "", 10) || 0);
const pageSize = Math.max(1, parseInt(cfg["pageSize"] ?? "", 10) || 60);
const totalPages = Math.ceil(total / pageSize);
const pageUrl = (n: number): string => (cfg["pageUrl"] ?? "").replace("{n}", String(n));
const pageOf = (index: number): number => Math.floor(index / pageSize) + 1;

// Geometry, from the layout (measure).
let cols = 1;
let rows = 0;
let rowGap = 0;
let rowH = 0; // card height + row gap: row r starts at r * rowH

let first = 0; // books with a card in the DOM: [first, last)
let last = 0;
let anchor: Anchor = { index: 0, frac: 0 }; // where the user is; kept across relayouts
let targetPage = 1; // page the UI says we're on
let lastLabelPage: number | null = null;
let scrollDir = 1; // 1 = down, -1 = up; biases what's rendered and fetched
let speed = 0; // rows per second the user is scrolling, 0 when stopped
let fetchSecs = 0.1; // how long a page fetch takes (moving average)
let lastY = 0; // scrollY the scroll handler last saw
let lastWidth = 0;
let keyScrolling = false;
let draggingScrollbar = false; // mouse held on the scrollbar, or a touch is active
let badgeTimer: ReturnType<typeof setTimeout> | undefined;
let historyTimer: ReturnType<typeof setTimeout> | undefined;
let historyPending = false; // the anchor moved since the URL was written

const headerHeight = (): number => document.querySelector<HTMLElement>(".top-bar")?.offsetHeight ?? 0;
const clamp = (n: number, lo: number, hi: number): number => Math.max(lo, Math.min(n, hi));

/** px of the grid above the screen (under the top bar or scrolled off): row r is at the top when this is r * rowH. */
const viewTop = (): number => headerHeight() - booksEl.getBoundingClientRect().top;

/** Rows on screen, counting partly visible ones. */
const screenRows = (): number => Math.ceil((window.innerHeight - headerHeight()) / rowH) + 1;

function setStatus(text: string): void {
    const el = maybeId("status");
    if (el) el.textContent = text;
}

function clampPage(n: number): number {
    return clamp(n, 1, Math.max(1, totalPages));
}

/** Floating "page / total" label that rides alongside the scrollbar thumb. */
function showScrollBadge(page: number): void {
    if (!badgeEl) return;
    const root = document.documentElement;
    const header = headerHeight();
    const ratio = window.scrollY / Math.max(1, root.scrollHeight - window.innerHeight);
    const room = window.innerHeight - header - badgeEl.offsetHeight - 8;

    badgeEl.textContent = `${page} / ${totalPages}`;
    badgeEl.style.top = `${header + 4 + ratio * Math.max(0, room)}px`;
    badgeEl.classList.add("visible");

    clearTimeout(badgeTimer);
    badgeTimer = setTimeout(() => badgeEl.classList.remove("visible"), 700);
}

/* ------------------------------------------------------------------ */
/* Init                                                               */
/* ------------------------------------------------------------------ */

window.addEventListener("DOMContentLoaded", () => {
    if (totalPagesEl) totalPagesEl.textContent = String(totalPages);
    if (pageInputEl) pageInputEl.max = String(totalPages); // arrow keys clamp natively
    updateNavigationButtons();
    if (!total) return setStatus(cfg["emptyText"] ?? "No books found.");

    // Make the hash-less URL the base history entry.
    const initialHash = window.location.hash;
    if (initialHash) {
        history.replaceState(history.state, "", window.location.pathname + window.location.search);
    }
    window.history.scrollRestoration = "manual";

    // Nothing above the screen ever changes height, so there's nothing for
    // the browser's scroll anchoring to do but fight the spacers.
    booksEl.style.overflowAnchor = "none";
    setupCoverStates(booksEl);
    booksEl.replaceChildren(topSpacer, bottomSpacer);
    lastWidth = booksEl.getBoundingClientRect().width;
    measure();
    jumpTo(startAnchor());

    // Re-add the hash as a new entry on top, then open the modal.
    if (initialHash.startsWith(HASH_PREFIX)) {
        const url = new URL(window.location.href);
        url.hash = initialHash;
        history.pushState({ ...history.state, modal: true }, "", url);
        loadBook(initialHash.substring(HASH_PREFIX.length));
    }

    setupScrollTracking();
    setupReading();
});

/* ------------------------------------------------------------------ */
/* Reading progress                                                   */
/* ------------------------------------------------------------------ */

function markReading(el: HTMLElement): void {
    const uuid = el.dataset["uuid"];
    if (uuid) markCard(el, reading.get(uuid));
}

async function refreshReading(): Promise<void> {
    readingFetchedAt = Date.now();
    const fresh = await fetchReading();
    if (!fresh) return;
    reading = fresh;
    for (const el of cards.values()) markReading(el);
}

/** Fetch now; again when the user comes back (from the reader, another tab) or changes a status in the popup. */
function setupReading(): void {
    void refreshReading();
    window.addEventListener("pageshow", (e) => {
        if (e.persisted) void refreshReading();
    });
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "visible" && Date.now() - readingFetchedAt > READING_REFRESH_MS) void refreshReading();
    });
    document.addEventListener("cf:status", () => void refreshReading());
}

/**
 * Where to start: the book history.state remembers if it's from this URL's
 * page (a reload, back/forward), else the top of ?page.
 */
function startAnchor(): Anchor {
    const urlPage = parseInt(new URLSearchParams(window.location.search).get("page") ?? "1", 10);
    const page = clampPage(Number.isFinite(urlPage) ? urlPage : 1);
    const state = history.state as { page?: unknown; at?: unknown; frac?: unknown } | null;
    if (
        state?.page === page &&
        typeof state.at === "number" &&
        Number.isInteger(state.at) &&
        state.at >= 0 &&
        state.at < total
    ) {
        const frac = typeof state.frac === "number" ? clamp(state.frac, -1, 1) : 0;
        return { index: state.at, frac };
    }
    return { index: (page - 1) * pageSize, frac: 0 };
}

export function setupCoverStates(container: HTMLElement): void {
    container.addEventListener(
        "load",
        (e) => {
            if (e.target instanceof HTMLImageElement && e.target.matches(".book-cover"))
                e.target.classList.add("loaded");
        },
        true,
    );

    container.addEventListener(
        "error",
        (e) => {
            const img = e.target;
            if (!(img instanceof HTMLImageElement) || !img.matches(".book-cover")) return;
            img.parentElement?.classList.add("no-cover"); // title/author info stays visible
            img.remove();
        },
        true,
    );
}

/* ------------------------------------------------------------------ */
/* Layout                                                             */
/* ------------------------------------------------------------------ */

function makeSpacer(): HTMLDivElement {
    const el = document.createElement("div");
    el.className = "grid-spacer";
    el.setAttribute("aria-hidden", "true");
    el.hidden = true;
    return el;
}

/**
 * Columns and row pitch, read from the layout so the arithmetic matches it
 * to the subpixel, and the spacers' empty-card pattern to go with them.
 */
function measure(): void {
    const cs = getComputedStyle(booksEl);
    const tracks = cs.gridTemplateColumns
        .split(" ")
        .map(parseFloat)
        .filter((w) => w > 0);
    cols = Math.max(1, tracks.length);
    rows = Math.ceil(total / cols);
    rowGap = parseFloat(cs.rowGap) || 0;
    const colGap = parseFloat(cs.columnGap) || 0;

    // Any card will do: they're all the same size. Without one, a probe.
    const probe = cards.size ? null : booksEl.appendChild(cardElements(SKELETON)[0]!);
    const card = probe ?? cards.values().next().value!;
    const cardH = card.getBoundingClientRect().height;
    const radius = parseFloat(getComputedStyle(card).borderTopLeftRadius) || 0;
    probe?.remove();
    rowH = cardH + rowGap;
    if (cardH <= 0) rows = 0; // no layout (hidden): nothing to place


    // One row of empty cards; CSS repeats it down the spacers.
    let x = 0;
    let rects = "";
    for (const w of tracks) {
        rects += `<rect x="${x}" width="${w}" height="${cardH}" rx="${radius}"/>`;
        x += w + colGap;
    }
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${x}" height="${rowH}">${rects}</svg>`;
    booksEl.style.setProperty("--skeleton-row", `url("data:image/svg+xml,${encodeURIComponent(svg)}")`);
    booksEl.style.setProperty("--skeleton-size", `${x}px ${rowH}px`);
    booksEl.style.setProperty("--skeleton-gap", `${rowGap}px`);
}

/** New width: same book at the top, laid out for the new column count. */
function relayout(): void {
    measure();
    jumpTo(anchor);
}

/** The row range to have in the DOM when `top` is the top row. */
function windowRows(top: number): [number, number] {
    const view = screenRows();
    const ahead = Math.ceil(view * RENDER_AHEAD);
    const behind = Math.ceil(view * RENDER_BEHIND);
    const down = scrollDir > 0;
    return [Math.max(0, top - (down ? behind : ahead)), Math.min(rows, top + view + (down ? ahead : behind))];
}

/** Parse card HTML into elements. */
function cardElements(html: string): HTMLElement[] {
    const t = document.createElement("template");
    t.innerHTML = html;
    return [...t.content.children] as HTMLElement[];
}

function cardHtml(i: number): string {
    const books = pageCache.get(pageOf(i));
    if (!books) return SKELETON;
    const book = books[i % pageSize];
    if (!book) return MISSING;
    return renderBookCard(book, loadedCovers.has(coverUrl(book)));
}

/** Cards for books [from, to), registered in `cards`; insert them, then activate them. */
function makeCards(from: number, to: number): HTMLElement[] {
    let html = "";
    for (let i = from; i < to; i++) html += cardHtml(i);
    const els = cardElements(html);
    els.forEach((el, k) => {
        cards.set(from + k, el);
        markReading(el);
    });
    return els;
}

/** htmx preloading for real cards; must run once they're in the document. */
function activate(els: HTMLElement[]): void {
    for (const el of els) {
        if (!el.classList.contains("skeleton") && !el.classList.contains("missing")) setupBookClickHandlers(el);
    }
}

function dropCard(i: number): void {
    const el = cards.get(i);
    if (!el) return;
    cards.delete(i);
    for (const img of el.querySelectorAll<HTMLImageElement>(".book-cover")) {
        coversInflight.delete(img);
        if (!img.complete) img.removeAttribute("src"); // cancel the download
    }
    el.remove();
}

/**
 * Have cards for books [a, b) and only those: keeps the ones already there,
 * adds and removes at the edges. The spacers make up the rest, so whatever
 * is on screen stays exactly where it is.
 */
function setWindow(a: number, b: number): void {
    if (a !== first || b !== last) {
        let added: HTMLElement[];
        if (b <= first || a >= last) {
            for (const i of [...cards.keys()]) dropCard(i);
            added = makeCards(a, b);
            bottomSpacer.before(...added);
        } else {
            for (let i = first; i < a; i++) dropCard(i);
            for (let i = b; i < last; i++) dropCard(i);
            const before = makeCards(a, first);
            const after = makeCards(last, b);
            topSpacer.after(...before);
            bottomSpacer.before(...after);
            added = [...before, ...after];
        }
        first = a;
        last = b;
        activate(added);
    }
    sizeSpacer(topSpacer, first / cols);
    sizeSpacer(bottomSpacer, rows - Math.ceil(last / cols));
}

function sizeSpacer(el: HTMLElement, rowCount: number): void {
    el.hidden = rowCount <= 0;
    el.style.height = `${rowCount * rowH - rowGap}px`; // the grid adds the gap after it
}

/** A page's data arrived: its skeletons on screen become cards. */
function fillPage(page: number): void {
    const from = Math.max(first, (page - 1) * pageSize);
    const to = Math.min(last, page * pageSize);
    if (from >= to || !cards.get(from)?.classList.contains("skeleton")) return;
    const old: HTMLElement[] = [];
    for (let i = from; i < to; i++) old.push(cards.get(i)!);
    const fresh = makeCards(from, to);
    old.forEach((el, k) => el.replaceWith(fresh[k]!));
    activate(fresh);
}

/**
 * Bring the DOM and data in line with the scroll position: render the rows
 * around the screen, fetch the pages around them (not mid-fling), queue
 * the covers, and update the page shown.
 */
function sync(): void {
    if (!rows) return;
    const top = clamp(Math.floor(viewTop() / rowH), 0, rows - 1);
    const [r0, r1] = windowRows(top);
    setWindow(r0 * cols, Math.min(total, r1 * cols));
    // Pages go by faster than they can be fetched (a scrollbar drag): they'd
    // be gone before they arrived. Skeletons until it slows.
    if ((speed * cols) / pageSize < MAX_PAGE_FETCHES / fetchSecs) fetchAround(top);
    // Covers are heavier, and only worth it once things slow down.
    if (speed * rowH < window.innerHeight * COVERS_WAIT_SCREENS_PER_SEC) pumpCovers();
    updatePageLabel(pageAtTop());
}

/** Lay out the rows around a book, then scroll its row to the top of the screen. */
function jumpTo(to: Anchor): void {
    anchor = to;
    const row = clamp(Math.floor(to.index / cols), 0, Math.max(0, rows - 1));
    const [r0, r1] = windowRows(row);
    setWindow(r0 * cols, Math.min(total, r1 * cols)); // the document is full height before scrolling
    window.scrollBy({ top: (row + to.frac) * rowH - viewTop(), behavior: "instant" });
    lastY = window.scrollY; // ours, not the user's: no fling, and the anchor stays exact
    sync();
    scheduleHistoryUpdate();
}

/** The anchor for the current scroll position: the nearest row's first book. */
function anchorHere(): Anchor {
    const y = viewTop() / rowH;
    const row = clamp(Math.round(y), 0, rows - 1);
    return { index: row * cols, frac: y - row };
}

/**
 * The page the top row belongs to: of its last book, so a jump to page n
 * (which puts the row with its first book at the top) shows n.
 */
function pageAtTop(): number {
    const row = clamp(Math.round(viewTop() / rowH), 0, rows - 1);
    return pageOf(Math.min(total, (row + 1) * cols) - 1);
}

/* ------------------------------------------------------------------ */
/* Fetching                                                           */
/* ------------------------------------------------------------------ */

async function fetchPage(page: number, signal: AbortSignal): Promise<Book[] | null> {
    try {
        const started = performance.now();
        const res = await fetch(pageUrl(page), { signal, priority: "high" });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const books: unknown = await res.json();
        if (!Array.isArray(books)) throw new Error("not a list of books");
        fetchSecs = fetchSecs * 0.7 + ((performance.now() - started) / 1000) * 0.3;
        pageCache.set(page, books as Book[]);
        failedAt.delete(page);
        return books as Book[];
    } catch (err) {
        if (!(err instanceof DOMException && err.name === "AbortError")) {
            console.error(`Failed to fetch page ${page}:`, err);
            failedAt.set(page, performance.now());
        }
        return null;
    }
}

/** Cached or in-flight data for a page; fetches (abortable) if neither. */
function loadPageData(page: number): Promise<Book[] | null> {
    const cached = pageCache.get(page);
    if (cached) return Promise.resolve(cached);

    let entry = loading.get(page);
    if (!entry) {
        const ctrl = new AbortController();
        const promise = fetchPage(page, ctrl.signal).finally(() => {
            if (loading.get(page) === entry) loading.delete(page);
        });
        entry = { promise, ctrl };
        loading.set(page, entry);
    }
    return entry.promise;
}

/** Pages holding rows [r0, r1), nearest `from` first. */
function pagesOfRows(r0: number, r1: number, from: "top" | "bottom"): number[] {
    r0 = Math.max(0, r0);
    r1 = Math.min(rows, r1);
    if (r0 >= r1) return [];
    const p0 = pageOf(r0 * cols);
    const p1 = pageOf(Math.min(total, r1 * cols) - 1);
    const pages: number[] = [];
    for (let p = p0; p <= p1; p++) pages.push(p);
    return from === "top" ? pages : pages.reverse();
}

/**
 * Fetch the pages around the screen: what's on it first, then ahead in the
 * scroll direction (further the faster it goes, so pages arrive before the
 * screen does), then behind. Fetches for pages no longer wanted are
 * aborted. Each arrival fills its cards and comes back here for the next.
 */
function fetchAround(top: number): void {
    const view = screenRows();
    const ahead = Math.ceil(Math.max(view * FETCH_AHEAD, speed * fetchSecs * FETCH_LEAD));
    const behind = Math.ceil(view * RENDER_BEHIND);
    const bottom = top + view;
    const down = scrollDir > 0;
    const order = down
        ? [
              ...pagesOfRows(top, bottom, "top"),
              ...pagesOfRows(bottom, bottom + ahead, "top"),
              ...pagesOfRows(top - behind, top, "bottom"),
          ]
        : [
              ...pagesOfRows(top, bottom, "bottom"),
              ...pagesOfRows(top - ahead, top, "bottom"),
              ...pagesOfRows(bottom, bottom + behind, "top"),
          ];
    const wanted = new Set(order);

    for (const [n, entry] of loading) {
        if (!wanted.has(n)) {
            entry.ctrl.abort();
            loading.delete(n);
        }
    }

    const now = performance.now();
    for (const n of wanted) {
        if (loading.size >= MAX_PAGE_FETCHES) break;
        if (pageCache.has(n) || loading.has(n)) continue;
        if (now - (failedAt.get(n) ?? -Infinity) < RETRY_FAILED_MS) continue;
        loadPageData(n).then((books) => {
            if (!books) return;
            fillPage(n);
            sync(); // the next fetch, and the new covers
        });
    }
}

/* ------------------------------------------------------------------ */
/* Navigation                                                         */
/* ------------------------------------------------------------------ */

/** Scroll so the row with the page's first book sits under the header. */
function scrollToPage(page: number, { syncInput = true }: { syncInput?: boolean } = {}): void {
    if (!Number.isFinite(page) || !rows) return;
    page = clampPage(page);
    if (syncInput && pageInputEl) pageInputEl.value = String(page);
    scrollDir = page >= targetPage ? 1 : -1;
    jumpTo({ index: (page - 1) * pageSize, frac: 0 });
}

const pageTopRow = (page: number): number => Math.floor(((page - 1) * pageSize) / cols);
const pageBottomRow = (page: number): number => Math.floor((Math.min(total, page * pageSize) - 1) / cols);

/** Home: the top of the current page; if already there, of the previous. */
function scrollToPageTop(): void {
    const y = viewTop();
    let page = targetPage;
    if (pageTopRow(page) * rowH >= y - 2) page--;
    const to = page < 1 ? 0 : pageTopRow(page) * rowH;
    window.scrollBy({ top: to - y, behavior: "instant" });
}

/** End: the bottom of the current page at the bottom of the screen; if already there, the next page's. */
function scrollToPageBottom(): void {
    const y = viewTop();
    const screen = window.innerHeight - headerHeight();
    const bottomAt = (page: number): number => (pageBottomRow(page) + 1) * rowH - rowGap - screen;
    let page = targetPage;
    if (bottomAt(page) <= y + 2) page++;
    if (page > totalPages) return;
    window.scrollBy({ top: bottomAt(page) - y, behavior: "instant" });
}

function updatePageLabel(page: number): void {
    if (page === lastLabelPage) return;

    lastLabelPage = page;
    targetPage = page;
    // Don't overwrite what the user is typing.
    if (pageInputEl && document.activeElement !== pageInputEl) {
        pageInputEl.value = String(page);
    }
    updateNavigationButtons();
}

/** Trailing-edge debounce; only the settled position reaches history. */
function scheduleHistoryUpdate(): void {
    historyPending = true;
    clearTimeout(historyTimer);
    historyTimer = setTimeout(flushHistory, HISTORY_DEBOUNCE_MS);
}

/**
 * Writes the position to the URL (?page) and history.state (the exact
 * book). Not while the book modal is open: it closes by going back to the
 * entry before its #book/ one, and that's only a hash change (which closes
 * it) if both entries have the same ?page. The position waits until the
 * modal closes ("cf:modal-closed" from modal.ts).
 */
function flushHistory(): void {
    clearTimeout(historyTimer);
    if (!historyPending || window.location.hash.startsWith(HASH_PREFIX)) return;
    historyPending = false;
    const page = targetPage;
    try {
        const params = new URLSearchParams(window.location.search);
        params.set("page", String(page));
        history.replaceState(
            { ...history.state, page, at: anchor.index, frac: Math.round(anchor.frac * 1000) / 1000 },
            "",
            `?${params}${window.location.hash}`,
        );
    } catch (err) {
        console.warn("history.replaceState throttled:", err);
    }
}

function updateNavigationButtons(): void {
    if (prevBtn) prevBtn.disabled = targetPage <= 1;
    if (nextBtn) nextBtn.disabled = targetPage >= totalPages;
}

/* ------------------------------------------------------------------ */
/* Scroll tracking                                                    */
/* ------------------------------------------------------------------ */

function setupScrollTracking(): void {
    let scheduled = false;
    let lastT = performance.now();
    let settleTimer: ReturnType<typeof setTimeout> | undefined;

    window.addEventListener(
        "scroll",
        () => {
            if (scheduled) return;
            scheduled = true;
            requestAnimationFrame(() => {
                scheduled = false;

                const now = performance.now();
                const y = window.scrollY;
                const dy = y - lastY; // 0 after our own jumpTo
                speed = (Math.abs(dy) / rowH / (now - lastT || 1)) * 1000;
                lastY = y;
                lastT = now;
                if (dy) {
                    scrollDir = dy > 0 ? 1 : -1;
                    anchor = anchorHere();
                    scheduleHistoryUpdate();
                }

                // Stopped: whatever speed held back (see sync) catches up.
                clearTimeout(settleTimer);
                settleTimer = setTimeout(() => {
                    speed = 0;
                    sync();
                }, SETTLE_MS);
                sync();
                if (draggingScrollbar) showScrollBadge(targetPage);
            });
        },
        { passive: true },
    );

    // The badge is a drag affordance: only show it while the scrollbar is held
    // (or a finger is down), not for keys/wheel where the top bar already says it.
    window.addEventListener(
        "mousedown",
        (e) => {
            const root = document.documentElement;
            const edge = Math.min(root.clientWidth, window.innerWidth - 16); // classic + overlay scrollbars
            draggingScrollbar = e.button === 0 && e.clientX >= edge;
        },
        true,
    );
    const endDrag = () => (draggingScrollbar = false);
    window.addEventListener("mouseup", endDrag, true);
    window.addEventListener("dragend", endDrag, true);
    window.addEventListener("blur", endDrag);
    window.addEventListener("touchstart", () => (draggingScrollbar = true), {
        passive: true,
    });
    window.addEventListener("touchend", endDrag, { passive: true });
    window.addEventListener("touchcancel", endDrag, { passive: true });

    // Home/End step one page per press; auto-repeat is ignored.
    window.addEventListener("keydown", (e) => {
        if (e.key !== "Home" && e.key !== "End") return;
        if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
        if (e.shiftKey) return;
        if (e.target instanceof Element && e.target.closest("input, textarea, select, [contenteditable]"))
            return;
        if (window.location.hash.startsWith(HASH_PREFIX)) return; // modal open

        e.preventDefault();
        if (e.repeat || keyScrolling) return;
        keyScrolling = true;
        if (e.key === "End") scrollToPageBottom();
        else scrollToPageTop();
        requestAnimationFrame(() => (keyScrolling = false));
    });

    window.addEventListener("pagehide", flushHistory);
    // Closing a deep-linked modal clears history.state: always write it back.
    document.addEventListener("cf:modal-closed", () => {
        historyPending = true;
        flushHistory();
    });
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden") flushHistory();
    });

    // A new width (window resize, a scrollbar appearing, rotation) changes
    // the columns and the row height: lay out again around the same book.
    // The observer runs after layout and before paint, so the reflowed grid
    // is never shown with stale spacers.
    new ResizeObserver((entries) => {
        const width = entries[0]?.contentRect.width ?? lastWidth;
        if (width === lastWidth) return;
        lastWidth = width;
        relayout();
    }).observe(booksEl);
    // A new height alone (mobile URL bar) only changes how many rows to
    // render. A new width is the observer's.
    window.addEventListener("resize", () => {
        if (booksEl.getBoundingClientRect().width === lastWidth) sync();
    });
}

/* ------------------------------------------------------------------ */
/* Covers                                                             */
/* ------------------------------------------------------------------ */

/** True while the book modal is loading: grid covers wait (see modal.ts). */
let coversPaused = false;

/**
 * Abort an in-flight cover download and put it back in the queue. Removing
 * src cancels the request without firing "error" (which would mark the book
 * as having no cover); a later retry is served from the browser cache if the
 * image had already arrived.
 */
function cancelCover(img: HTMLImageElement): void {
    coversInflight.delete(img);
    const src = img.getAttribute("src");
    if (!src || img.complete) return;
    img.removeAttribute("src");
    img.dataset["src"] = src;
}

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
function maxCovers(): number {
    const effective = (navigator as Navigator & { connection?: { effectiveType?: string } }).connection?.effectiveType;
    if (effective?.endsWith("2g")) return COVERS_2G;
    if (effective === "3g") return COVERS_3G;
    return /^h[23]/.test(pageProtocol()) ? COVERS_MULTIPLEXED : COVERS_HTTP1;
}

/**
 * Load covers on screen first, then the rest nearest the viewport (favouring
 * the scroll direction), keeping at most maxCovers() requests open:
 *   - off-screen covers only start once nothing on screen is waiting or loading
 *   - while an on-screen cover waits, off-screen downloads are cancelled
 *   - nothing starts while the book modal is loading
 * Each finished image pumps the queue again.
 */
function pumpCovers(): void {
    // Images removed with their card never fire load/error.
    for (const img of coversInflight) {
        if (!img.isConnected) coversInflight.delete(img);
    }
    if (coversPaused) return;

    const header = headerHeight();
    const vh = window.innerHeight;
    const down = scrollDir > 0;
    // Distance from the viewport in px (0 = on screen); covers behind the
    // scroll direction cost double.
    const distance = (img: HTMLImageElement): number => {
        const r = img.getBoundingClientRect();
        if (r.top > vh) return (r.top - vh) * (down ? 1 : 2);
        if (r.bottom < header) return (header - r.bottom) * (down ? 2 : 1);
        return 0;
    };

    const queue = [...booksEl.querySelectorAll<HTMLImageElement>(".book-cover[data-src]")].map((img) => ({
        img,
        dist: distance(img),
    }));
    const onScreenWaiting = queue.some((c) => c.dist === 0);

    let onScreenLoading = false;
    for (const img of [...coversInflight]) {
        const dist = distance(img);
        if (dist === 0) onScreenLoading = true;
        else if (onScreenWaiting) {
            cancelCover(img); // give the slot (and bandwidth) to the viewport
            queue.push({ img, dist });
        }
    }
    queue.sort((a, b) => a.dist - b.dist);

    const cap = maxCovers();
    for (const { img, dist } of queue) {
        if (coversInflight.size >= cap) break;
        if (dist > 0 && (onScreenWaiting || onScreenLoading)) break; // viewport first

        const src = img.dataset["src"] ?? "";
        delete img.dataset["src"];
        coversInflight.add(img);
        if (dist === 0) onScreenLoading = true;
        img.src = src;
    }
}

// The book modal is loading: stop background covers until it's done.
document.addEventListener("cf:modal-busy", (e) => {
    coversPaused = (e as CustomEvent<boolean>).detail;
    if (coversPaused) {
        for (const img of [...coversInflight]) cancelCover(img);
    } else {
        pumpCovers();
    }
});

function onCoverSettled(e: Event): void {
    const img = e.target;
    if (!(img instanceof HTMLImageElement) || !img.classList.contains("book-cover")) return;
    if (e.type === "load") loadedCovers.add(img.getAttribute("src") ?? "");
    if (coversInflight.delete(img)) pumpCovers();
}
document.addEventListener("load", onCoverSettled, true);
document.addEventListener("error", onCoverSettled, true);

/* ------------------------------------------------------------------ */
/* Controls                                                           */
/* ------------------------------------------------------------------ */

const pageInput = pageInputEl;

if (pageInput) {
    // Arrow keys / spinner / typing: jump immediately, leave the field alone.
    pageInput.addEventListener("input", () => {
        const n = parseInt(pageInput.value, 10);
        if (Number.isNaN(n)) return; // mid-edit (empty field)
        scrollToPage(n, { syncInput: false });
    });

    // Blur / Enter: normalize whatever is in the field.
    pageInput.addEventListener("change", () => {
        const n = parseInt(pageInput.value, 10);
        if (Number.isNaN(n)) {
            pageInput.value = String(targetPage);
            return;
        }
        pageInput.value = String(clampPage(n));
    });
}

nextBtn?.addEventListener("click", () => scrollToPage(targetPage + 1));
prevBtn?.addEventListener("click", () => scrollToPage(targetPage - 1));
