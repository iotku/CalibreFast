// Choosing books in a grid to remove them (admins, with -allow-writes).
// "Select" in the top bar turns clicks on cards into ticks; a bar at the
// bottom counts them and opens the confirmation (delete-books.ts). The
// choice survives scrolling: cards are marked again as they're re-rendered
// (markSelected, from app-htmx.ts).

import { el, modalOpen } from "./dom.js";
import { deleteBooks, type BookToDelete } from "./delete-books.js";

const chosen = new Map<string, BookToDelete>(); // uuid -> book, in the order picked
let selecting = false;
let bar: HTMLElement | null = null;
let countEl: HTMLElement | null = null;
let removeBtn: HTMLButtonElement | null = null;
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

        toggleBtn = el("button", { type: "button", className: "tb-btn tb-select-btn", title: "Select books to remove them from the library" }, "Select");
        toggleBtn.setAttribute("aria-pressed", "false");
        toggleBtn.addEventListener("click", () => setSelecting(grid, !selecting));
        tools.append(toggleBtn);

        countEl = el("span", { className: "select-count", role: "status" });
        removeBtn = el("button", { type: "button", className: "btn-action btn-danger", disabled: true });
        const clear = el("button", { type: "button", className: "btn-action" }, "Clear");
        const done = el("button", { type: "button", className: "btn-action" }, "Done");
        bar = el("div", { className: "select-bar", hidden: true }, countEl, el("span", { className: "select-bar-actions" }, clear, done, removeBtn));
        bar.setAttribute("aria-label", "Selected books");
        document.body.append(bar);

        clear.addEventListener("click", () => {
            chosen.clear();
            for (const card of cardsOf(grid)) markSelected(card);
            update();
        });
        done.addEventListener("click", () => setSelecting(grid, false));
        removeBtn.addEventListener("click", async () => {
            if (await deleteBooks([...chosen.values()])) location.reload();
        });

        // Capture, so the card's own link (the book popup) never sees it.
        grid.addEventListener(
            "click",
            (e) => {
                if (!selecting || !(e.target instanceof Element)) return;
                const card = e.target.closest<HTMLElement>(".book[data-uuid]");
                if (!card) return;
                e.preventDefault();
                e.stopPropagation();
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
