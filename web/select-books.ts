// Choosing books in a grid: to put them on a shelf (anyone signed in), or
// to merge, edit or remove them (editors, with -allow-writes). A Ctrl+click
// on a card, a long press on one on a touch screen or "Select books"
// above the grid turns taps on cards into ticks; a bar at the bottom counts them and
// offers Add to shelf (shelf-picker.ts) and, for editors, Merge (the
// metadata editor's merge mode, Go: merge.go), Edit in table (the bulk
// editor, library-edit.ts) and Remove (the confirmation, delete-books.ts). A placeholder (a book not in the library
// yet, data-placeholder) can be picked too, to merge with the library books
// that are it (Go: merge_placeholder.go); Edit and Remove leave it out. The
// choice survives scrolling: cards are marked again as they're re-rendered
// (markSelected, from app-htmx.ts).

import { el, modalOpen } from "./dom.js";
import { deleteBooks, type BookToDelete } from "./delete-books.js";
import { addToShelf } from "./shelf-picker.js";

const LONG_PRESS_MS = 450; // before Android opens its link menu (~500 ms)
const LONG_PRESS_SLOP_PX = 10; // a finger moving further is scrolling

const chosen = new Map<string, BookToDelete>(); // uuid -> book, in the order picked
const chosenPlaceholders = new Set<string>(); // the chosen that are placeholders
let selecting = false;
let bar: HTMLElement | null = null;
let countEl: HTMLElement | null = null;
let removeBtn: HTMLButtonElement | null = null;
let mergeBtn: HTMLAnchorElement | null = null;
let tableBtn: HTMLButtonElement | null = null;
let shelfBtn: HTMLButtonElement | null = null;

/** At most this many books are merged at once (Go: mergeMaxBooks). */
const MERGE_MAX = 20;
/** sessionStorage: the books to show in the bulk editor (library-edit.ts). */
export const BULK_SELECTION_KEY = "cf-bulk-selection";
/** The buttons that start and stop selecting: the sidebar's and the grid's. */
const toggleBtns: HTMLButtonElement[] = [];

/** A card's id: a library book's uuid, or a placeholder's. */
function cardId(card: HTMLElement): string | undefined {
    return card.dataset["uuid"] ?? card.dataset["placeholder"];
}

/** Mark a freshly rendered card if it's chosen. */
export function markSelected(card: HTMLElement): void {
    const uuid = cardId(card);
    if (uuid) paint(card, chosen.has(uuid));
}

function paint(card: HTMLElement, on: boolean): void {
    card.classList.toggle("selected", on);
    if (selecting) card.setAttribute("aria-checked", String(on));
    else card.removeAttribute("aria-checked");
}

function cardsOf(grid: HTMLElement): NodeListOf<HTMLElement> {
    return grid.querySelectorAll<HTMLElement>(CARD);
}

const CARD = ".book[data-uuid], .book[data-placeholder]";

/** The chosen library books, without placeholders. */
function chosenBooks(): BookToDelete[] {
    return [...chosen.values()].filter((b) => !chosenPlaceholders.has(b.uuid));
}

function clearChosen(): void {
    chosen.clear();
    chosenPlaceholders.clear();
}

function update(): void {
    const n = chosen.size;
    const books = chosenBooks().map((b) => b.uuid);
    const nb = books.length;
    const np = chosenPlaceholders.size;
    if (countEl) countEl.textContent = n ? (np ? `${n} selected (${np} not in library)` : `${n} selected`) : "Tap books to select them";
    if (removeBtn) {
        removeBtn.disabled = nb === 0;
        removeBtn.textContent = nb ? `Remove ${nb} from library…` : "Remove from library…";
    }
    if (mergeBtn) {
        // A link, so a middle click opens the merge in a new tab. With a
        // placeholder: it and at least one library book, which replaces it.
        let title = "Make the selected books one book, with every format: pick its metadata, cover and files";
        let href = "";
        if (np > 1) title = "Merge one placeholder at a time";
        else if (nb > MERGE_MAX) title = `At most ${MERGE_MAX} books at a time`;
        else if (np === 1) {
            if (nb === 0) title = "Select the library book (or books) that is this placeholder";
            else {
                title = "The selected library books replace the placeholder: pick the metadata, cover and files";
                const q = new URLSearchParams({ ids: books.join(","), placeholder: [...chosenPlaceholders][0]! });
                href = `/book/merge?${q}`;
            }
        } else if (nb >= 2) href = `/book/merge?${new URLSearchParams({ ids: books.join(",") })}`;
        if (href) {
            mergeBtn.href = href;
            mergeBtn.removeAttribute("aria-disabled");
        } else {
            mergeBtn.removeAttribute("href");
            mergeBtn.setAttribute("aria-disabled", "true");
        }
        mergeBtn.title = title;
        mergeBtn.textContent = n >= 2 ? `Merge ${n}…` : "Merge…";
    }
    if (tableBtn) tableBtn.disabled = nb === 0;
    if (shelfBtn) {
        shelfBtn.disabled = nb === 0;
        shelfBtn.textContent = nb ? `Add ${nb} to shelf…` : "Add to shelf…";
    }
}

function setSelecting(grid: HTMLElement, on: boolean): void {
    selecting = on;
    document.body.classList.toggle("selecting", on);
    for (const b of toggleBtns) {
        b.setAttribute("aria-pressed", String(on));
        b.textContent = on ? "Stop selecting" : "Select books";
    }
    if (bar) bar.hidden = !on;
    if (!on) clearChosen();
    for (const card of cardsOf(grid)) markSelected(card);
    update();
}

/** Offer selection on this grid once we know the viewer is signed in (and, for the library's actions, may change it). */
export function setupSelection(grid: HTMLElement): void {
    let ready = false;
    let editorReady = false;
    const root = document.documentElement.dataset;

    // Merge, Edit in table and Remove: editors only, added once we know.
    const addEditorActions = (actions: HTMLElement): void => {
        if (editorReady || root["canEdit"] !== "1") return;
        editorReady = true;

        removeBtn = el("button", { type: "button", className: "btn-action btn-danger", disabled: true });
        mergeBtn = el("a", { className: "btn-action" }, "Merge…");
        mergeBtn.setAttribute("aria-disabled", "true");
        tableBtn = el("button", { type: "button", className: "btn-action", disabled: true, title: "Open the selected books in the bulk editor's table" }, "Edit in table");
        actions.append(tableBtn, mergeBtn, removeBtn);
        tableBtn.addEventListener("click", () => {
            try {
                sessionStorage.setItem(BULK_SELECTION_KEY, JSON.stringify(chosenBooks().map((b) => b.uuid)));
            } catch {
                /* no storage: the table opens with every book */
            }
            location.assign("/library/edit?selected=1");
        });
        removeBtn.addEventListener("click", async () => {
            if (await deleteBooks(chosenBooks())) location.reload();
        });
        update();
    };

    let actions: HTMLElement | null = null;
    const start = (): void => {
        if (ready) {
            if (actions) addEditorActions(actions);
            return;
        }
        if (root["signedIn"] !== "1" && root["canEdit"] !== "1") return;
        ready = true;

        // "Select books" above the grid, for everyone signed in: by the sort
        // select where there is one.
        const gridBtn = el("button", { type: "button", className: "btn-standard grid-select-btn", title: "Choose books to add to a shelf (or Ctrl+click / long-press a book)" }, "Select books");
        gridBtn.setAttribute("aria-pressed", "false");
        gridBtn.addEventListener("click", () => setSelecting(grid, !selecting));
        toggleBtns.push(gridBtn);
        let tools = document.querySelector<HTMLElement>(".grid-sort");
        if (!tools) {
            tools = el("div", { className: "grid-sort grid-tools" });
            grid.before(tools);
        }
        tools.prepend(gridBtn);

        countEl = el("span", { className: "select-count", role: "status" });
        const clear = el("button", { type: "button", className: "btn-action" }, "Clear");
        const done = el("button", { type: "button", className: "btn-action" }, "Done");
        shelfBtn = el("button", { type: "button", className: "btn-action btn-action-read", disabled: true, title: "Put the selected books on one of your shelves" }, "Add to shelf…");
        actions = el("span", { className: "select-bar-actions" }, clear, done, shelfBtn);
        bar = el("div", { className: "select-bar", hidden: true }, countEl, actions);
        bar.setAttribute("aria-label", "Selected books");
        document.body.append(bar);

        clear.addEventListener("click", () => {
            clearChosen();
            for (const card of cardsOf(grid)) markSelected(card);
            update();
        });
        done.addEventListener("click", () => setSelecting(grid, false));
        shelfBtn.addEventListener("click", async () => {
            const said = await addToShelf(chosenBooks().map((b) => b.uuid));
            if (!said) return;
            setSelecting(grid, false);
            flash(said); // the bar's gone with the selection: say it where it was

        });
        addEditorActions(actions);

        const cardAt = (target: EventTarget | null): HTMLElement | null =>
            target instanceof Element ? target.closest<HTMLElement>(CARD) : null;
        const toggle = (card: HTMLElement): void => {
            const uuid = cardId(card)!;
            if (chosen.has(uuid)) {
                chosen.delete(uuid);
                chosenPlaceholders.delete(uuid);
            } else {
                if (card.dataset["placeholder"] === uuid) chosenPlaceholders.add(uuid);
                chosen.set(uuid, {
                    uuid,
                    title: card.querySelector(".book-info h2")?.textContent ?? "",
                    authors: card.querySelector(".book-author")?.textContent ?? "",
                });
            }
            paint(card, chosen.has(uuid));
            update();
        };

        // Long press (touch and pen): start selecting, with this book. The
        // click the lifted finger sends next is swallowed, so the popup
        // doesn't open (or the tick doesn't come straight off again).
        grid.classList.add("can-select"); // no iOS link preview or text selection on a long press
        let press: { timer: ReturnType<typeof setTimeout>; x: number; y: number } | null = null;
        // The click that follows a long press or a Mac's Ctrl+click, on the
        // same card: a click on another one is a pick of its own.
        let swallowClickUntil = 0;
        let swallowCard: HTMLElement | null = null;
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
                    swallowCard = card;
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

        // Ctrl+click (a mouse's long press): start selecting, with this book.
        // On a Mac, Ctrl+click is a right click: it comes as contextmenu,
        // and no click follows.
        const ctrlPick = (card: HTMLElement): void => {
            if (!selecting) setSelecting(grid, true);
            toggle(card);
        };
        grid.addEventListener("contextmenu", (e) => {
            const card = cardAt(e.target);
            if (!card || !e.ctrlKey) return;
            e.preventDefault();
            swallowClickUntil = Date.now() + 500;
            swallowCard = card;
            ctrlPick(card);
        });

        // Capture, so the card's own link (the book popup) never sees it.
        grid.addEventListener(
            "click",
            (e) => {
                const card = cardAt(e.target);
                if (card && card === swallowCard && Date.now() < swallowClickUntil) {
                    swallowClickUntil = 0;
                    swallowCard = null;
                    e.preventDefault();
                    e.stopPropagation();
                    return;
                }
                if (card && e.ctrlKey && !selecting) {
                    e.preventDefault(); // not a new tab
                    e.stopPropagation();
                    ctrlPick(card);
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

/** A short-lived note at the bottom of the page. */
function flash(text: string): void {
    const note = el("div", { className: "select-flash", role: "status" }, text);
    document.body.append(note);
    setTimeout(() => note.remove(), 4000);
}
