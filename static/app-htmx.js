/**
 * CalibreFast Main Library - htmx Implementation
 * Infinite scroll with JSON transformation
 */

import { renderBooksPage } from "./utils.js";
import { setupBookClickHandlers, loadBook, HASH_PREFIX } from "./modal.js";

const NAV_DEBOUNCE_MS = 0;
const BUFFER_PAGES = 2;
const prefetch = new Map(); // page -> Promise<books|null>

// Global state
window.totalPages = null;
let currentPage = 1; // last page appended to the DOM
let targetPage = 1; // page the UI says we're on / heading to
let navigating = false; // a jump is pending or in flight
let navToken = 0; // bumped on every nav request; stale results are dropped
let navTimer = null;
let navAbort = null;
let loadingNext = false;
let lastReplacedPage = null;
let sentinelObserver = null;

const $ = (id) => document.getElementById(id);

function clampPage(n) {
    const max = window.totalPages || Infinity;
    return Math.max(1, Math.min(n, max));
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

    await goToPage(start, ++navToken);

    // Re-add the hash as a new entry on top, then open the modal.
    if (initialHash.startsWith(HASH_PREFIX)) {
        const url = new URL(window.location.href);
        url.hash = initialHash;
        history.pushState({ page: currentPage, modal: true }, "", url);
        loadBook(initialHash.substring(HASH_PREFIX.length));
    }

    setupInfiniteScrollSentinel();
    setupScrollTracking();
});

async function loadLibraryInfo() {
    try {
        const res = await fetch("/library-info");
        const info = await res.json();
        window.totalPages = info.total_pages;

        $("total-pages").textContent = window.totalPages;
        $("page-input").max = window.totalPages; // arrow keys clamp natively
        updateNavigationButtons();
    } catch (err) {
        console.error("Failed to load library info:", err);
    }
}

/* ------------------------------------------------------------------ */
/* Fetching                                                           */
/* ------------------------------------------------------------------ */

async function fetchPage(page, signal) {
    if (!Number.isInteger(page) || page < 1) return null;
    try {
        const res = await fetch(`/pages/page${page}.json`, {
            signal,
            priority: "high",
        });
        if (!res.ok) return null;
        return await res.json();
    } catch (err) {
        if (err.name !== "AbortError") {
            console.error(`Failed to fetch page ${page}:`, err);
        }
        return null;
    }
}

/* ------------------------------------------------------------------ */
/* Navigation (jump to page)                                          */
/* ------------------------------------------------------------------ */

/**
 * Request a jump. Debounced; supersedes any pending/in-flight jump.
 * @param {number} target
 * @param {{syncInput?: boolean, delay?: number}} opts
 */

function requestPage(
    target,
    { syncInput = true, delay = NAV_DEBOUNCE_MS } = {},
) {
    if (!Number.isFinite(target)) return;
    const page = clampPage(target);

    const token = ++navToken;
    clearTimeout(navTimer);
    navAbort?.abort();

    targetPage = page;
    if (syncInput) $("page-input").value = page;

    if (page === getVisiblePage()) {
        navigating = false;
        updateNavigationButtons();
        setupCoverLazyLoading($("books")); // resumes the queue, re-queues covers
        return;
    }

    navigating = true;
    updateNavigationButtons();
    navTimer = setTimeout(() => goToPage(page, token), delay);
}

async function goToPage(page, token) {
    navAbort?.abort();
    navAbort = new AbortController();

    const books = await fetchPage(page, navAbort.signal);
    if (token !== navToken) return; // superseded

    navigating = false;

    if (!books || books.length === 0) {
        targetPage = getVisiblePage();
        $("page-input").value = targetPage;
        updateNavigationButtons();
        pumpCovers();
        return;
    }

    currentPage = page;
    targetPage = page;
    lastReplacedPage = page;
    loadingNext = false;
    prefetch.clear();

    const booksDiv = $("books");
    booksDiv.innerHTML = renderBooksPage(books, page);
    setupBookClickHandlers(booksDiv);
    pumpCovers();

    $("page-input").value = page;
    history.replaceState({ page }, "", `?page=${page}`);
    window.scrollTo(0, 0);
    updateNavigationButtons();
    rearmSentinel();
}

/* ------------------------------------------------------------------ */
/* Infinite scroll                                                    */
/* ------------------------------------------------------------------ */
function prefetchPage(page) {
    if (page > window.totalPages || prefetch.has(page)) return;
    prefetch.set(
        page,
        fetchPage(page).then((books) => {
            if (!books) prefetch.delete(page); // allow retry
            return books;
        }),
    );
}

/** Append pages until currentPage is BUFFER_PAGES ahead of the visible page. */
async function loadNext() {
    if (navigating || loadingNext) return;

    loadingNext = true;
    const token = navToken;

    try {
        while (
            !navigating &&
            token === navToken &&
            currentPage < window.totalPages &&
            currentPage < getVisiblePage() + BUFFER_PAGES
        ) {
            const next = currentPage + 1;

            // Fire fetches for the whole window in parallel.
            for (let i = 0; i < BUFFER_PAGES; i++) prefetchPage(next + i);

            const books = await prefetch.get(next);
            prefetch.delete(next);

            if (token !== navToken || navigating) return;
            if (!books || books.length === 0) return;

            const temp = document.createElement("div");
            temp.innerHTML = renderBooksPage(books, next);
            const pageEl = temp.firstElementChild;

            $("books").appendChild(pageEl);
            setupBookClickHandlers(pageEl);

            currentPage = next;
            updateNavigationButtons();
            pumpCovers();
        }
    } finally {
        loadingNext = false;
    }
    rearmSentinel();
}

function setupInfiniteScrollSentinel() {
    sentinelObserver = new IntersectionObserver(
        (entries) => {
            if (entries[0].isIntersecting) loadNext();
        },
        { rootMargin: "1500px" },
    );
    sentinelObserver.observe($("load-more"));
}

/** Re-observe so the callback fires again if the sentinel is still in range. */
function rearmSentinel() {
    if (!sentinelObserver) return;
    const el = $("load-more");
    sentinelObserver.unobserve(el);
    sentinelObserver.observe(el);
}

/* ------------------------------------------------------------------ */
/* Scroll tracking                                                    */
/* ------------------------------------------------------------------ */

function setupScrollTracking() {
    let updateScheduled = false;
    let lastVisiblePage = null;

    function scheduleUpdate() {
        if (updateScheduled) return;
        updateScheduled = true;
        requestAnimationFrame(() => {
            updateScheduled = false;
            if (navigating) return; // don't fight an in-progress jump

            updatePageLabel();

            const visiblePage = getVisiblePage();
            if (visiblePage !== lastVisiblePage) {
                lastVisiblePage = visiblePage;
                pumpCovers();
                loadNext();
            }
        });
    }

    window.addEventListener("scroll", scheduleUpdate);
    window.addEventListener("keydown", (e) => {
        if (e.key === "Home" || e.key === "End") scheduleUpdate();
    });
}

const MAX_COVERS_INFLIGHT = 4; // leave headroom under the 6-per-host limit
const coversInflight = new Set();

/**
 * Start pending covers in DOM order (nearest the viewport first),
 * keeping at most MAX_COVERS_INFLIGHT requests open. Each finished
 * image pumps the queue again.
 */
function pumpCovers() {
    // Images removed by a page jump never fire load/error.
    for (const img of coversInflight) {
        if (!img.isConnected) coversInflight.delete(img);
    }

    const visible = getVisiblePage();

    for (const img of document.querySelectorAll(".book-cover[data-src]")) {
        if (coversInflight.size >= MAX_COVERS_INFLIGHT) break;

        const pageNumber = Number(img.closest(".page")?.dataset.page);
        if (pageNumber < visible - 1) continue; // behind the viewport, wait

        const src = img.dataset.src;
        delete img.dataset.src;
        coversInflight.add(img);

        const done = () => {
            coversInflight.delete(img);
            pumpCovers();
        };
        img.addEventListener("load", done, { once: true });
        img.addEventListener("error", done, { once: true });
        img.src = src;
    }
}

/** Always returns a valid page number. */
function getVisiblePage() {
    const pages = document.querySelectorAll(".page");
    if (pages.length === 0) return currentPage;

    const headerHeight = document.querySelector(".top-bar").offsetHeight;
    let current = parseInt(pages[0].dataset.page, 10);

    for (const page of pages) {
        const rect = page.getBoundingClientRect();
        if (rect.top <= headerHeight + 5) {
            current = parseInt(page.dataset.page, 10);
        } else {
            break;
        }
    }

    return Number.isFinite(current) ? current : currentPage;
}

function updatePageLabel() {
    const page = getVisiblePage();
    if (page === lastReplacedPage) return;

    lastReplacedPage = page;
    targetPage = page;
    $("page-input").value = page;
    history.replaceState({ page }, "", `?page=${page}`);
    updateNavigationButtons();
}

function updateNavigationButtons() {
    $("prev-page").disabled = targetPage <= 1;
    $("next-page").disabled = targetPage >= window.totalPages;
}

/* ------------------------------------------------------------------ */
/* Controls                                                           */
/* ------------------------------------------------------------------ */

const pageInput = $("page-input");

// Arrow keys / spinner / typing: debounced, only the final value loads.
pageInput.addEventListener("input", () => {
    const n = parseInt(pageInput.value, 10);
    if (Number.isNaN(n)) return; // mid-edit (empty field)
    requestPage(n, { syncInput: false });
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

$("next-page").addEventListener("click", () => requestPage(targetPage + 1));
$("prev-page").addEventListener("click", () => requestPage(targetPage - 1));
