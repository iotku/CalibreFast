/**
 * Edit (the top bar's button) on Favorites, Saved, Finished and the
 * shelves a user can edit: the grid becomes a list of the books, to drag
 * into place or move with the buttons (reorder.ts), as on the home page.
 * Each move is saved (list_order.go); Done shows the grid in the new order.
 *
 * Configured on #list-edit: data-arrange, where the order is PUT; on a
 * shelf, data-remove, the shelf's API path: each book (or whole series)
 * then has Remove, which takes it off the shelf (shelves.go, shelf_series.go).
 */
import { api, byId, el, errorMessage } from "./dom.js";
import type { Book } from "./types.js";
import { colorFromBook, coverUrl, seriesLabel } from "./utils.js";
import { makeReorderable, type Reorderable } from "./reorder.js";

const SAVE_DELAY_MS = 400; // moves in a row are one save

const btn = byId("list-edit", HTMLButtonElement);
const books = byId("books");
const url = btn.dataset["arrange"] ?? "";
const removeBase = btn.dataset["remove"] ?? "";
const SERIES_KEY = "series:"; // a whole series' entry (Go: seriesKeyPrefix)
// The grid's own JSON, the whole list at once.
const allUrl = (books.dataset["pageUrl"] ?? "").replace(/&page=\{n\}$/, "") + "&all=1";

let editing: Reorderable | null = null;
let panel: HTMLElement | null = null;
let changed = false;
let pending: string[] | null = null;
let timer: ReturnType<typeof setTimeout> | undefined;
let saving: Promise<void> = Promise.resolve();

function say(text: string, isError = false): void {
    const el = panel?.querySelector<HTMLElement>(".arrange-status");
    if (!el) return;
    el.textContent = text;
    el.classList.toggle("error", isError);
}

function flush(): Promise<void> {
    clearTimeout(timer);
    const order = pending;
    pending = null;
    if (!order) return saving;
    saving = saving.then(() =>
        api("PUT", url, { order }).then(
            () => say(pending ? "" : "Saved."),
            (err: unknown) => say(`Not saved: ${errorMessage(err)}`, true),
        ),
    );
    return saving;
}

function row(b: Book): HTMLLIElement {
    const title = b.title || "Untitled";
    // A shelf's whole series (shelf_series.go): one entry, moved like a book.
    const series = b.series_card;
    const sub = series ? el("span", { className: "arrange-series" }, seriesLabel(series.count)) : b.authors || "Unknown Author";
    const cover = el("img", { className: "arrange-cover", src: coverUrl(b), alt: "", loading: "lazy", decoding: "async" });
    cover.style.background = colorFromBook(b);
    const li = el(
        "li",
        { className: `arrange-item${series ? " is-series" : ""}` },
        cover,
        el("span", { className: "arrange-text" }, el("span", { className: "arrange-title" }, title), el("span", { className: "arrange-author" }, sub)),
        el("span", { className: "arrange-controls" }),
    );
    li.dataset["id"] = b.uuid;
    li.dataset["name"] = title;
    if (removeBase) {
        const rm = el("button", { type: "button", className: "arrange-remove", title: `Remove “${title}” from this shelf` }, "Remove");
        rm.setAttribute("aria-label", `Remove ${title} from this shelf`);
        rm.addEventListener("click", () => void remove(li, b.uuid, title, rm));
        li.append(rm);
    }
    return li;
}

/** Takes an entry off the shelf, and out of the list. */
async function remove(li: HTMLElement, id: string, title: string, rm: HTMLButtonElement): Promise<void> {
    rm.disabled = true;
    const path = id.startsWith(SERIES_KEY)
        ? `${removeBase}/series/${encodeURIComponent(id.slice(SERIES_KEY.length))}`
        : `${removeBase}/books/${encodeURIComponent(id)}`;
    try {
        await flush(); // an order on its way names this entry: let it land first
        await api("DELETE", path);
        const next = li.nextElementSibling ?? li.previousElementSibling;
        li.remove();
        changed = true;
        say(`Removed “${title}”.`);
        next?.querySelector<HTMLButtonElement>(".arrange-remove")?.focus();
    } catch (err) {
        rm.disabled = false;
        say(`Not removed: ${errorMessage(err)}`, true);
    }
}

function setPressed(on: boolean): void {
    btn.setAttribute("aria-pressed", String(on));
    const label = on ? "Done arranging" : "Arrange books";
    btn.setAttribute("aria-label", label);
    btn.title = label;
    document.body.classList.toggle("list-editing", on);
}

async function start(): Promise<void> {
    btn.disabled = true;
    try {
        const list = await api<Book[]>("GET", allUrl);
        panel = document.createElement("section");
        panel.className = "arrange";
        panel.setAttribute("aria-label", "Arrange books");
        const statusEl = el("p", { className: "arrange-status" });
        statusEl.setAttribute("role", "status");
        panel.append(
            el(
                "p",
                { className: "arrange-hint" },
                removeBase
                    ? "Drag the books into place, move them with the arrows, or remove them from the shelf. Changes are saved as you go."
                    : "Drag the books into place, or move them with the arrows. Changes are saved as you go.",
            ),
            statusEl,
            el("ol", { className: "arrange-list" }, ...list.map(row)),
        );
        // No cover: the book's colour, as on the grid.
        panel.addEventListener(
            "error",
            (e) => {
                const img = e.target as HTMLElement;
                const blank = document.createElement("span");
                blank.className = img.className;
                blank.style.cssText = img.style.cssText;
                img.replaceWith(blank);
            },
            true,
        );
        books.before(panel);
        const ol = panel.querySelector<HTMLElement>(".arrange-list")!;
        editing = makeReorderable({
            list: ol,
            items: ".arrange-item",
            label: (el) => el.dataset["name"] ?? "",
            slot: (el) => el.querySelector<HTMLElement>(".arrange-controls")!,
            onChange: (ids) => {
                changed = true;
                pending = ids;
                say("Saving…");
                clearTimeout(timer);
                timer = setTimeout(() => void flush(), SAVE_DELAY_MS);
            },
        });
        setPressed(true);
        window.scrollTo(0, 0);
    } catch (err) {
        alert(errorMessage(err));
    } finally {
        btn.disabled = false;
    }
}

async function stop(): Promise<void> {
    btn.disabled = true;
    await flush();
    if (changed) {
        // The grid's pages are in the old order: start it again, at the top.
        const u = new URL(location.href);
        u.searchParams.delete("page");
        u.hash = "";
        history.replaceState(null, "", u);
        location.reload();
        return;
    }
    editing?.stop();
    editing = null;
    panel?.remove();
    panel = null;
    setPressed(false);
    btn.disabled = false;
    window.dispatchEvent(new Event("resize")); // the grid measures itself again
}

btn.addEventListener("click", () => void (editing ? stop() : start()));
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && editing && !document.querySelector(".modal-overlay.active")) void stop();
});
// Leaving with a save still waiting: send it.
window.addEventListener("pagehide", () => {
    if (pending) void flush();
});
