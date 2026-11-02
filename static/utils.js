/**
 * CalibreFast Utilities
 * Functions for transforming book data and generating UI elements
 */

/**
 * Generate a hash from a string
 * @param {string} str - String to hash
 * @returns {number} Hash value
 */
export function hashString(str) {
    let h = 0;
    for (let i = 0; i < str.length; i++) {
        h = (h << 5) - h + str.charCodeAt(i);
        h |= 0;
    }
    return Math.abs(h);
}

/**
 * Generate a color from a book's UUID
 * @param {Object} book - Book object with uuid property
 * @returns {string} HSL color string
 */
export function colorFromBook(book) {
    const h = hashString(book.uuid);
    return `hsl(${h % 360}, 55%, 35%)`;
}

/**
 * Generate a text-based cover canvas for books without cover images
 * @param {Object} book - Book object with title and author_sort
 * @returns {HTMLCanvasElement} Canvas element with book info
 */
export function generateCoverText(book) {
    const canvas = document.createElement("canvas");
    canvas.className = "cover-text";
    canvas.width = 200;
    canvas.height = 300;

    const ctx = canvas.getContext("2d");
    const scale = Math.min(window.devicePixelRatio || 1, 3);
    canvas.width *= scale;
    canvas.height *= scale;
    ctx.scale(scale, scale);

    // Background filled with book color
    ctx.fillStyle = colorFromBook(book);
    ctx.fillRect(0, 0, 200, 300);

    ctx.fillStyle = "white";
    ctx.textAlign = "center";

    // Title
    ctx.font = "bold 16px sans-serif";
    wrapText(ctx, book.title, 100, 120, 180, 20);

    // Author
    ctx.font = "12px sans-serif";
    ctx.fillText(book.author_sort || "", 100, 260);

    return canvas;
}

/**
 * Wrap text to fit within a maximum width
 * @param {CanvasRenderingContext2D} ctx - Canvas context
 * @param {string} text - Text to wrap
 * @param {number} x - X position
 * @param {number} y - Y position
 * @param {number} maxWidth - Maximum width
 * @param {number} lineHeight - Line height
 */
function wrapText(ctx, text, x, y, maxWidth, lineHeight) {
    const words = (text || "").split(" ");
    let line = "";

    for (let i = 0; i < words.length; i++) {
        const test = line + words[i] + " ";
        const width = ctx.measureText(test).width;

        if (width > maxWidth && i > 0) {
            ctx.fillText(line, x, y);
            line = words[i] + " ";
            y += lineHeight;
        } else {
            line = test;
        }
    }
    ctx.fillText(line, x, y);
}

/**
 * Escape HTML special characters
 * @param {string} str - String to escape
 * @returns {string} Escaped string
 */
export function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

/**
 * Render a single book card HTML
 * @param {Object} book - Book object
 * @returns {string} HTML string for book card
 */
export function renderBookCard(book) {
    const title = escapeHtml(book.title || "Untitled");
    const author = escapeHtml(book.author_sort || "Unknown Author");
    const coverColor = colorFromBook(book);
    const uuid = escapeHtml(book.uuid);

    return `
<div class="book">
    <a href="#" data-book-uuid="${uuid}" class="book-link">
        <div class="cover-wrapper" style="background:${coverColor}" 
             hx-get="/cover-thumb/${uuid}" 
             hx-trigger="revealed, mouseenter once"
             hx-swap="none"
             hx-on::after:request="window.handleCoverPreload(this, event)">
            <img class="book-cover" 
                 data-src="/cover-thumb/${uuid}" 
                 data-id="${uuid}"
                 fetchpriority="low" />
        </div>
    </a>
    <div class="book-info">
        <h2>${title}</h2>
        <div class="book-author">${author}</div>
        <div class="formats"></div>
    </div>
</div>`;
}

/**
 * Render a page of books
 * @param {Array} books - Array of book objects
 * @param {number} pageNumber - Page number
 * @returns {string} HTML string for page container with books
 */
export function renderBooksPage(books, pageNumber) {
    if (!books || !Array.isArray(books) || books.length === 0) {
        return '';
    }

    const booksHtml = books.map(book => renderBookCard(book)).join('\n');
    
    return `
<div class="page" data-page="${pageNumber}">
    ${booksHtml}
</div>`;
}

/**
 * Setup book card click handlers for modal opening
 * Attach this after inserting book cards into DOM
 * @param {HTMLElement} container - Container element with book cards
 * @param {Function} openModalCallback - Callback function(uuid)
 */
export function setupBookClickHandlers(container, openModalCallback) {
    const bookLinks = container.querySelectorAll('.book-link');
    bookLinks.forEach(link => {
        link.addEventListener('click', (e) => {
            e.preventDefault();
            const uuid = link.dataset.bookUuid;
            if (uuid && openModalCallback) {
                openModalCallback(uuid);
            }
        });
    });
}

/**
 * Image Loading Queue Manager
 * Prioritizes visible images and manages concurrent requests
 */
class ImageQueue {
    constructor(maxConcurrent = 8) {
        this.maxConcurrent = maxConcurrent;
        this.queue = []; // Array of {img, priority, generation}
        this.inQueue = new Set(); // Set of img elements
        this.active = 0; // Number of active requests
        this.generation = 0; // Increments when queue is aborted
        this.abortController = new AbortController();
    }

    /**
     * Abort all pending and active requests
     */
    abort() {
        this.generation++;
        this.abortController.abort();
        this.abortController = new AbortController();
        this.queue = [];
        this.inQueue.clear();
    }

    /**
     * Add image to queue with priority
     * Higher priority = loads first
     * @param {HTMLImageElement} img - Image element
     * @param {number} priority - Priority (higher = sooner)
     */
    enqueue(img, priority = 0) {
        if (this.inQueue.has(img)) {
            // Update priority if already queued
            const item = this.queue.find(q => q.img === img);
            if (item && priority > item.priority) {
                item.priority = priority;
                // Re-sort queue
                this.queue.sort((a, b) => b.priority - a.priority);
            }
            return;
        }

        this.inQueue.add(img);
        this.queue.push({ img, priority, generation: this.generation });
        
        // Sort by priority (highest first)
        this.queue.sort((a, b) => b.priority - a.priority);
        
        this.processQueue();
    }

    /**
     * Process queued image loads
     */
    processQueue() {
        const signal = this.abortController.signal;
        const generation = this.generation;

        while (this.active < this.maxConcurrent && this.queue.length > 0) {
            const item = this.queue.shift();
            const { img } = item;
            
            this.inQueue.delete(img);
            this.active++;

            const src = img.dataset.src;
            if (!src) {
                this.active--;
                continue;
            }

            fetch(src, { signal })
                .then(r => r.blob())
                .then(blob => {
                    if (generation !== this.generation) return; // Aborted
                    
                    img.src = URL.createObjectURL(blob);
                    img.onload = () => img.classList.add('loaded');
                })
                .catch((err) => {
                    if (err.name === 'AbortError') return;
                    if (generation !== this.generation) return;
                    
                    // On error, generate text-based cover
                    const bookCard = img.closest('.book');
                    if (bookCard) {
                        const wrapper = img.parentElement;
                        const bookData = {
                            title: bookCard.querySelector('h2')?.textContent || 'Unknown',
                            author_sort: bookCard.querySelector('.book-author')?.textContent || '',
                            uuid: img.dataset.id
                        };
                        const canvas = generateCoverText(bookData);
                        img.remove();
                        wrapper.appendChild(canvas);
                    }
                })
                .finally(() => {
                    this.active--;
                    if (generation === this.generation) {
                        this.processQueue();
                    }
                });
        }
    }

    /**
     * Remove images that are far from viewport
     * @param {number} visiblePage - Currently visible page number
     * @param {number} keepRange - Pages to keep around visible page
     */
    cancelFarImages(visiblePage, keepRange = 2) {
        // Remove items from queue that are too far away
        const oldLength = this.queue.length;
        this.queue = this.queue.filter(item => {
            const img = item.img;
            const page = img.closest('.page');
            if (!page) return true; // Keep if we can't determine page
            
            const pageNum = parseInt(page.dataset.page, 10);
            if (isNaN(pageNum)) return true;
            
            const distance = Math.abs(pageNum - visiblePage);
            if (distance > keepRange) {
                this.inQueue.delete(img);
                return false; // Remove from queue
            }
            return true; // Keep in queue
        });
        
        if (oldLength !== this.queue.length) {
            // Re-sort after filtering
            this.queue.sort((a, b) => b.priority - a.priority);
        }
    }
}

// Global image queue instance
const imageQueue = new ImageQueue(8);

/**
 * Setup cover image lazy loading with IntersectionObserver
 * Uses priority queue to load visible images first
 * @param {HTMLElement} container - Container element with book cards
 */
export function setupCoverLazyLoading(container) {
    const images = container.querySelectorAll('.book-cover[data-src]');
    
    const observer = new IntersectionObserver(
        (entries) => {
            entries.forEach(entry => {
                const img = entry.target;
                
                if (entry.isIntersecting) {
                    // Image is visible - high priority
                    if (!img.src && img.dataset.src) {
                        imageQueue.enqueue(img, 100);
                    }
                } else if (entry.intersectionRatio === 0) {
                    // Image is not visible at all - low priority
                    if (!img.src && img.dataset.src) {
                        imageQueue.enqueue(img, 1);
                    }
                }
            });
        },
        { 
            rootMargin: "800px",
            threshold: [0, 0.1] // Detect when entering/leaving viewport
        }
    );
    
    images.forEach(img => {
        observer.observe(img);
        
        // Check if already in viewport
        const rect = img.getBoundingClientRect();
        const inViewport = rect.top < window.innerHeight && rect.bottom > 0;
        
        if (inViewport && !img.src && img.dataset.src) {
            // Immediately queue visible images with high priority
            imageQueue.enqueue(img, 100);
        }
    });
}

/**
 * Cancel loading of images far from current viewport
 * Call this when scrolling to prioritize nearby images
 * @param {number} visiblePage - Currently visible page number
 * @param {number} keepRange - Pages to keep around visible page (default: 2)
 */
export function cancelFarImages(visiblePage, keepRange = 2) {
    imageQueue.cancelFarImages(visiblePage, keepRange);
}

/**
 * Abort all pending image loads
 * Use when navigating away or clearing content
 */
export function abortImageQueue() {
    imageQueue.abort();
}

/**
 * Handle cover preload from htmx
 * Exposed globally for hx-on handlers
 */
window.handleCoverPreload = function(wrapper, event) {
    const img = wrapper.querySelector('.book-cover');
    if (!img || img.src) return; // Already loaded
    
    const xhr = event.detail.xhr;
    if (!xhr || xhr.status !== 200) return;
    
    // Get blob from response
    xhr.responseType = 'blob';
    const blob = xhr.response;
    
    // Create object URL and set image
    img.src = URL.createObjectURL(blob);
    img.onload = () => img.classList.add('loaded');
};
