/**
 * Edit (the top bar's button) on Favorites, Want to read, Finished and the
 * shelves a user can edit: the grid becomes a list of the books, to drag
 * into place or move with the buttons (reorder.ts), as on the home page.
 * Each move is saved (list_order.go); Done shows the grid in the new order.
 *
 * Configured on #list-edit: data-arrange, where the order is PUT.
 */
import { api, byId, errorMessage } from "./dom.js";
import type { Book } from "./types.js";
import { colorFromBook, coverUrl, escapeHtml } from "./utils.js";
import { makeReorderable, type Reorderable } from "./reorder.js";

const SAVE_DELAY_MS = 400; // moves in a row are one save

const btn = byId("list-edit", HTMLButtonElement);
const books = byId("books");
const url = btn.dataset["arrange"] ?? "";
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

function row(b: Book): string {
    const title = escapeHtml(b.title || "Untitled");
    return `<li class="arrange-item" data-id="${escapeHtml(b.uuid)}" data-name="${title}">
        <img class="arrange-cover" src="${escapeHtml(coverUrl(b))}" alt="" loading="lazy" decoding="async" style="background:${colorFromBook(b)}" />
        <span class="arrange-text"><span class="arrange-title">${title}</span><span class="arrange-author">${escapeHtml(b.authors || "Unknown Author")}</span></span>
        <span class="arrange-controls"></span>
    </li>`;
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
        panel.innerHTML =
            `<p class="arrange-hint">Drag the books into place, or move them with the arrows. Changes are saved as you go.</p>` +
            `<p class="arrange-status" role="status"></p>` +
            `<ol class="arrange-list">${list.map(row).join("")}</ol>`;
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
