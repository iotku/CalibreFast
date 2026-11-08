/**
 * CalibreFast Main Library
 *
 * The document always contains one element per page (index = page - 1).
 * Pages near the viewport are real; all others are fixed-height placeholders.
 * Scroll limits are therefore physical, not "end of what's loaded", so no
 * input method (keys, fling, scrollbar) can run away by chasing the loader.
 *
 * Data source is configured on #books:
 *   data-page-url    URL template, "{n}" -> page number (default: static JSON)
 *   data-total-pages total page count; if absent, fetched from /library-info
 * Page nav controls (#page-input, #prev-page, #next-page) are optional.
 */

import { renderBooksPage } from "./utils.js";
import { setupBookClickHandlers, loadBook, HASH_PREFIX } from "./modal.js";

const KEEP_AHEAD = 1; // real pages kept in the scroll direction
const KEEP_BEHIND = 1; // ...and the opposite way
const MAX_PAGE_FETCHES = 2; // concurrent page-JSON requests
const HISTORY_DEBOUNCE_MS = 300; // browsers throttle rapid history updates
const FAST_SCROLL_PAGES_PER_SEC = 4; // faster than this: don't fetch/render yet
const SETTLE_MS = 120; // quiet time before rendering after a fast scroll
const MAX_COVERS_INFLIGHT = 6 - MAX_PAGE_FETCHES; // headroom under the 6-per-host limit

const coversInflight = new Set();
const pageCache = new Map(); // page -> books
const loading = new Map(); // page -> { promise, ctrl }
const realPages = new Set(); // page numbers currently rendered for real

window.totalPages = null;
let pageHeight = 0; // estimated px height of one page
let targetPage = 1; // page the UI says we're on / heading to
let lastLabelPage = null;
let lastWidth = window.innerWidth;
let keyScrolling = false;
let scrollDir = 1; // 1 = down, -1 = up; biases the render window
let scrollAdjust = 0; // px of programmatic compensation the scroll handler hasn't seen yet
const badgeEl = document.getElementById("scroll-badge"); // in index.html
let draggingScrollbar = false; // mouse held on the scrollbar, or a touch is active
let badgeTimer = null;
let historyTimer = null;
let historyPage = null; // page waiting to be written to the URL

const $ = (id) => document.getElementById(id);
const headerHeight = () => document.querySelector(".top-bar").offsetHeight;
const pageEl = (n) => $("books").children[n - 1];
const booksCfg = $("books").dataset;
const pageUrl = (n) =>
    (booksCfg.pageUrl || "/pages/page{n}.json").replace("{n}", n);

function setStatus(text) {
    const el = $("status");
    if (el) el.textContent = text;
}

function clampPage(n) {
    const max = window.totalPages || Infinity;
    return Math.max(1, Math.min(n, max));
}

/** Floating "page / total" label that rides alongside the scrollbar thumb. */
function showScrollBadge(page) {
    if (!badgeEl) return;
    const root = document.documentElement;
    const header = headerHeight();
    const ratio =
        window.scrollY / Math.max(1, root.scrollHeight - window.innerHeight);
    const room = window.innerHeight - header - badgeEl.offsetHeight - 8;

    badgeEl.textContent = `${page} / ${window.totalPages}`;
    badgeEl.style.top = `${header + 4 + ratio * Math.max(0, room)}px`;
    badgeEl.classList.add("visible");

    clearTimeout(badgeTimer);
    badgeTimer = setTimeout(() => badgeEl.classList.remove("visible"), 700);
}

/* ------------------------------------------------------------------ */
/* Init                                                               */
/* ------------------------------------------------------------------ */

window.addEventListener("DOMContentLoaded", async () => {
    await loadLibraryInfo();

    const params = new URLSearchParams(window.location.search);
    const urlPage = parseInt(params.get("page") || "1", 10);
    const start = clampPage(Number.isFinite(urlPage) ? urlPage : 1);

    // Make the hash-less URL the base history entry.
    const initialHash = window.location.hash;
    if (initialHash) {
        history.replaceState(
            history.state,
            "",
            window.location.pathname + window.location.search,
        );
    }

    window.history.scrollRestoration = "manual";
    window.scrollTo(0, 0);

    // We compensate scroll manually when swapping pages above the viewport.
    $("books").style.overflowAnchor = "none";
    setupCoverStates($("books"));

    await buildDocument(start);

    // Re-add the hash as a new entry on top, then open the modal.
    if (initialHash.startsWith(HASH_PREFIX)) {
        const url = new URL(window.location.href);
        url.hash = initialHash;
        history.pushState({ page: targetPage, modal: true }, "", url);
        loadBook(initialHash.substring(HASH_PREFIX.length));
    }

    setupScrollTracking();
});

export function setupCoverStates(container) {
    container.addEventListener(
        "load",
        (e) => {
            if (e.target.matches?.(".book-cover"))
                e.target.classList.add("loaded");
        },
        true,
    );

    container.addEventListener(
        "error",
        (e) => {
            const img = e.target;
            if (!img.matches?.(".book-cover")) return;
            img.parentElement?.classList.add("no-cover"); // placeholder stays visible
            img.remove();
        },
        true,
    );
}

async function loadLibraryInfo() {
    try {
        if (booksCfg.totalPages !== undefined) {
            window.totalPages = parseInt(booksCfg.totalPages, 10) || 0;
        } else {
            const res = await fetch("/library-info");
            const info = await res.json();
            window.totalPages = info.total_pages;
        }

        if ($("total-pages")) $("total-pages").textContent = window.totalPages;
        if ($("page-input")) $("page-input").max = window.totalPages; // arrow keys clamp natively
        updateNavigationButtons();
    } catch (err) {
        console.error("Failed to load library info:", err);
    }
}

/**
 * Render the first real page, measure it, then lay out one placeholder per
 * page of the library at that height and scroll to the start page.
 */
async function buildDocument(start) {
    const total = window.totalPages;
    const empty = booksCfg.emptyText ?? "No books found.";
    if (!total) return setStatus(empty);

    const books = await loadPageData(start);
    if (!books || books.length === 0) return setStatus(empty);

    const booksEl = $("books");
    booksEl.innerHTML = "";

    const first = buildPageEl(books, start);
    if (!first) return;
    booksEl.appendChild(first);
    pageHeight = pageExtent(first) || 1000;
    booksEl.style.setProperty("--page-h", `${pageHeight}px`);

    const frag = document.createDocumentFragment();
    for (let n = 1; n <= total; n++) {
        frag.appendChild(n === start ? first : makePlaceholder(n));
    }
    booksEl.replaceChildren(frag);
    realPages.add(start);
    setupBookClickHandlers(first); // must run while attached to the document

    scrollToPage(start);
}

/* ------------------------------------------------------------------ */
/* Fetching                                                           */
/* ------------------------------------------------------------------ */

async function fetchPage(page, signal) {
    try {
        const res = await fetch(pageUrl(page), {
            signal,
            priority: "high",
        });
        if (!res.ok) return null;
        const books = await res.json();
        pageCache.set(page, books);
        return books;
    } catch (err) {
        if (err.name !== "AbortError") {
            console.error(`Failed to fetch page ${page}:`, err);
        }
        return null;
    }
}

/** Cached or in-flight data for a page; fetches (abortable) if neither. */
function loadPageData(page) {
    if (pageCache.has(page)) return Promise.resolve(pageCache.get(page));

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

/* ------------------------------------------------------------------ */
/* Pages and placeholders                                             */
/* ------------------------------------------------------------------ */

function buildPageEl(books, page) {
    const temp = document.createElement("div");
    temp.innerHTML = renderBooksPage(books, page);
    return temp.firstElementChild; // htmx processing happens after insertion
}

function makePlaceholder(n, height) {
    const ph = document.createElement("div");
    ph.className = "page placeholder";
    ph.dataset.page = n;
    // Only pin a height when it differs from the shared estimate.
    if (height && Math.abs(height - pageHeight) > 1) {
        ph.style.height = `${height}px`;
    }
    return ph;
}

/**
 * Viewport-relative top/bottom of a page. `display: contents` wrappers have
 * no box, so measure the span of their children instead.
 */
function pageBounds(el) {
    if (
        el.classList.contains("placeholder") ||
        getComputedStyle(el).display !== "contents"
    ) {
        const r = el.getBoundingClientRect();
        return { top: r.top, bottom: r.bottom };
    }
    let top = Infinity;
    let bottom = -Infinity;
    for (const child of el.children) {
        const r = child.getBoundingClientRect();
        top = Math.min(top, r.top);
        bottom = Math.max(bottom, r.bottom);
    }
    return bottom > top ? { top, bottom } : { top: 0, bottom: 0 };
}

function pageExtent(el) {
    const { top, bottom } = pageBounds(el);
    return bottom - top;
}

/** Make a page real. Scroll compensation is handled by syncWindow. */
function renderPage(n, books) {
    if (realPages.has(n)) return;
    const ph = pageEl(n);
    if (!ph) return;
    const el = buildPageEl(books, n);
    if (!el) return;

    el.classList.add("appear"); // fade-in, see CSS
    ph.replaceWith(el);
    realPages.add(n);
    setupBookClickHandlers(el); // must run while attached to the document
}

/** Replace a real page with a same-height placeholder. */
function evictPage(n) {
    const el = pageEl(n);
    realPages.delete(n);
    if (!el) return;
    const height = pageExtent(el);
    for (const img of el.querySelectorAll(".book-cover")) {
        coversInflight.delete(img);
        img.removeAttribute("src"); // cancel inflight fetches
    }
    el.replaceWith(makePlaceholder(n, height));
}

/**
 * Make the pages around the viewport real and everything else a placeholder.
 * Pins the visible page's top across all swaps. Returns the visible page.
 */
function syncWindow() {
    if (!window.totalPages || !pageHeight) return targetPage;

    const visible = getVisiblePage();
    const anchorTop = pageBounds(pageEl(visible)).top;

    // Window leans in the scroll direction so we stay ahead of the viewport.
    const down = scrollDir > 0;
    const lo = Math.max(1, visible - (down ? KEEP_BEHIND : KEEP_AHEAD));
    const hi = Math.min(
        window.totalPages,
        visible + (down ? KEEP_AHEAD : KEEP_BEHIND),
    );

    for (const n of [...realPages]) {
        if (n < lo || n > hi) evictPage(n);
    }

    // Stop fetching pages the user has already left behind.
    for (const [n, entry] of loading) {
        if (n < lo || n > hi) {
            entry.ctrl.abort();
            loading.delete(n);
        }
    }

    // Visible page first, then alternating ahead/behind, nearest first.
    const order = [visible];
    for (let d = 1; d <= KEEP_AHEAD; d++) {
        order.push(visible + scrollDir * d, visible - scrollDir * d);
    }

    for (const n of order) {
        if (n < lo || n > hi || realPages.has(n)) continue;

        if (pageCache.has(n)) {
            renderPage(n, pageCache.get(n));
        } else if (!loading.has(n) && loading.size < MAX_PAGE_FETCHES) {
            // On arrival, re-sync: renders it and starts the next fetch.
            loadPageData(n).then((books) => {
                if (books) syncWindow();
            });
        }
    }

    // Swaps above the viewport can change heights: pin the visible page.
    const drift = pageBounds(pageEl(visible)).top - anchorTop;
    if (Math.abs(drift) >= 1) {
        scrollAdjust += drift;
        window.scrollBy({ top: drift, behavior: "instant" });
    }

    pumpCovers();
    return visible;
}

/* ------------------------------------------------------------------ */
/* Navigation                                                         */
/* ------------------------------------------------------------------ */

/** Scroll so the page's top sits under the header, then render around it. */
function scrollToPage(page, { syncInput = true } = {}) {
    if (!Number.isFinite(page)) return;
    page = clampPage(page);
    const el = pageEl(page);
    if (!el) return;

    targetPage = page;
    if (syncInput && $("page-input")) $("page-input").value = page;

    const delta = pageBounds(el).top - headerHeight();
    if (delta) scrollDir = delta > 0 ? 1 : -1;
    window.scrollBy({ top: delta, behavior: "instant" });

    updatePageLabel(syncWindow());
}

/** Home: top of the visible page; if already there, top of the previous. */
function scrollToPageTop() {
    const visible = getVisiblePage();
    let delta = pageBounds(pageEl(visible)).top - headerHeight();

    if (delta > -2) {
        if (visible <= 1) {
            window.scrollTo({ top: 0, behavior: "instant" });
            return;
        }
        delta = pageBounds(pageEl(visible - 1)).top - headerHeight();
    }
    window.scrollBy({ top: delta, behavior: "instant" });
}

/** End: bottom of the visible page; if already there, bottom of the next. */
function scrollToPageBottom() {
    const visible = getVisiblePage();
    let bottom = pageBounds(pageEl(visible)).bottom;

    if (bottom - window.innerHeight <= 2) {
        if (visible >= window.totalPages) return;
        bottom = pageBounds(pageEl(visible + 1)).bottom;
    }
    window.scrollBy({ top: bottom - window.innerHeight, behavior: "instant" });
}

/** Binary search: last page whose top has reached the header. */
function getVisiblePage() {
    const pages = $("books").children;
    if (pages.length === 0) return targetPage;

    const limit = headerHeight() + 5;
    let lo = 0;
    let hi = pages.length - 1;
    while (lo < hi) {
        const mid = (lo + hi + 1) >> 1;
        if (pageBounds(pages[mid]).top <= limit) lo = mid;
        else hi = mid - 1;
    }
    return lo + 1;
}

function updatePageLabel(page) {
    if (page === lastLabelPage) return;

    lastLabelPage = page;
    targetPage = page;
    // Don't overwrite what the user is typing.
    if ($("page-input") && document.activeElement !== $("page-input")) {
        $("page-input").value = page;
    }
    scheduleHistoryUpdate(page);
    updateNavigationButtons();
}

/** Trailing-edge debounce; only the settled page reaches history. */
function scheduleHistoryUpdate(page) {
    historyPage = page;
    clearTimeout(historyTimer);
    historyTimer = setTimeout(flushHistory, HISTORY_DEBOUNCE_MS);
}

function flushHistory() {
    clearTimeout(historyTimer);
    if (historyPage === null) return;
    const page = historyPage;
    historyPage = null;
    try {
        // Keep the hash and state so an open modal's entry isn't clobbered.
        const params = new URLSearchParams(window.location.search);
        params.set("page", page);
        history.replaceState(
            { ...history.state, page },
            "",
            `?${params}${window.location.hash}`,
        );
    } catch (err) {
        console.warn("history.replaceState throttled:", err);
    }
}

function updateNavigationButtons() {
    if ($("prev-page")) $("prev-page").disabled = targetPage <= 1;
    if ($("next-page")) $("next-page").disabled = targetPage >= window.totalPages;
}

/* ------------------------------------------------------------------ */
/* Scroll tracking                                                    */
/* ------------------------------------------------------------------ */

function setupScrollTracking() {
    let scheduled = false;
    let lastY = window.scrollY;
    let lastT = performance.now();
    let settleTimer = null;

    window.addEventListener(
        "scroll",
        () => {
            if (scheduled) return;
            scheduled = true;
            requestAnimationFrame(() => {
                scheduled = false;

                const now = performance.now();
                const y = window.scrollY;
                // Ignore our own compensation: only user scroll counts.
                const dy = y - lastY - scrollAdjust;
                scrollAdjust = 0;
                const pxPerSec = (Math.abs(dy) / (now - lastT || 1)) * 1000;
                if (dy) scrollDir = dy > 0 ? 1 : -1;
                lastY = y;
                lastT = now;
                clearTimeout(settleTimer);

                if (
                    pageHeight &&
                    pxPerSec / pageHeight > FAST_SCROLL_PAGES_PER_SEC
                ) {
                    // Flying past: keep the label live, but skip fetching and
                    // rendering pages we're about to leave. Catch up once it settles.
                    const visible = getVisiblePage();
                    updatePageLabel(visible);
                    if (draggingScrollbar) showScrollBadge(visible);
                    settleTimer = setTimeout(
                        () => updatePageLabel(syncWindow()),
                        SETTLE_MS,
                    );
                } else {
                    updatePageLabel(syncWindow());
                }
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
        if (e.target.closest?.("input, textarea, select, [contenteditable]"))
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
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden") flushHistory();
    });

    // Column count changes with width => page height changes.
    let resizeTimer;
    window.addEventListener("resize", () => {
        clearTimeout(resizeTimer);
        resizeTimer = setTimeout(onResize, 150);
    });
}

function onResize() {
    if (window.innerWidth === lastWidth) return; // mobile URL-bar show/hide
    lastWidth = window.innerWidth;

    const keep = getVisiblePage();
    const el = pageEl(keep);
    if (realPages.has(keep)) {
        pageHeight = pageExtent(el) || pageHeight;
        $("books").style.setProperty("--page-h", `${pageHeight}px`);
    }
    for (const ph of $("books").querySelectorAll(".placeholder[style]")) {
        ph.style.height = "";
    }
    scrollToPage(keep);
}

/* ------------------------------------------------------------------ */
/* Covers                                                             */
/* ------------------------------------------------------------------ */

/**
 * Start pending covers nearest the viewport first, favouring the scroll
 * direction, keeping at most MAX_COVERS_INFLIGHT requests open. Each
 * finished image pumps the queue again.
 */
function pumpCovers() {
    // Images removed by a placeholder swap never fire load/error.
    for (const img of coversInflight) {
        if (!img.isConnected) coversInflight.delete(img);
    }
    if (coversInflight.size >= MAX_COVERS_INFLIGHT) return;

    const header = headerHeight();
    const vh = window.innerHeight;
    const down = scrollDir > 0;

    // Distance from the viewport in px; covers behind the scroll direction cost double.
    const queue = [];
    for (const img of document.querySelectorAll(".book-cover[data-src]")) {
        const r = img.getBoundingClientRect();
        let dist = 0;
        if (r.top > vh) dist = (r.top - vh) * (down ? 1 : 2);
        else if (r.bottom < header) dist = (header - r.bottom) * (down ? 2 : 1);
        queue.push({ img, dist });
    }
    queue.sort((a, b) => a.dist - b.dist);

    for (const { img } of queue) {
        if (coversInflight.size >= MAX_COVERS_INFLIGHT) break;

        const src = img.dataset.src;
        delete img.dataset.src;
        coversInflight.add(img);

        const done = () => pumpCovers();
        img.addEventListener("load", done, { once: true });
        img.addEventListener("error", done, { once: true });
        img.src = src;
    }
}

function onCoverSettled(e) {
    const img = e.target;
    if (!img.classList?.contains("book-cover")) return;
    if (coversInflight.delete(img)) pumpCovers();
}
document.addEventListener("load", onCoverSettled, true);
document.addEventListener("error", onCoverSettled, true);

/* ------------------------------------------------------------------ */
/* Controls                                                           */
/* ------------------------------------------------------------------ */

const pageInput = $("page-input");

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
            pageInput.value = targetPage;
            return;
        }
        pageInput.value = clampPage(n);
    });
}

$("next-page")?.addEventListener("click", () => scrollToPage(targetPage + 1));
$("prev-page")?.addEventListener("click", () => scrollToPage(targetPage - 1));
