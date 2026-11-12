// Choosing books in a grid to merge, edit or remove them (admins, with
// -allow-writes). "Select" in the top bar, or a long press on a card on a
// touch screen, turns taps on cards into ticks; a bar at the bottom counts
// them and offers Merge (the metadata editor's merge mode, Go: merge.go),
// Edit in table (the bulk editor, library-edit.ts) and Remove (the
// confirmation, delete-books.ts). The
// choice survives scrolling: cards are marked again as they're re-rendered
// (markSelected, from app-htmx.ts).

import { el, modalOpen } from "./dom.js";
import { deleteBooks, type BookToDelete } from "./delete-books.js";

const LONG_PRESS_MS = 450; // before Android opens its link menu (~500 ms)
const LONG_PRESS_SLOP_PX = 10; // a finger moving further is scrolling

const chosen = new Map<string, BookToDelete>(); // uuid -> book, in the order picked
let selecting = false;
let bar: HTMLElement | null = null;
let countEl: HTMLElement | null = null;
let removeBtn: HTMLButtonElement | null = null;
let mergeBtn: HTMLButtonElement | null = null;
let tableBtn: HTMLButtonElement | null = null;

/** At most this many books are merged at once (Go: mergeMaxBooks). */
const MERGE_MAX = 20;
/** sessionStorage: the books to show in the bulk editor (library-edit.ts). */
export const BULK_SELECTION_KEY = "cf-bulk-selection";
let toggleBtn: HTMLButtonElement | null = null;

/** Mark a freshly rendered card if it's chosen. */
export function markSelected(card: HTMLElement): void {
    const uuid = card.dataset["uuid"];
    if (uuid) paint(card, chosen.has(uuid));
}

function paint(card: HTMLElement, on: boolean): void {
    card.classList.toggle("selected", on);
    if (selecting) card.setAttribute("aria-checked", String(on));
    else card.removeAttribute("aria-checked");
}

function cardsOf(grid: HTMLElement): NodeListOf<HTMLElement> {
    return grid.querySelectorAll<HTMLElement>(".book[data-uuid]");
}

function update(): void {
    const n = chosen.size;
    if (countEl) countEl.textContent = n ? `${n} selected` : "Tap books to select them";
    if (removeBtn) {
        removeBtn.disabled = n === 0;
        removeBtn.textContent = n ? `Remove ${n} from library…` : "Remove from library…";
    }
    if (mergeBtn) {
        mergeBtn.disabled = n < 2 || n > MERGE_MAX;
        mergeBtn.title = n > MERGE_MAX ? `At most ${MERGE_MAX} books at a time` : "Make the selected books one book, with every format: pick its metadata, cover and files";
        mergeBtn.textContent = n >= 2 ? `Merge ${n}…` : "Merge…";
    }
    if (tableBtn) tableBtn.disabled = n === 0;
}

function setSelecting(grid: HTMLElement, on: boolean): void {
    selecting = on;
    document.body.classList.toggle("selecting", on);
    toggleBtn?.setAttribute("aria-pressed", String(on));
    if (bar) bar.hidden = !on;
    if (!on) chosen.clear();
    for (const card of cardsOf(grid)) markSelected(card);
    update();
}

/** Offer selection on this grid once we know the viewer may change the library. */
export function setupSelection(grid: HTMLElement): void {
    let ready = false;
    const start = (): void => {
        if (ready || document.documentElement.dataset["canEdit"] !== "1") return;
        const tools = document.querySelector(".tb-tools");
        if (!tools) return;
        ready = true;

        toggleBtn = el("button", { type: "button", className: "tb-btn tb-select-btn", title: "Select books to merge, edit or remove them" }, "Select");
        toggleBtn.setAttribute("aria-pressed", "false");
        toggleBtn.addEventListener("click", () => setSelecting(grid, !selecting));
        tools.append(toggleBtn);

        countEl = el("span", { className: "select-count", role: "status" });
        removeBtn = el("button", { type: "button", className: "btn-action btn-danger", disabled: true });
        const clear = el("button", { type: "button", className: "btn-action" }, "Clear");
        const done = el("button", { type: "button", className: "btn-action" }, "Done");
        mergeBtn = el("button", { type: "button", className: "btn-action", disabled: true }, "Merge…");
        tableBtn = el("button", { type: "button", className: "btn-action", disabled: true, title: "Open the selected books in the bulk editor's table" }, "Edit in table");
        bar = el("div", { className: "select-bar", hidden: true }, countEl, el("span", { className: "select-bar-actions" }, clear, done, tableBtn, mergeBtn, removeBtn));
        bar.setAttribute("aria-label", "Selected books");
        document.body.append(bar);

        clear.addEventListener("click", () => {
            chosen.clear();
            for (const card of cardsOf(grid)) markSelected(card);
            update();
        });
        done.addEventListener("click", () => setSelecting(grid, false));
        mergeBtn.addEventListener("click", () => {
            const q = new URLSearchParams({ ids: [...chosen.keys()].join(",") });
            location.assign(`/book/merge?${q}`);
        });
        tableBtn.addEventListener("click", () => {
            try {
                sessionStorage.setItem(BULK_SELECTION_KEY, JSON.stringify([...chosen.keys()]));
            } catch {
                /* no storage: the table opens with every book */
            }
            location.assign("/library/edit?selected=1");
        });
        removeBtn.addEventListener("click", async () => {
            if (await deleteBooks([...chosen.values()])) location.reload();
        });

        const cardAt = (target: EventTarget | null): HTMLElement | null =>
            target instanceof Element ? target.closest<HTMLElement>(".book[data-uuid]") : null;
        const toggle = (card: HTMLElement): void => {
            const uuid = card.dataset["uuid"]!;
            if (chosen.has(uuid)) chosen.delete(uuid);
            else
                chosen.set(uuid, {
                    uuid,
                    title: card.querySelector(".book-info h2")?.textContent ?? "",
                    authors: card.querySelector(".book-author")?.textContent ?? "",
                });
            paint(card, chosen.has(uuid));
            update();
        };

        // Long press (touch and pen): start selecting, with this book. The
        // click the lifted finger sends next is swallowed, so the popup
        // doesn't open (or the tick doesn't come straight off again).
        grid.classList.add("can-select"); // no iOS link preview or text selection on a long press
        let press: { timer: ReturnType<typeof setTimeout>; x: number; y: number } | null = null;
        let swallowClickUntil = 0;
        const cancelPress = (): void => {
            if (press) clearTimeout(press.timer);
            press = null;
        };
        grid.addEventListener("pointerdown", (e) => {
            cancelPress();
            if (e.pointerType === "mouse" || !e.isPrimary) return;
            const card = cardAt(e.target);
            if (!card) return;
            press = {
                x: e.clientX,
                y: e.clientY,
                timer: setTimeout(() => {
                    press = null;
                    swallowClickUntil = Date.now() + 1500;
                    if (!selecting) setSelecting(grid, true);
                    toggle(card);
                    navigator.vibrate?.(15);
                }, LONG_PRESS_MS),
            };
        });
        grid.addEventListener("pointermove", (e) => {
            if (press && Math.hypot(e.clientX - press.x, e.clientY - press.y) > LONG_PRESS_SLOP_PX) cancelPress();
        });
        for (const type of ["pointerup", "pointercancel", "pointerleave"]) grid.addEventListener(type, cancelPress);
        window.addEventListener("scroll", cancelPress, { passive: true });
        // Android's long-press menu for the link.
        grid.addEventListener("contextmenu", (e) => {
            if (Date.now() < swallowClickUntil || (press && cardAt(e.target))) e.preventDefault();
        });

        // Capture, so the card's own link (the book popup) never sees it.
        grid.addEventListener(
            "click",
            (e) => {
                const card = cardAt(e.target);
                if (card && Date.now() < swallowClickUntil) {
                    swallowClickUntil = 0;
                    e.preventDefault();
                    e.stopPropagation();
                    return;
                }
                if (!selecting || !card) return;
                e.preventDefault();
                e.stopPropagation();
                toggle(card);
            },
            true,
        );
        document.addEventListener("keydown", (e) => {
            if (e.key === "Escape" && selecting && !modalOpen()) setSelecting(grid, false);
        });
        update();
    };
    start();
    document.addEventListener("cf:me", start); // nav.ts, once /api/me answers
}
