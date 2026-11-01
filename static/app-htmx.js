import { coverObserver, openBookModal, generateCoverText } from "/static/shared.js";

let currentPage = 1;

// Update navigation state
function updateNavigation(page, totalPages) {
    currentPage = page;
    document.getElementById('page-input').value = page;
    
    const prevBtn = document.querySelector('button[aria-label="Previous page"]');
    const nextBtn = document.querySelector('button[aria-label="Next page"]');
    
    if (prevBtn) {
        prevBtn.disabled = page <= 1;
        if (page > 1) {
            prevBtn.setAttribute('hx-get', `/htmx-page?page=${page - 1}`);
        }
    }
    
    if (nextBtn) {
        nextBtn.disabled = page >= totalPages;
        if (page < totalPages) {
            nextBtn.setAttribute('hx-get', `/htmx-page?page=${page + 1}`);
        }
    }
}

// Re-initialize observers and handlers after htmx swaps content
document.body.addEventListener('htmx:afterSwap', function(event) {
    // Re-attach cover observers to new images
    const newImages = event.detail.target.querySelectorAll('.book-cover');
    newImages.forEach(img => {
        coverObserver.observe(img);
        
        // Add error handler for fallback text covers
        img.addEventListener("error", () => {
            const bookUuid = img.dataset.id;
            const bookTitle = img.closest('.book').querySelector('h2').textContent;
            const bookAuthor = img.closest('.book').querySelector('.book-author').textContent;
            
            const canvas = generateCoverText({
                uuid: bookUuid,
                title: bookTitle,
                author_sort: bookAuthor
            });
            
            const wrapper = img.parentElement;
            img.remove();
            wrapper.appendChild(canvas);
        });
    });
    
    // Re-attach click handlers for book modals
    const bookLinks = event.detail.target.querySelectorAll('[data-book-uuid]');
    bookLinks.forEach(link => {
        link.addEventListener('click', (e) => {
            e.preventDefault();
            const uuid = link.dataset.bookUuid;
            openBookModal(uuid);
        });
    });
});

// Update navigation when page changes (for next/prev/input)
document.body.addEventListener('htmx:beforeRequest', function(event) {
    // Extract page from request URL
    const url = new URL(event.detail.requestConfig.path, window.location.origin);
    const page = parseInt(url.searchParams.get('page') || '1');
    const totalPagesEl = document.getElementById('total-pages');
    const totalPages = parseInt(totalPagesEl.textContent);
    
    // Only update if this is a navigation request (not infinite scroll)
    if (event.detail.target.id === 'books' || event.detail.elt.id === 'page-input') {
        updateNavigation(page, totalPages);
    }
});

// Handle initial page load
window.addEventListener("DOMContentLoaded", () => {
    const modal = document.getElementById("book-modal");
    const totalPages = parseInt(document.getElementById('total-pages').textContent);
    const pageInput = document.getElementById('page-input');
    currentPage = parseInt(pageInput.value);
    
    updateNavigation(currentPage, totalPages);

    document.getElementById("modal-close").addEventListener("click", () => {
        modal.classList.remove("open");
    });

    modal.addEventListener("click", (e) => {
        if (e.target === modal) modal.classList.remove("open");
    });

    window.addEventListener("keydown", (e) => {
        if (e.key === "Escape") modal.classList.remove("open");
    });
});
