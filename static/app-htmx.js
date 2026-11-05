/**
 * CalibreFast Main Library - htmx Implementation
 * Infinite scroll with JSON transformation
 */

import { renderBooksPage } from "./utils.js";
import { setupBookClickHandlers, HASH_PREFIX } from "./modal.js";

const NAV_DEBOUNCE_MS = 0;

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
        setupCoverLazyLoading($("books")); // resumes the queue, re-queues covers
        return;
    }

    currentPage = page;
    targetPage = page;
    lastReplacedPage = page;
    loadingNext = false;

    updateCoverRequests();

    const booksDiv = $("books");
    booksDiv.innerHTML = renderBooksPage(books, page);
    setupBookClickHandlers(booksDiv);

    $("page-input").value = page;
    history.replaceState({ page }, "", `?page=${page}`);
    window.scrollTo(0, 0);
    updateNavigationButtons();
    rearmSentinel();
}

/* ------------------------------------------------------------------ */
/* Infinite scroll                                                    */
/* ------------------------------------------------------------------ */

async function loadNext() {
    if (navigating || loadingNext) return;
    if (currentPage >= window.totalPages) return;

    loadingNext = true;
    const token = navToken;
    const next = currentPage + 1;

    try {
        const books = await fetchPage(next);
        if (token !== navToken || navigating || !books || books.length === 0)
            return;

        const temp = document.createElement("div");
        temp.innerHTML = renderBooksPage(books, next);
        const pageEl = temp.firstElementChild;

        $("books").appendChild(pageEl);
        setupBookClickHandlers(pageEl);

        currentPage = next;
        updateNavigationButtons();
        updateCoverRequests();
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
                updateCoverRequests();
            }
        });
    }

    window.addEventListener("scroll", scheduleUpdate);
    window.addEventListener("keydown", (e) => {
        if (e.key === "Home" || e.key === "End") scheduleUpdate();
    });
}

/**
 * Cancel cover image requests more than two pages behind
 * the current viewport. Restore their requests when they
 * come back within range.
 */
function updateCoverRequests() {
    const visiblePage = getVisiblePage();
    const oldestAllowedPage = visiblePage - 1;

    document.querySelectorAll(".page[data-page]").forEach((page) => {
        const pageNumber = Number(page.dataset.page);
        const images = page.querySelectorAll(".book-cover");

        images.forEach((img) => {
            // Already successfully loaded; nothing to do.
            if (img.classList.contains("loaded")) return;

            if (pageNumber < oldestAllowedPage) {
                // Preserve the original URL before removing src.
                if (img.hasAttribute("src")) {
                    img.dataset.coverSrc = img.getAttribute("src");
                    img.removeAttribute("src");
                }
            } else if (img.dataset.coverSrc) {
                // Restore the request when scrolling back.
                img.setAttribute("src", img.dataset.coverSrc);
                delete img.dataset.coverSrc;
            }
        });
    });
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
