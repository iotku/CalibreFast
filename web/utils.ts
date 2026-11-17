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
 * Hue of a book's placeholder cover (CSS --cover-hue). coverHue in book.go
 * computes the same for the book page: keep them in step.
 */
export function coverHue(book: Pick<Book, "uuid">): number {
    return hashString(book.uuid) % 360;
}

/**
 * Generate a color from a book's UUID
 * @param book - Book object with uuid property
 * @returns HSL color string
 */
export function colorFromBook(book: Pick<Book, "uuid">): string {
    return `hsl(${coverHue(book)}, 55%, 35%)`;
}

/**
 * Generate a text-based cover canvas for books without cover images
 * @param book - Book object with title and authors
 * @returns Canvas element with book info
 */
export function generateCoverText(book: Pick<Book, "uuid" | "title" | "authors">): HTMLCanvasElement {
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
    ctx.fillText(book.authors || "", 100, 260);

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

/** URL of a book's grid thumbnail. */
export function coverUrl(book: Pick<Book, "uuid" | "last_modified" | "series_card">): string {
    const coverOf = book.series_card?.cover ?? book.uuid; // a series: its first book's
    return `/cover-thumb/${encodeURIComponent(coverOf)}?t=${encodeURIComponent(book.last_modified)}`;
}

/** "Series · 5 books", a series card's banner and its Arrange row's line. */
export function seriesLabel(count: number): string {
    return `Series · ${count} ${count === 1 ? "book" : "books"}`;
}

/**
 * A shelf's whole series (Book.series_card): the first book's cover under
 * a "Series · N books" banner, stacked, linking to the series page. No
 * data-uuid, so the reading marks, cover buttons, selection and the popup's
 * next/previous all pass it by.
 */
function renderSeriesCard(book: Book, s: NonNullable<Book["series_card"]>, coverReady: boolean): string {
    const name = escapeHtml(s.name || book.title);
    const author = escapeHtml(book.authors || "");
    const cover = escapeHtml(coverUrl(book));
    const img = coverReady
        ? `<img class="book-cover loaded" src="${cover}" alt="" />`
        : `<img class="book-cover" data-src="${cover}" alt="" fetchpriority="low" />`;
    const label = escapeHtml(seriesLabel(s.count));
    return `<div class="book series-card" data-series="${name}">
    <a href="${escapeHtml(s.href)}" class="book-link series-link" aria-label="${name}, ${label}">
        <div class="cover-wrapper" style="--cover-hue:${coverHue(book)}">${img}<span class="series-banner" aria-hidden="true">${label}</span></div>
    </a>
    <div class="book-info">
        <h2>${name}</h2>
        <div class="book-author">${author}</div>
        <div class="formats"></div>
    </div>
</div>`;
}

/**
 * Render a single book card HTML
 * Plain link to #book/<uuid>; modal logic lives in setupModal(). htmx only
 * preloads the book on hover (hx-trigger="none": the click is the hash
 * change, and modal.ts's loadBook uses the preloaded response).
 *
 * The cover waits in data-src for the grid's download queue, unless
 * coverReady says the browser already has it: then it's shown at once,
 * without the fade-in.
 */
export function renderBookCard(book: Book, coverReady = false): string {
    if (book.series_card) return renderSeriesCard(book, book.series_card, coverReady);
    const title = escapeHtml(book.title || "Untitled");
    const author = escapeHtml(book.authors || "Unknown Author");
    const uuid = escapeHtml(book.uuid);
    const hue = coverHue(book);
    const cover = escapeHtml(coverUrl(book));
    const img = coverReady
        ? `<img class="book-cover loaded" src="${cover}" data-id="${uuid}" />`
        : `<img class="book-cover" data-src="${cover}" data-id="${uuid}" fetchpriority="low" />`;

    return `<div class="book" data-uuid="${uuid}">
    <a href="#book/${uuid}" class="book-link"
       hx-get="/book/get/${uuid}"
       hx-trigger="none"
       hx-preload="mouseover"
    >
        <div class="cover-wrapper" style="--cover-hue:${hue}">${img}</div>
    </a>
    <div class="book-info">
        <h2>${title}</h2>
        <div class="book-author">${author}</div>
        <div class="formats"></div>
    </div>
</div>`;
}

/** Covers fade in once loaded; one that fails leaves the card's title and author showing. */
export function setupCoverStates(container: HTMLElement): void {
    container.addEventListener(
        "load",
        (e) => {
            if (e.target instanceof HTMLImageElement && e.target.matches(".book-cover"))
                e.target.classList.add("loaded");
        },
        true,
    );

    container.addEventListener(
        "error",
        (e) => {
            const img = e.target;
            if (!(img instanceof HTMLImageElement) || !img.matches(".book-cover")) return;
            img.parentElement?.classList.add("no-cover"); // title/author info stays visible
            img.remove();
        },
        true,
    );
}
