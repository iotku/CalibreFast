/**
 * A whole series on a shelf (shelf_series.go): the shelf checkboxes for a
 * series, on the series page (its top bar's "Add to shelf" panel) and in
 * the book popup ("Add series to shelf", templates/book-user-actions.html).
 *
 * The checkboxes (input[data-series-shelf]) and the New shelf field live in
 * an element with data-series, the series as browsed. Handled by delegation,
 * so popups that arrive later work too.
 */
import { api, errorMessage, modalOpen } from "./dom.js";

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

    // The series page's top-bar button shows and hides its panel; its ×
    // closes it.
    const toggle = t?.closest<HTMLButtonElement>(".series-shelf-toggle");
    const panel = document.getElementById("series-shelf-panel");
    if (toggle && panel) {
        const show = panel.hidden;
        panel.hidden = !show;
        toggle.setAttribute("aria-expanded", String(show));
        if (show) followButton(panel, toggle);
    }
    if (t?.closest(".series-shelf-close")) closeShelfPanel();
});

function closeShelfPanel(): void {
    const panel = document.getElementById("series-shelf-panel");
    if (!panel || panel.hidden) return;
    panel.hidden = true;
    for (const b of document.querySelectorAll(".series-shelf-toggle")) b.setAttribute("aria-expanded", "false");
}

// A tap off the open panel closes it. Off the bar too, it goes no further
// (it doesn't open the book under it); on the bar it does (another menu).
document.addEventListener(
    "click",
    (e) => {
        const panel = document.getElementById("series-shelf-panel");
        const t = e.target instanceof Element ? e.target : null;
        if (!panel || panel.hidden || !t || panel.contains(t) || t.closest(".series-shelf-toggle")) return;
        closeShelfPanel();
        if (t.closest(".site-bar")) return;
        e.preventDefault();
        e.stopPropagation();
    },
    true,
);

/** Up to the skinny bar (style.css), the panel is centred on its button; past it, the full width. */
const SKINNY = "(max-width: 600px)";
let following: (() => void) | null = null;

/**
 * Keeps the open panel centred under the button that opened it, as far as
 * the window's edges allow, and following it while the bar is redrawn
 * (resized, steps folded or unfolded: nav.ts). When the button has folded
 * into ⋯, the panel goes under the ⋯ instead.
 */
function followButton(panel: HTMLElement, toggle: HTMLElement): void {
    const bar = document.querySelector<HTMLElement>(".site-bar");
    const place = (): void => {
        if (panel.hidden) return stop();
        if (window.matchMedia(SKINNY).matches) {
            panel.style.left = panel.style.right = panel.style.top = "";
            return;
        }
        const anchor = [toggle, toggle.closest(".tb-actions")?.querySelector<HTMLElement>(".tb-manage-btn")]
            .map((el) => el?.getBoundingClientRect())
            .find((r) => r && r.width > 0);
        if (!anchor) return;
        const w = panel.offsetWidth;
        const max = document.documentElement.clientWidth - w - 8;
        const centre = anchor.left + anchor.width / 2;
        panel.style.left = `${Math.round(Math.max(8, Math.min(centre - w / 2, max)))}px`;
        panel.style.right = "auto";
        panel.style.top = `${Math.round(Math.max(anchor.bottom, bar?.getBoundingClientRect().bottom ?? 0) + 8)}px`;
    };
    stop();
    const observer = new ResizeObserver(place);
    if (bar) observer.observe(bar);
    // The bar keeps its size when a step folds; its parts move.
    const mutations = new MutationObserver(place);
    if (bar) mutations.observe(bar, { attributes: true, attributeFilter: ["class"] });
    window.addEventListener("resize", place);
    following = () => {
        observer.disconnect();
        mutations.disconnect();
        window.removeEventListener("resize", place);
    };
    place();

    function stop(): void {
        following?.();
        following = null;
    }
}

// Capture phase: seen before the book popup's own Esc closes it.
document.addEventListener("keydown", (e) => e.key === "Escape" && !modalOpen() && closeShelfPanel(), true);

document.addEventListener("keydown", (e) => {
    const input = e.target;
    if (e.key !== "Enter" || !(input instanceof HTMLInputElement) || !input.matches(".series-shelf-new")) return;
    e.preventDefault();
    void create(input);
});

document.addEventListener("input", (e) => {
    if (e.target instanceof HTMLInputElement && e.target.matches(".series-shelf-new")) e.target.setCustomValidity("");
});
