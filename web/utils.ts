/**
 * CalibreFast Utilities
 * Functions for transforming book data and generating UI elements
 */

import { el, safeUrl } from "./dom.js";
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

const HTML_ESCAPES: Record<string, string> = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };

/**
 * Escape a string for HTML text or a quoted attribute value. Quotes are
 * escaped too: a name with " in it mustn't end an attribute early.
 */
export function escapeHtml(str: string): string {
    return str.replace(/[&<>"']/g, (c) => HTML_ESCAPES[c]!);
}

/**
 * fn, run only once calls have paused for ms (trailing edge), with the last
 * call's arguments. cancel() drops a run still waiting.
 */
export function debounce<A extends unknown[]>(fn: (...args: A) => void, ms: number): ((...args: A) => void) & { cancel(): void } {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const run = (...args: A): void => {
        clearTimeout(timer);
        timer = setTimeout(() => fn(...args), ms);
    };
    return Object.assign(run, { cancel: () => clearTimeout(timer) });
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

/** A card's cover <img>: shown at once when coverReady, else waiting in data-src for the download queue. */
function coverImg(src: string, coverReady: boolean, uuid?: string): HTMLImageElement {
    const img = el("img", { className: coverReady ? "book-cover loaded" : "book-cover" });
    if (coverReady) img.src = src;
    else {
        img.dataset["src"] = src;
        img.setAttribute("fetchpriority", "low");
    }
    if (uuid !== undefined) img.dataset["id"] = uuid;
    else img.alt = "";
    return img;
}

/** A card's cover box: --cover-hue, then the image (if any) and a banner. */
function coverWrapper(book: Pick<Book, "uuid">, ...children: (Node | string)[]): HTMLDivElement {
    const w = el("div", { className: "cover-wrapper" }, ...children);
    w.style.setProperty("--cover-hue", String(coverHue(book)));
    return w;
}

/** A card's title, author and (empty) formats under the cover. */
function bookInfo(title: string, author: string): HTMLDivElement {
    return el(
        "div",
        { className: "book-info" },
        el("h2", {}, title),
        el("div", { className: "book-author" }, author),
        el("div", { className: "formats" }),
    );
}

/**
 * A shelf's whole series (Book.series_card): the first book's cover under
 * a "Series · N books" banner, stacked, linking to the series page. No
 * data-uuid, so the reading marks, cover buttons and selection pass it by;
 * data-series-key (its "series:<name>" key) is what the popup's arrows step
 * onto (modal.ts).
 */
function seriesCard(book: Book, s: NonNullable<Book["series_card"]>, coverReady: boolean): HTMLElement {
    const name = s.name || book.title;
    const label = seriesLabel(s.count);
    const link = el("a", { href: safeUrl(s.href), className: "book-link series-link" });
    link.setAttribute("aria-label", `${name}, ${label}`);
    const banner = el("span", { className: "series-banner" }, label);
    banner.setAttribute("aria-hidden", "true");
    link.append(coverWrapper(book, coverImg(coverUrl(book), coverReady), banner));
    const card = el("div", { className: "book series-card" }, link, bookInfo(name, book.authors || ""));
    card.dataset["series"] = name;
    card.dataset["seriesKey"] = book.uuid;
    return card;
}

/**
 * A book not in the library yet (Book.placeholder): its picture, or its
 * title and author on the card's colour, under a "Not in library" banner,
 * linking to its page. No data-uuid, so reading marks pass it by;
 * data-placeholder names it (On deck's ✕, the popup's arrows, which step
 * onto it: modal.ts, and selection, to merge it with library books:
 * select-books.ts).
 */
function placeholderCard(book: Book, p: NonNullable<Book["placeholder"]>): HTMLElement {
    const title = book.title || "Untitled";
    const index = book.series_index && book.series_index !== 1 ? ` #${book.series_index}` : "";
    const link = el("a", { href: safeUrl(p.href), className: "book-link placeholder-link" });
    link.setAttribute("aria-label", `${title}${index}, not in the library`);
    const banner = el("span", { className: "placeholder-banner" }, "Not in library");
    banner.setAttribute("aria-hidden", "true");
    const wrapper = p.cover
        ? coverWrapper(book, el("img", { className: "book-cover loaded", src: safeUrl(p.cover), alt: "" }), banner)
        : coverWrapper(book, banner);
    if (!p.cover) wrapper.classList.add("no-cover");
    link.append(wrapper);
    const card = el("div", { className: "book placeholder-card" }, link, bookInfo(title, book.authors || ""));
    card.dataset["placeholder"] = book.uuid;
    return card;
}

/**
 * A book's card.
 * Plain link to #book/<uuid>; modal logic lives in setupModal(). htmx only
 * preloads the book on hover (hx-trigger="none": the click is the hash
 * change, and modal.ts's loadBook uses the preloaded response). Run
 * htmx.process on the container once it's in the page (setupBookClickHandlers
 * does).
 *
 * The cover waits in data-src for the grid's download queue, unless
 * coverReady says the browser already has it: then it's shown at once,
 * without the fade-in.
 */
export function bookCard(book: Book, coverReady = false): HTMLElement {
    if (book.series_card) return seriesCard(book, book.series_card, coverReady);
    if (book.placeholder) return placeholderCard(book, book.placeholder);
    const id = encodeURIComponent(book.uuid);
    const link = el("a", { href: `#book/${id}`, className: "book-link" });
    link.setAttribute("hx-get", `/book/get/${id}`);
    link.setAttribute("hx-trigger", "none");
    link.setAttribute("hx-preload", "mouseover");
    link.append(coverWrapper(book, coverImg(coverUrl(book), coverReady, book.uuid)));
    const card = el("div", { className: "book" }, link, bookInfo(book.title || "Untitled", book.authors || "Unknown Author"));
    card.dataset["uuid"] = book.uuid;
    return card;
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
