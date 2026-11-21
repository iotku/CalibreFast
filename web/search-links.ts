/**
 * The search page's matching authors, series and tags (templates/search.html):
 * each kind lists its first 20, and its "N more" button shows the rest (and
 * hides them again). Elements only, no markup: the names are library data.
 */

function setupSearchLinks(): void {
    document.querySelectorAll<HTMLButtonElement>(".search-links-more").forEach((btn) => {
        const group = btn.closest(".search-links");
        if (!group) return;
        const more = btn.dataset.more ?? "";
        btn.addEventListener("click", () => {
            const open = btn.getAttribute("aria-expanded") !== "true";
            group.querySelectorAll<HTMLElement>(".search-link-extra, .search-links-cut").forEach((a) => {
                a.hidden = !open;
            });
            btn.setAttribute("aria-expanded", String(open));
            btn.textContent = open ? "Show fewer" : `${more} more`;
        });
    });
}

setupSearchLinks();
