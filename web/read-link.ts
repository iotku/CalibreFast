/**
 * The cover on a book's page and popup (templates/book.html, .cover-link
 * with data-formats) opens the book in the format last read, for a book
 * with both an EPUB and a PDF (last-format.ts). The page links the EPUB.
 * Loaded with user.ts, which runs wherever a book page or popup can show.
 */

import { readerUrl } from "./book-cache.js";
import { preferredFormat } from "./last-format.js";

function point(link: HTMLAnchorElement): void {
    const uuid = link.dataset["uuid"];
    if (!uuid) return;
    const format = preferredFormat(uuid, (link.dataset["formats"] ?? "").split(" "));
    if (format) link.href = readerUrl(uuid, format);
}

function scan(root: Node): void {
    if (!(root instanceof HTMLElement)) return;
    if (root.matches("a.cover-link[data-formats]")) point(root as HTMLAnchorElement);
    root.querySelectorAll<HTMLAnchorElement>("a.cover-link[data-formats]").forEach(point);
}

new MutationObserver((records) => {
    for (const r of records) for (const n of r.addedNodes) scan(n);
}).observe(document.documentElement, { childList: true, subtree: true });
scan(document.body);
// Back from the reader to a page kept in the back/forward cache: it may have been the other format.
window.addEventListener("pageshow", (e) => {
    if (e.persisted) scan(document.body);
});
