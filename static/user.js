/**
 * The per-user controls under a book's read/download buttons (favorite,
 * reading status, shelves). The server renders them (/book/me/<uuid>); this
 * file loads them into each book page's .book-user-actions and wires up
 * clicks.
 *
 * In the book modal they're fetched ahead: when the pointer is over a book
 * card (alongside the card's hx-preload of the book) and as soon as the
 * modal starts loading the book, so they arrive with it rather than a round
 * trip after it.
 */
import { api, errorMessage } from "./dom.js";
const PREFETCH_TTL_MS = 30_000; // like the book page's own max-age
const prefetched = new Map();
function fetchControls(uuid) {
    return fetch(`/book/me/${encodeURIComponent(uuid)}`, { credentials: "same-origin" })
        .then((res) => (res.ok ? res.text() : null))
        .catch(() => null); // offline: the throbber stays
}
/** Start fetching a book's controls, for the next time they're shown. */
export function prefetchControls(uuid) {
    const now = Date.now();
    for (const [k, v] of prefetched)
        if (v.expires <= now)
            prefetched.delete(k);
    if (prefetched.has(uuid))
        return;
    prefetched.set(uuid, { html: fetchControls(uuid), expires: now + PREFETCH_TTL_MS });
}
/** (Re)load the controls into their container. A prefetch is used once; fresh skips it. */
async function loadControls(container, fresh = false) {
    const uuid = container.dataset["uuid"] ?? "";
    const hit = prefetched.get(uuid);
    prefetched.delete(uuid);
    const html = await (!fresh && hit && hit.expires > Date.now() ? hit.html : fetchControls(uuid));
    if (html !== null && container.isConnected)
        container.innerHTML = html;
}
function bind(controls) {
    if (controls.dataset["bound"])
        return;
    controls.dataset["bound"] = "1";
    const uuid = controls.dataset["uuid"] ?? "";
    const container = controls.closest(".book-user-actions");
    const star = controls.querySelector(".btn-favorite");
    star?.addEventListener("click", async () => {
        const want = star.getAttribute("aria-pressed") !== "true";
        const paint = (on) => {
            star.setAttribute("aria-pressed", String(on));
            star.classList.toggle("is-favorite", on);
            star.textContent = `${on ? "★" : "☆"} Favorite`;
            star.title = on ? "Remove from favorites" : "Add to favorites";
        };
        paint(want); // optimistic
        try {
            await api("PUT", `/api/me/books/${uuid}/favorite`, { favorite: want });
        }
        catch {
            paint(!want);
        }
    });
    const status = controls.querySelector(".book-status");
    const note = controls.querySelector(".book-status-note");
    let current = status?.value ?? "";
    status?.addEventListener("change", async () => {
        try {
            const res = await api("PUT", `/api/me/books/${uuid}/status`, { status: status.value });
            current = res.status;
            if (note)
                note.textContent = res.status && res.status_source === "auto" ? "set by the reader" : "";
        }
        catch {
            status.value = current;
        }
    });
    const count = controls.querySelector(".book-shelves-count");
    const paintCount = () => {
        const n = controls.querySelectorAll("input[data-shelf]:checked").length;
        if (count)
            count.textContent = n ? ` (${n})` : "";
    };
    controls.querySelectorAll("input[data-shelf]").forEach((box) => box.addEventListener("change", async () => {
        try {
            await api(box.checked ? "PUT" : "DELETE", `/api/shelves/${box.dataset["shelf"]}/books/${uuid}`);
        }
        catch {
            box.checked = !box.checked;
        }
        paintCount();
    }));
    // Create a personal shelf with this book on it.
    const input = controls.querySelector(".book-shelf-new");
    const create = async () => {
        if (!input || !container)
            return;
        const name = input.value.trim();
        if (!name)
            return;
        try {
            const shelf = await api("POST", "/api/shelves", { name });
            await api("PUT", `/api/shelves/${shelf.id}/books/${uuid}`);
            await loadControls(container, true);
            container.querySelector(".book-shelves")?.setAttribute("open", "");
        }
        catch (err) {
            input.setCustomValidity(errorMessage(err));
            input.reportValidity();
        }
    };
    controls.querySelector(".book-shelf-add")?.addEventListener("click", create);
    input?.addEventListener("input", () => input.setCustomValidity(""));
    input?.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
            e.preventDefault();
            void create();
        }
    });
}
function scan(root) {
    if (!(root instanceof HTMLElement))
        return;
    // Controls that arrived.
    if (root.matches(".book-user-controls"))
        bind(root);
    root.querySelectorAll(".book-user-controls").forEach(bind);
    // Book pages that arrived: load their controls.
    const containers = root.matches(".book-user-actions") ? [root] : [...root.querySelectorAll(".book-user-actions")];
    for (const c of containers) {
        if (c.dataset["loading"])
            continue;
        c.dataset["loading"] = "1";
        void loadControls(c);
    }
}
new MutationObserver((records) => {
    for (const r of records)
        for (const n of r.addedNodes)
            scan(n);
}).observe(document.documentElement, { childList: true, subtree: true });
scan(document.documentElement);
