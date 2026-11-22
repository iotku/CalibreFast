/**
 * A whole series on a shelf (shelf_series.go): the shelf checkboxes for a
 * series, on the series page (its top bar's "Add to shelf" panel) and in
 * the book popup ("Add series to shelf", templates/book-user-actions.html).
 *
 * The checkboxes (input[data-series-shelf]) and the New shelf field live in
 * an element with data-series, the series as browsed. Handled by delegation,
 * so popups that arrive later work too.
 */
import { api, errorMessage } from "./dom.js";

const seriesUrl = (shelf: string, series: string): string =>
    `/api/shelves/${encodeURIComponent(shelf)}/series/${encodeURIComponent(series)}`;

function seriesOf(el: Element): string | null {
    return el.closest<HTMLElement>("[data-series]")?.dataset["series"] ?? null;
}

/** "(N)" on the popup's summary: the shelves the series is on. */
function paintCount(el: Element): void {
    const box = el.closest<HTMLElement>("[data-series]");
    const count = box?.querySelector<HTMLElement>(".book-shelves-count");
    if (!box || !count) return;
    const n = box.querySelectorAll("input[data-series-shelf]:checked").length;
    count.textContent = n ? ` (${n})` : "";
}

function shelfBox(id: number, name: string): HTMLLabelElement {
    const label = document.createElement("label");
    label.className = "book-shelf";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.dataset["seriesShelf"] = String(id);
    box.checked = true;
    label.append(box, ` ${name}`);
    return label;
}

async function create(input: HTMLInputElement): Promise<void> {
    const series = seriesOf(input);
    const name = input.value.trim();
    if (!series || !name) return;
    try {
        const shelf = await api<{ id: number }>("POST", "/api/shelves", { name });
        await api("PUT", seriesUrl(String(shelf.id), series));
        input.closest(".book-shelf-create")?.before(shelfBox(shelf.id, name));
        input.value = "";
        paintCount(input);
    } catch (err) {
        input.setCustomValidity(errorMessage(err));
        input.reportValidity();
    }
}

document.addEventListener("change", async (e) => {
    const box = e.target;
    if (!(box instanceof HTMLInputElement) || !box.matches("input[data-series-shelf]")) return;
    const series = seriesOf(box);
    if (!series) return;
    box.disabled = true;
    try {
        await api(box.checked ? "PUT" : "DELETE", seriesUrl(box.dataset["seriesShelf"] ?? "", series));
    } catch (err) {
        box.checked = !box.checked;
        alert(errorMessage(err));
    } finally {
        box.disabled = false;
    }
    paintCount(box);
});

document.addEventListener("click", (e) => {
    const t = e.target instanceof Element ? e.target : null;
    const add = t?.closest(".series-shelf-add");
    const input = add?.parentElement?.querySelector<HTMLInputElement>(".series-shelf-new");
    if (input) void create(input);

    // The series page's top-bar button shows and hides its panel.
    const toggle = t?.closest<HTMLButtonElement>(".series-shelf-toggle");
    const panel = document.getElementById("series-shelf-panel");
    if (toggle && panel) {
        const show = panel.hidden;
        // Where it was pressed, read before the panel shows (and before a
        // folded ⋯ menu holding the button closes).
        const anchor = show ? anchorOf(toggle, e) : null;
        panel.hidden = !show;
        toggle.setAttribute("aria-expanded", String(show));
        if (anchor) placeUnder(panel, anchor);
    }
});

interface Anchor {
    left: number;
    right: number;
    bottom: number;
}

/** The button's box; or, when it's not laid out, the point pressed, or the ⋯ that held it. */
function anchorOf(toggle: HTMLElement, e: MouseEvent): Anchor | null {
    const r = toggle.getBoundingClientRect();
    if (r.width > 0) return r;
    if (e.clientX || e.clientY) return { left: e.clientX, right: e.clientX, bottom: e.clientY };
    const more = toggle.closest(".tb-actions")?.querySelector(".tb-manage-btn")?.getBoundingClientRect();
    return more && more.width > 0 ? more : null;
}

/**
 * Opens the panel just under what was pressed: its right edge lined up
 * with the anchor's, moved in to stay 8px inside the window. A phone keeps
 * the stylesheet's full-width panel and only takes the height.
 */
function placeUnder(panel: HTMLElement, a: Anchor): void {
    const bar = document.querySelector(".site-bar")?.getBoundingClientRect().bottom ?? 0;
    panel.style.top = `${Math.round(Math.max(a.bottom, bar) + 8)}px`;
    if (window.innerWidth <= 600) {
        panel.style.left = "";
        panel.style.right = "";
        return;
    }
    const w = panel.offsetWidth;
    const max = document.documentElement.clientWidth - w - 8;
    panel.style.left = `${Math.round(Math.max(8, Math.min(a.right - w, max)))}px`;
    panel.style.right = "auto";
}

document.addEventListener("keydown", (e) => {
    const input = e.target;
    if (e.key !== "Enter" || !(input instanceof HTMLInputElement) || !input.matches(".series-shelf-new")) return;
    e.preventDefault();
    void create(input);
});

document.addEventListener("input", (e) => {
    if (e.target instanceof HTMLInputElement && e.target.matches(".series-shelf-new")) e.target.setCustomValidity("");
});
