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

    ctx.fillStyle = colorFromBook(book);
    ctx.fillRect(0, 0, 200, 300);

    ctx.fillStyle = "white";
    ctx.textAlign = "center";

    ctx.font = "bold 16px sans-serif";
    wrapText(ctx, book.title, 100, 120, 180, 20);

    ctx.font = "12px sans-serif";
    ctx.fillText(book.author_sort || "", 100, 260);

    return canvas;
}

/**
 * Wrap text to fit within a maximum width
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
    const div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
}

/**
 * Render a single book card HTML
 * Plain link to #book/<uuid>; modal logic lives in setupModal().
 * @param {Object} book - Book object
 * @returns {string} HTML string for book card
 */
export function renderBookCard(book) {
    const title = escapeHtml(book.title || "Untitled");
    const author = escapeHtml(book.author_sort || "Unknown Author");
    const uuid = escapeHtml(book.uuid);

    return `
<div class="book" >
<a href="#book/${uuid}" class="book-link"
   hx-get="/book/get/${uuid}"
   hx-target="#modal-body"
   hx-swap="innerHTML"
   hx-preload="mouseover"
>
        <div class="cover-wrapper">
            <img class="book-cover"
                 src="/cover-thumb/${uuid}?t=${book.last_modified}"
                 data-id="${uuid}"
                 loading="lazy"
                 fetchpriority="low"
                onload="this.classList.add('loaded')" />
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
        return "";
    }

    const booksHtml = books.map((book) => renderBookCard(book)).join("\n");

    return `
<div class="page" data-page="${pageNumber}">
    ${booksHtml}
</div>`;
}
