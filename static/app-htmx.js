import { coverObserver, openBookModal, generateCoverText, enqueue } from "/static/shared.js";

let currentPage = 1;

// Update navigation state
function updateNavigation(page, totalPages) {
    currentPage = page;
    const pageInput = document.getElementById('page-input');
    if (pageInput) {
        pageInput.value = page;
    }
    
    const prevBtn = document.querySelector('button[aria-label="Previous page"]');
    const nextBtn = document.querySelector('button[aria-label="Next page"]');
    
    if (prevBtn) {
        prevBtn.disabled = page <= 1;
        prevBtn.setAttribute('hx-get', `/htmx-page?page=${Math.max(1, page - 1)}`);
    }
    
    if (nextBtn) {
        nextBtn.disabled = page >= totalPages;
        nextBtn.setAttribute('hx-get', `/htmx-page?page=${Math.min(totalPages, page + 1)}`);
    }
    
    console.log('[Navigation] Updated to page', page, 'of', totalPages);
}

// Initialize books in a container
function initializeBooksInContainer(container) {
    console.log('[Init] Initializing books in container', container);
    
    // Attach cover observers to images
    const images = container.querySelectorAll('.book-cover');
    console.log('[Init] Found', images.length, 'images to observe');
    
    let newImagesCount = 0;
    let alreadyAttachedCount = 0;
    
    images.forEach((img, index) => {
        if (!img.dataset.observerAttached) {
            newImagesCount++;
            console.log('[Init] Observing NEW image', index, 'UUID:', img.dataset.id, 'data-src:', img.dataset.src);
            coverObserver.observe(img);
            img.dataset.observerAttached = 'true';
            
            // Add error handler for fallback text covers
            img.addEventListener("error", () => {
                console.log('[Image] Error loading image, creating fallback for', img.dataset.id);
                const bookUuid = img.dataset.id;
                const bookEl = img.closest('.book');
                if (!bookEl) return;
                
                const bookTitle = bookEl.querySelector('h2')?.textContent || '';
                const bookAuthor = bookEl.querySelector('.book-author')?.textContent || '';
                
                const canvas = generateCoverText({
                    uuid: bookUuid,
                    title: bookTitle,
                    author_sort: bookAuthor
                });
                
                const wrapper = img.parentElement;
                img.remove();
                wrapper.appendChild(canvas);
            });
            
            // Manually trigger the enqueue if image is already visible
            const rect = img.getBoundingClientRect();
            const windowHeight = window.innerHeight;
            const threshold = windowHeight + 800;
            const isVisible = rect.top < threshold;
            console.log('[Init] Visibility check for image', index, '- rect.top:', rect.top, 'windowHeight:', windowHeight, 'threshold:', threshold, 'isVisible:', isVisible);
            if (isVisible) {
                console.log('[Init] Image', index, 'is already visible (top:', rect.top, '), enqueueing immediately');
                enqueue(img);
            } else {
                console.log('[Init] Image', index, 'is not yet visible (top:', rect.top, '), will wait for intersection');
            }
        } else {
            alreadyAttachedCount++;
            console.log('[Init] Image', index, 'already has observer attached, skipping');
        }
    });
    
    console.log('[Init] Summary: New images:', newImagesCount, 'Already attached:', alreadyAttachedCount);
    
    // Attach click handlers for book modals
    const bookLinks = container.querySelectorAll('.book-link');
    console.log('[Init] Found', bookLinks.length, 'book links to attach handlers');
    
    let newLinksCount = 0;
    let alreadyAttachedLinksCount = 0;
    
    bookLinks.forEach((link, index) => {
        if (!link.dataset.clickAttached) {
            newLinksCount++;
            console.log('[Init] Attaching click handler to NEW link', index, 'for UUID:', link.dataset.bookUuid);
            link.addEventListener('click', (e) => {
                e.preventDefault();
                const uuid = link.dataset.bookUuid;
                console.log('[Click] Opening modal for book:', uuid);
                if (uuid) {
                    openBookModal(uuid);
                }
            });
            link.dataset.clickAttached = 'true';
        } else {
            alreadyAttachedLinksCount++;
        }
    });
    
    console.log('[Init] Links summary: New links:', newLinksCount, 'Already attached:', alreadyAttachedLinksCount);
}

// Re-initialize after htmx swaps content
document.body.addEventListener('htmx:afterSwap', function(event) {
    console.log('[htmx:afterSwap] Event fired');
    console.log('[htmx:afterSwap] Target:', event.detail.target);
    console.log('[htmx:afterSwap] Target ID:', event.detail.target.id);
    
    // For infinite scroll (outerHTML), the new content replaces the trigger
    // For navigation (innerHTML to #books), the content is inside #books
    
    // Wait a brief moment for DOM to settle, then scan entire #books container
    setTimeout(() => {
        const booksContainer = document.getElementById('books');
        if (booksContainer) {
            console.log('[htmx:afterSwap] Scanning entire #books container for uninitialized content');
            initializeBooksInContainer(booksContainer);
        }
    }, 50);
    
    // Extract page number from visible page elements
    const booksContainer = document.getElementById('books');
    const pageElements = booksContainer?.querySelectorAll('.page') || [];
    console.log('[htmx:afterSwap] Found', pageElements.length, 'page elements');
    
    if (pageElements.length > 0) {
        // Get the last page number (most recently added)
        const lastPage = pageElements[pageElements.length - 1];
        const pageNum = parseInt(lastPage.dataset.page);
        if (!isNaN(pageNum)) {
            const totalPages = parseInt(document.getElementById('total-pages')?.textContent || '1');
            console.log('[htmx:afterSwap] Last page number:', pageNum);
            // Only update navigation for non-infinite-scroll operations
            if (pageElements.length === 1) {
                updateNavigation(pageNum, totalPages);
            }
        }
    }
});

// Update navigation when page changes (for next/prev/input)
document.body.addEventListener('htmx:beforeRequest', function(event) {
    console.log('[htmx:beforeRequest] Request to:', event.detail.requestConfig.path);
    
    // Extract page from request URL
    try {
        const url = new URL(event.detail.requestConfig.path, window.location.origin);
        const page = parseInt(url.searchParams.get('page') || '1');
        const totalPagesEl = document.getElementById('total-pages');
        const totalPages = parseInt(totalPagesEl?.textContent || '1');
        
        console.log('[htmx:beforeRequest] Page:', page, 'Total:', totalPages);
        
        // Only update if this is a navigation request (not infinite scroll)
        if (event.detail.target.id === 'books' || event.detail.elt.id === 'page-input') {
            updateNavigation(page, totalPages);
        }
    } catch (err) {
        console.error('[htmx:beforeRequest] Error processing request:', err);
    }
});

// Handle initial page load
window.addEventListener("DOMContentLoaded", () => {
    console.log('[DOMContentLoaded] Initializing application');
    
    // Scroll to top on initial load
    window.history.scrollRestoration = "manual";
    window.scrollTo(0, 0);
    
    const modal = document.getElementById("book-modal");
    const totalPagesEl = document.getElementById('total-pages');
    const pageInput = document.getElementById('page-input');
    
    const totalPages = parseInt(totalPagesEl?.textContent || '1');
    currentPage = parseInt(pageInput?.value || '1');
    
    console.log('[DOMContentLoaded] Initial page:', currentPage, 'Total pages:', totalPages);
    
    updateNavigation(currentPage, totalPages);

    if (modal) {
        const modalClose = document.getElementById("modal-close");
        if (modalClose) {
            modalClose.addEventListener("click", () => {
                console.log('[Modal] Close button clicked');
                modal.classList.remove("open");
            });
        }

        modal.addEventListener("click", (e) => {
            if (e.target === modal) {
                console.log('[Modal] Backdrop clicked');
                modal.classList.remove("open");
            }
        });
    }

    window.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && modal?.classList.contains("open")) {
            console.log('[Modal] Escape key pressed');
            modal.classList.remove("open");
        }
    });
    
    // Wait a bit for htmx to finish initial load, then initialize
    setTimeout(() => {
        const booksContainer = document.getElementById('books');
        if (booksContainer) {
            console.log('[DOMContentLoaded] Initializing books container after delay');
            initializeBooksInContainer(booksContainer);
        }
    }, 100);
});
