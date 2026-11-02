/**
 * CalibreFast Main Library - htmx Implementation
 * Infinite scroll with JSON transformation
 */

import {
    renderBooksPage,
    setupBookClickHandlers,
    setupCoverLazyLoading,
    cancelFarImages,
    abortImageQueue
} from "./utils.js";

// Global state
window.totalPages = null;
let currentPage = 1;
let isInitialLoad = true;

// Initialize on DOM load
window.addEventListener("DOMContentLoaded", async () => {
    // Load library info
    await loadLibraryInfo();
    
    // Get initial page from URL
    const params = new URLSearchParams(window.location.search);
    const urlPage = parseInt(params.get("page") || "1", 10);
    currentPage = Math.max(1, Math.min(urlPage, window.totalPages || urlPage));
    
    // Disable scroll restoration (we manage it ourselves)
    window.history.scrollRestoration = "manual";
    window.scrollTo(0, 0);
    
    // Load initial page
    await loadInitialPage();
    
    // Setup infinite scroll sentinel
    setupInfiniteScrollSentinel();
    
    // Setup page label updates on scroll
    setupScrollTracking();
});

/**
 * Load library metadata (total pages)
 */
async function loadLibraryInfo() {
    try {
        const res = await fetch("/library-info");
        const info = await res.json();
        window.totalPages = info.total_pages;
        
        document.getElementById("total-pages").textContent = window.totalPages;
        updateNavigationButtons();
    } catch (err) {
        console.error("Failed to load library info:", err);
    }
}

/**
 * Load the initial page of books
 */
async function loadInitialPage() {
    try {
        const books = await fetchPage(currentPage);
        if (!books || books.length === 0) {
            return;
        }
        
        const html = renderBooksPage(books, currentPage);
        document.getElementById("books").innerHTML = html;
        
        // Setup interactivity
        const booksDiv = document.getElementById("books");
        setupBookClickHandlers(booksDiv);
        setupCoverLazyLoading(booksDiv);
        
        // Update UI
        document.getElementById("page-input").value = currentPage;
        updateNavigationButtons();
        
        isInitialLoad = false;
    } catch (err) {
        console.error("Failed to load initial page:", err);
    }
}

/**
 * Fetch a page of books from JSON endpoint
 * @param {number} page - Page number to fetch
 * @returns {Promise<Array>} Array of book objects
 */
async function fetchPage(page) {
    try {
        const res = await fetch(`/pages/page${page}.json`);
        if (!res.ok) return null;
        return await res.json();
    } catch (err) {
        console.error(`Failed to fetch page ${page}:`, err);
        return null;
    }
}

/**
 * Setup infinite scroll with revealed trigger
 * Creates a sentinel element that loads next page when visible
 */
function setupInfiniteScrollSentinel() {
    const loadMore = document.getElementById("load-more");
    
    // Use htmx's revealed trigger via JavaScript API
    // This fires once when element enters viewport
    htmx.on(loadMore, "intersect", async (evt) => {
        if (currentPage >= window.totalPages) {
            return; // No more pages
        }
        
        const nextPage = currentPage + 1;
        const books = await fetchPage(nextPage);
        
        if (!books || books.length === 0) {
            return;
        }
        
        // Render and append new page
        const html = renderBooksPage(books, nextPage);
        const temp = document.createElement("div");
        temp.innerHTML = html;
        const pageEl = temp.firstElementChild;
        
        const booksDiv = document.getElementById("books");
        booksDiv.appendChild(pageEl);
        
        // Setup interactivity for new content
        setupBookClickHandlers(pageEl);
        setupCoverLazyLoading(pageEl);
        
        currentPage = nextPage;
        updateNavigationButtons();
    });
    
    // Start observing
    const observer = new IntersectionObserver(
        (entries) => {
            if (entries[0].isIntersecting) {
                htmx.trigger(loadMore, "intersect");
            }
        },
        { rootMargin: "1500px" }
    );
    
    observer.observe(loadMore);
}

/**
 * Setup scroll tracking to update page number in URL
 */
function setupScrollTracking() {
    let updateScheduled = false;
    let lastVisiblePage = null;
    
    function scheduleUpdate() {
        if (updateScheduled) return;
        updateScheduled = true;
        requestAnimationFrame(() => {
            updateScheduled = false;
            updatePageLabel();
            
            // Cancel far images on scroll
            const visiblePage = getVisiblePage();
            if (visiblePage !== lastVisiblePage) {
                lastVisiblePage = visiblePage;
                cancelFarImages(visiblePage, 2); // Keep 2 pages on each side
            }
        });
    }
    
    window.addEventListener("scroll", scheduleUpdate);
    window.addEventListener("keydown", (e) => {
        if (e.key === "Home" || e.key === "End") {
            scheduleUpdate();
        }
    });
}

/**
 * Get the currently visible page number
 * @returns {number} Visible page number
 */
function getVisiblePage() {
    const pages = document.querySelectorAll(".page");
    const headerHeight = document.querySelector(".top-bar").offsetHeight;
    let current = null;
    
    for (const page of pages) {
        const rect = page.getBoundingClientRect();
        if (rect.top <= headerHeight + 5) {
            current = parseInt(page.dataset.page, 10);
        } else {
            break;
        }
    }
    
    return current ?? parseInt(document.querySelector(".page")?.dataset.page, 10) ?? 1;
}

/**
 * Update page label and URL
 */
let lastReplacedPage = null;
function updatePageLabel() {
    const page = getVisiblePage();
    if (isNaN(page) || page === lastReplacedPage) return;
    
    lastReplacedPage = page;
    document.getElementById("page-input").value = page;
    history.replaceState({ page }, "", `?page=${page}`);
    updateNavigationButtons();
}

/**
 * Close modal
 */
function closeModal() {
    const modal = document.getElementById("book-modal");
    document.body.classList.remove("no-scroll");
    modal.classList.remove("open");
}

// Modal event handlers
document.getElementById("modal-close").addEventListener("click", () => {
    closeModal();
    // Only go back if we're on a book URL
    if (window.location.pathname.includes('/book/get/')) {
        history.back();
    }
});

document.getElementById("book-modal").addEventListener("click", (e) => {
    if (e.target.id === "book-modal") {
        closeModal();
        if (window.location.pathname.includes('/book/get/')) {
            history.back();
        }
    }
});

window.addEventListener("keydown", (e) => {
    const modal = document.getElementById("book-modal");
    if (e.key === "Escape" && modal.classList.contains("open")) {
        closeModal();
        if (window.location.pathname.includes('/book/get/')) {
            history.back();
        }
    }
});

window.addEventListener("popstate", (e) => {
    // Close modal if it's open when back button is pressed
    const modal = document.getElementById("book-modal");
    if (modal.classList.contains("open")) {
        closeModal();
    }
});

/**
 * Update navigation button states
 */
function updateNavigationButtons() {
    const visiblePage = getVisiblePage();
    document.getElementById("prev-page").disabled = visiblePage <= 1;
    document.getElementById("next-page").disabled = currentPage >= window.totalPages;
}

// Navigation controls
document.getElementById("page-input").addEventListener("change", async (e) => {
    let target = parseInt(e.target.value, 10);
    if (isNaN(target)) return;
    
    target = Math.max(1, Math.min(target, window.totalPages));
    e.target.value = target;
    
    // Abort pending image loads
    abortImageQueue();
    
    // Clear and reload
    document.getElementById("books").innerHTML = "";
    currentPage = target;
    
    await loadInitialPage();
    window.scrollTo(0, 0);
});

document.getElementById("next-page").addEventListener("click", async () => {
    const page = getVisiblePage();
    if (page >= window.totalPages) return;
    
    // Abort pending image loads
    abortImageQueue();
    
    document.getElementById("books").innerHTML = "";
    currentPage = page + 1;
    
    await loadInitialPage();
    window.scrollTo(0, 0);
});

document.getElementById("prev-page").addEventListener("click", async () => {
    const page = getVisiblePage();
    if (page <= 1) return;
    
    // Abort pending image loads
    abortImageQueue();
    
    document.getElementById("books").innerHTML = "";
    currentPage = page - 1;
    
    await loadInitialPage();
    window.scrollTo(0, 0);
});
