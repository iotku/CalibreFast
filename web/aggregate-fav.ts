/**
 * The ★ favorite buttons (`.agg-fav`, data-kind and data-name) before an
 * entry's name on its page (templates/agg-fav.html, in filtered.html), and
 * the user's favorites on /account (`#account-favs`), each with its own ★.
 * The markup is the same for everyone and starts hidden; it's shown to
 * signed-in users from their favorites (favorites.ts).
 *
 * Also /me/favorites' carousels of favorite authors (rendered by the
 * server, filtered.html) and of series (drawn here from their grid entries
 * with bookCard, as a shelf's series are): they drag sideways like
 * Discover's rows, and each one's "Show all" wraps it to show every card.
 */
import { el, errorMessage, icon } from "./dom.js";
import { setupDragScroll } from "./drag-scroll.js";
import { isFavorite, keptFavorites, loadFavorites, setFavorite } from "./favorites.js";
import type { AggFavorite, Book } from "./types.js";
import { bookCard, setupCoverStates } from "./utils.js";

const STAR = "M12 3l2.7 5.6 6.1.9-4.4 4.3 1 6.1L12 17l-5.4 2.9 1-6.1-4.4-4.3 6.1-.9z";

/** Section headings on /account, by kind, in this order. */
const KINDS: [kind: string, heading: string, prefix: string][] = [
    ["author", "Authors", "/author/"],
    ["series", "Series", "/series/"],
    ["tag", "Tags", "/tag/"],
    ["publisher", "Publishers", "/publisher/"],
    ["language", "Languages", "/lang/"],
];

function starButton(kind: string, name: string): HTMLButtonElement {
    const btn = el("button", { type: "button", className: "agg-fav" });
    btn.dataset["kind"] = kind;
    btn.dataset["name"] = name;
    btn.setAttribute("aria-label", "Favorite");
    btn.append(icon({ width: 18, height: 18, fill: "none", stroke: "currentColor", "stroke-width": 2, "stroke-linejoin": "round" }, STAR));
    return btn;
}

/** /account's list: favorites by kind, each a link and its ★. */
function paintAccount(list: AggFavorite[]): void {
    const box = document.getElementById("account-favs");
    if (!box) return;
    const groups = KINDS.flatMap(([kind, heading, prefix]) => {
        const mine = list.filter((f) => f.kind === kind);
        if (!mine.length) return [];
        return [
            el("h3", {}, heading),
            el(
                "div",
                { className: "agg-chips" },
                ...mine.map((f) =>
                    el(
                        "span",
                        { className: "agg-chip" },
                        el("a", { href: prefix + encodeURIComponent(f.name) }, f.label || f.name),
                        el("span", { className: "agg-count" }, String(f.count)),
                        starButton(f.kind, f.name),
                    ),
                ),
            ),
        ];
    });
    const empty = box.querySelector<HTMLElement>(".account-favs-empty");
    box.querySelector(".account-favs-list")?.replaceChildren(...groups);
    if (empty) empty.hidden = groups.length > 0;
    box.hidden = false;
}

function paint(list: AggFavorite[]): void {
    paintAccount(list);
    for (const btn of document.querySelectorAll<HTMLButtonElement>(".agg-fav")) {
        const on = isFavorite(list, btn.dataset["kind"] ?? "", btn.dataset["name"] ?? "");
        btn.setAttribute("aria-pressed", String(on));
        btn.title = on ? "Favorite (click to remove)" : "Add to favorites";
        btn.hidden = false;
    }
}

const kept = keptFavorites();
if (kept) paint(kept);
void loadFavorites()
    .then((list) => {
        if (list) paint(list);
        else for (const btn of document.querySelectorAll<HTMLElement>(".agg-fav, #account-favs")) btn.hidden = true;
    })
    .catch(() => {
        /* offline: the kept list stands */
    });

document.addEventListener("click", async (e) => {
    const btn = (e.target as Element | null)?.closest<HTMLButtonElement>(".agg-fav");
    if (!btn || btn.disabled) return;
    const on = btn.getAttribute("aria-pressed") !== "true";
    btn.disabled = true;
    try {
        paint(await setFavorite(btn.dataset["kind"] ?? "", btn.dataset["name"] ?? "", on));
    } catch (err) {
        alert(errorMessage(err));
    } finally {
        btn.disabled = false;
    }
});

// Series rows: their entries as a shelf's series cards, the same as anywhere else.
for (const row of document.querySelectorAll<HTMLElement>(".fav-books-row[data-books]")) {
    let books: Book[] = [];
    try {
        books = JSON.parse(row.dataset["books"] ?? "[]") as Book[];
    } catch {
        /* nothing to show */
    }
    setupCoverStates(row);
    row.replaceChildren(...books.map((b) => bookCard(b, true)));
}
if (document.querySelector(".fav-aggs-row")) setupDragScroll(document.body, ".fav-aggs-row");
for (const all of document.querySelectorAll<HTMLButtonElement>(".fav-aggs-all")) {
    const row = document.getElementById(all.getAttribute("aria-controls") ?? "");
    if (!row) continue;
    // Only offered while the row doesn't show every card already.
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
}
