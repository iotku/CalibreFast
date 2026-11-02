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
        <div class="cover-wrapper" style="background:${coverColor}">
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
 * Setup cover image lazy loading with IntersectionObserver
 * @param {HTMLElement} container - Container element with book cards
 */
export function setupCoverLazyLoading(container) {
    const images = container.querySelectorAll('.book-cover[data-src]');
    
    const observer = new IntersectionObserver(
        (entries) => {
            entries.forEach(entry => {
                if (entry.isIntersecting) {
                    const img = entry.target;
                    const src = img.dataset.src;
                    
                    if (src && !img.src) {
                        fetch(src)
                            .then(r => r.blob())
                            .then(blob => {
                                img.src = URL.createObjectURL(blob);
                                img.onload = () => img.classList.add('loaded');
                            })
                            .catch(() => {
                                // On error, generate text-based cover
                                const bookCard = img.closest('.book');
                                if (bookCard) {
                                    const wrapper = img.parentElement;
                                    const bookData = {
                                        title: bookCard.querySelector('h2').textContent,
                                        author_sort: bookCard.querySelector('.book-author').textContent,
                                        uuid: img.dataset.id
                                    };
                                    const canvas = generateCoverText(bookData);
                                    img.remove();
                                    wrapper.appendChild(canvas);
                                }
                            });
                        
                        observer.unobserve(img);
                    }
                }
            });
        },
        { rootMargin: "800px" }
    );
    
    images.forEach(img => observer.observe(img));
}
