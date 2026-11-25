/**
 * The carousels of favorite authors: on /me/favorites (filtered.html,
 * rendered by the server, with web/aggregate-fav.ts) and at the top of
 * /authors (aggregate.html, drawn from the user's kept favorites by its
 * inline script while the page parses, then by web/aggregate.ts). A row
 * drags sideways like Discover's, and its "Show all" wraps it to show every
 * card.
 *
 * favCard builds the same card as filtered.html and aggregate.html's inline
 * script: keep the three in step.
 */
import { el, safeUrl } from "./dom.js";
import { setupDragScroll } from "./drag-scroll.js";
import type { AggFavorite } from "./types.js";

/** "1 book", "1,234 books" (the templates' count). */
export function booksLabel(n: number): string {
    return `${n.toLocaleString("en-US")} ${n === 1 ? "book" : "books"}`;
}

/** One favorite as a card: its picture (or initial), name, and meta under it. */
export function favCard(f: AggFavorite, meta: string): HTMLAnchorElement {
    let pic: HTMLElement;
    if (f.image_url) {
        pic = el("img", { className: "fav-agg-img", src: safeUrl(f.image_url), alt: "", loading: "lazy" });
    } else {
        pic = el("span", { className: "fav-agg-img fav-agg-initial" }, f.initial ?? "");
        pic.setAttribute("aria-hidden", "true");
    }
    return el(
        "a",
        { className: "fav-agg", href: safeUrl(f.href) },
        pic,
        el("span", { className: "fav-agg-name" }, f.label || f.name),
        el("span", { className: "fav-agg-meta" }, meta),
    );
}

/**
 * The carousels under root (.fav-aggs-row) drag sideways, and each
 * .fav-aggs-all button (aria-controls: its row) wraps its row, offered only
 * while the row doesn't show every card already. Once per page; rows may be
 * filled later.
 */
export function setupFavRows(root: HTMLElement): void {
    if (!root.querySelector(".fav-aggs-row")) return;
    setupDragScroll(root, ".fav-aggs-row");
    for (const all of root.querySelectorAll<HTMLButtonElement>(".fav-aggs-all")) {
        const row = document.getElementById(all.getAttribute("aria-controls") ?? "");
        if (!row) continue;
        const fits = (): void => {
            all.hidden = !row.classList.contains("fav-aggs-wrap") && row.scrollWidth <= row.clientWidth;
        };
        all.addEventListener("click", () => {
            const open = row.classList.toggle("fav-aggs-wrap");
            all.setAttribute("aria-expanded", String(open));
            all.textContent = open ? "Show fewer" : "Show all";
            fits();
        });
        new ResizeObserver(fits).observe(row);
        // Cards added or taken away (aggregate.ts) change the width without resizing the row.
        new MutationObserver(fits).observe(row, { childList: true });
    }
}
