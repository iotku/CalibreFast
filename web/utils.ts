/**
 * CalibreFast Utilities
 * Functions for transforming book data and generating UI elements
 */

import type { Book } from "./types.js";

/**
 * Generate a hash from a string
 * @param str - String to hash
 * @returns Hash value
 */
export function hashString(str: string): number {
    let h = 0;
    for (let i = 0; i < str.length; i++) {
        h = (h << 5) - h + str.charCodeAt(i);
        h |= 0;
    }
    return Math.abs(h);
}

/**
 * Generate a color from a book's UUID
 * @param book - Book object with uuid property
 * @returns HSL color string
 */
export function colorFromBook(book: Pick<Book, "uuid">): string {
    const h = hashString(book.uuid);
    return `hsl(${h % 360}, 55%, 35%)`;
}

/**
 * Generate a text-based cover canvas for books without cover images
 * @param book - Book object with title and author_sort
 * @returns Canvas element with book info
 */
export function generateCoverText(book: Pick<Book, "uuid" | "title" | "author_sort">): HTMLCanvasElement {
    const canvas = document.createElement("canvas");
    canvas.className = "cover-text";
    canvas.width = 200;
    canvas.height = 300;

    const ctx = canvas.getContext("2d");
    if (!ctx) return canvas;
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
function wrapText(ctx: CanvasRenderingContext2D, text: string, x: number, y: number, maxWidth: number, lineHeight: number): void {
    const words = (text || "").split(" ");
    let line = "";

    for (let i = 0; i < words.length; i++) {
        const test = line + words[i] + " ";
        const width = ctx.measureText(test).width;

        if (width > maxWidth && i > 0) {
            ctx.fillText(line, x, y);
            line = `${words[i]} `;
            y += lineHeight;
        } else {
            line = test;
        }
    }
    ctx.fillText(line, x, y);
}

/**
 * Escape HTML special characters
 * @param str - String to escape
 * @returns Escaped string
 */
export function escapeHtml(str: string): string {
    const div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
}

/**
 * Render a single book card HTML
 * Plain link to #book/<uuid>; modal logic lives in setupModal().
 * @param book - Book object
 * @returns HTML string for book card
 */
export function renderBookCard(book: Book): string {
    const title = escapeHtml(book.title || "Untitled");
    const author = escapeHtml(book.author_sort || "Unknown Author");
    const uuid = escapeHtml(book.uuid);
    const coverColor = colorFromBook(book);

    return `
<div class="book" >
    <a href="#book/${uuid}" class="book-link"
       hx-get="/book/get/${uuid}"
       hx-target="#modal-body"
       hx-swap="innerHTML"
       hx-preload="mouseover"
    >
        <div class="cover-wrapper" style="background:${coverColor}">
                <img class="book-cover"
                    data-src="/cover-thumb/${uuid}?t=${book.last_modified}"
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
 * @param books - Array of book objects
 * @param pageNumber - Page number
 * @returns HTML string for page container with books
 */
export function renderBooksPage(books: Book[], pageNumber: number): string {
    if (!books || !Array.isArray(books) || books.length === 0) {
        return "";
    }

    const booksHtml = books.map((book) => renderBookCard(book)).join("\n");

    return `
<div class="page" data-page="${pageNumber}">
    ${booksHtml}
</div>`;
}
