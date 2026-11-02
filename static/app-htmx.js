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

/**
 * Handle page load from htmx JSON response
 * Exposed globally for hx-on handlers
 */
window.handlePageLoad = function(event) {
    const xhr = event.detail.xhr;
    const books = JSON.parse(xhr.responseText);
    const pageNum = getCurrentPageFromUrl() || currentPage;
    
    // Abort old images
    abortImageQueue();
    
    // Transform JSON to HTML
    const html = renderBooksPage(books, pageNum);
    const booksDiv = document.getElementById("books");
    booksDiv.innerHTML = html;
    
    // Setup interactivity
    setupBookClickHandlers(booksDiv, openBookModal);
    setupCoverLazyLoading(booksDiv);
    
    // Update state
    currentPage = pageNum;
    document.getElementById("page-input").value = pageNum;
    updateNavigationButtons();
    
    // Update URL
    history.pushState({ page: pageNum }, "", `?page=${pageNum}`);
    
    // Scroll to top
    window.scrollTo(0, 0);
};

function getCurrentPageFromUrl() {
    const params = new URLSearchParams(window.location.search);
    return parseInt(params.get("page")) || null;
}

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
        setupBookClickHandlers(booksDiv, openBookModal);
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
        setupBookClickHandlers(pageEl, openBookModal);
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
 * Update navigation button states
 */
function updateNavigationButtons() {
    const visiblePage = getVisiblePage();
    document.getElementById("prev-page").disabled = visiblePage <= 1;
    document.getElementById("next-page").disabled = currentPage >= window.totalPages;
}

/**
 * Open book modal
 * @param {string} uuid - Book UUID
 */
function openBookModal(uuid) {
    const modal = document.getElementById("book-modal");
    const modalBody = document.getElementById("modal-body");
    
    // Show modal
    document.body.classList.add("no-scroll");
    modal.classList.add("open");
    modalBody.innerHTML = "Loading...";
    
    // Push history state
    history.pushState({ modal: uuid }, "");
    
    // Load book content
    fetch(`/book/get/${uuid}`)
        .then(r => r.text())
        .then(html => {
            modalBody.innerHTML = html;
            return loadFormats(uuid);
        })
        .catch(err => {
            console.error("Failed to load book:", err);
            modalBody.innerHTML = "Failed to load book details.";
        });
}

/**
 * Load book formats and create read buttons
 * @param {string} uuid - Book UUID
 */
async function loadFormats(uuid) {
    try {
        const res = await fetch(`/formats/${uuid}`);
        const formats = await res.json();
        
        if (!Array.isArray(formats)) return;
        
        const btnContainer = document.createElement("div");
        btnContainer.style.cssText = `
            display:flex;
            flex-direction:column;
            gap:8px;
            margin-top:1rem;
            width:100%;
        `;
        
        for (const format of formats) {
            let href = null;
            let label = null;
            
            if (format === "epub") {
                href = `/read?uuid=${uuid}`;
                label = "Read EPUB";
            } else if (format === "pdf") {
                href = `/read-pdf?uuid=${uuid}`;
                label = "Read PDF";
            }
            
            if (!label) continue;
            
            const btn = document.createElement("button");
            btn.textContent = label;
            btn.style.cssText = `height:44px; width:100%;`;
            btn.addEventListener("click", () => {
                window.location.href = href;
            });
            
            btnContainer.appendChild(btn);
        }
        
        const readButtons = document.querySelector("#modal-body #read-buttons");
        if (readButtons && btnContainer.children.length > 0) {
            readButtons.appendChild(btnContainer);
        }
    } catch (err) {
        console.error("Failed to load formats:", err);
    }
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
    history.back();
});

document.getElementById("book-modal").addEventListener("click", (e) => {
    if (e.target.id === "book-modal") {
        history.back();
    }
});

window.addEventListener("keydown", (e) => {
    const modal = document.getElementById("book-modal");
    if (e.key === "Escape" && modal.classList.contains("open")) {
        history.back();
    }
});

window.addEventListener("popstate", () => {
    closeModal();
});
