/**
 * The per-user controls under a book's read/download buttons (favorite,
 * reading status, shelves). The server renders them (/book/me/<uuid>); this
 * file loads them into each book page's .book-user-actions and wires up
 * clicks.
 *
 * In the book modal they're fetched as soon as the modal starts loading the
 * book, so they arrive with it rather than a round trip after it. Not on
 * hover: the grid's marks (reading.ts) already say what the cover buttons
 * need. Each time the controls load, what they show is reported
 * ("cf:book-state") so the grid can check its marks against the server;
 * changes saved here are reported too ("cf:status"), for the grid to apply.
 */

import { api, errorMessage } from "./dom.js";
import { deleteBooks } from "./delete-books.js";
import "./book-files.js"; // the book page's Files row (admins)
import { ICONS, type BookChange, type BookState } from "./reading.js";

interface StatusResponse {
    status: string;
    status_source: string;
}

const PREFETCH_TTL_MS = 30_000; // like the book page's own max-age
const prefetched = new Map<string, { html: Promise<string | null>; expires: number }>();

function fetchControls(uuid: string): Promise<string | null> {
    return fetch(`/book/me/${encodeURIComponent(uuid)}`, { credentials: "same-origin" })
        .then((res) => (res.ok ? res.text() : null))
        .catch(() => null); // offline: the throbber stays
}

/** Start fetching a book's controls, for the next time they're shown. */
export function prefetchControls(uuid: string): void {
    const now = Date.now();
    for (const [k, v] of prefetched) if (v.expires <= now) prefetched.delete(k);
    if (prefetched.has(uuid)) return;
    prefetched.set(uuid, { html: fetchControls(uuid), expires: now + PREFETCH_TTL_MS });
}

/** Forget a prefetch of a book's controls (its state changed elsewhere). */
export function forgetControls(uuid: string): void {
    prefetched.delete(uuid);
}

/** (Re)load the controls into their container. A prefetch is used once; fresh skips it. */
async function loadControls(container: HTMLElement, fresh = false): Promise<void> {
    const uuid = container.dataset["uuid"] ?? "";
    const hit = prefetched.get(uuid);
    prefetched.delete(uuid);
    const html = await (!fresh && hit && hit.expires > Date.now() ? hit.html : fetchControls(uuid));
    if (html === null || !container.isConnected) return;
    container.innerHTML = html;
    reportState(container, uuid);
}

/** Tell the grid what the server says about the book, as the controls just loaded show it. */
function reportState(container: HTMLElement, uuid: string): void {
    const controls = container.querySelector<HTMLElement>(".book-user-controls");
    if (!controls) return;
    const star = controls.querySelector(".btn-favorite");
    const detail: BookState = {
        uuid,
        signedIn: star !== null,
        favorite: star?.getAttribute("aria-pressed") === "true",
        status: controls.querySelector<HTMLSelectElement>(".book-status")?.value ?? "",
    };
    document.dispatchEvent(new CustomEvent<BookState>("cf:book-state", { detail }));
}

/** Tell the grid about a change saved here, for it to apply to its marks. */
function reportChange(change: BookChange): void {
    document.dispatchEvent(new CustomEvent<BookChange>("cf:status", { detail: change }));
}

/** Paint the Want to read button for a status. */
function paintWant(btn: HTMLButtonElement, status: string): void {
    const toggle = status === "" || status === "wanted";
    const on = status === "wanted";
    const icon = status === "reading" ? ICONS.read : status === "finished" ? ICONS.done : on ? ICONS.wanted : ICONS.want;
    const label = status === "reading" ? "Reading" : status === "finished" ? "Finished" : "Want to read";
    btn.innerHTML = `${icon}<span>${label}</span>`;
    btn.classList.toggle("is-on", status !== "");
    if (toggle) btn.setAttribute("aria-pressed", String(on));
    else btn.removeAttribute("aria-pressed");
    btn.title = on ? "Remove from Want to read" : toggle ? "Add to Want to read" : "Change reading status";
}

function bind(controls: HTMLElement): void {
    if (controls.dataset["bound"]) return;
    controls.dataset["bound"] = "1";
    const uuid = controls.dataset["uuid"] ?? "";
    const container = controls.closest<HTMLElement>(".book-user-actions");

    const star = controls.querySelector<HTMLButtonElement>(".btn-favorite");
    star?.addEventListener("click", async () => {
        const want = star.getAttribute("aria-pressed") !== "true";
        const paint = (on: boolean): void => {
            star.setAttribute("aria-pressed", String(on));
            star.classList.toggle("is-favorite", on);
            star.textContent = `${on ? "★" : "☆"} Favorite`;
            star.title = on ? "Remove from favorites" : "Add to favorites";
        };
        paint(want); // optimistic
        try {
            await api("PUT", `/api/me/books/${uuid}/favorite`, { favorite: want });
            reportChange({ uuid, favorite: want });
        } catch {
            paint(!want);
        }
    });

    const status = controls.querySelector<HTMLSelectElement>(".book-status");
    const want = controls.querySelector<HTMLButtonElement>(".btn-want");
    const note = controls.querySelector<HTMLElement>(".book-status-note");
    let current = status?.value ?? "";
    if (want) paintWant(want, current);
    const save = async (value: string): Promise<void> => {
        if (!status) return;
        status.value = value; // optimistic
        if (want) paintWant(want, value);
        try {
            const res = await api<StatusResponse>("PUT", `/api/me/books/${uuid}/status`, { status: value });
            current = res.status;
            if (note) note.textContent = note.dataset["progress"] || (res.status && res.status_source === "auto" ? "set by the reader" : "");
            reportChange({ uuid, status: res.status });
        } catch {
            /* put back below */
        }
        status.value = current;
        if (want) paintWant(want, current);
    };
    status?.addEventListener("change", () => void save(status.value));
    // Like the cover's button: toggles Want to read for a book not started.
    // Once reading or finished it shows that, and opens the menu.
    want?.addEventListener("click", () => {
        if (!status) return;
        if (current === "" || current === "wanted") {
            void save(current === "wanted" ? "" : "wanted");
            return;
        }
        try {
            status.showPicker();
        } catch {
            status.focus(); // no showPicker for selects (older Safari)
        }
    });

    const count = controls.querySelector<HTMLElement>(".book-shelves-count");
    const paintCount = (): void => {
        const n = controls.querySelectorAll("input[data-shelf]:checked").length;
        if (count) count.textContent = n ? ` (${n})` : "";
    };
    controls.querySelectorAll<HTMLInputElement>("input[data-shelf]").forEach((box) =>
        box.addEventListener("change", async () => {
            try {
                await api(box.checked ? "PUT" : "DELETE", `/api/shelves/${box.dataset["shelf"]}/books/${uuid}`);
            } catch {
                box.checked = !box.checked;
            }
            paintCount();
        }),
    );

    // Create a personal shelf with this book on it.
    const input = controls.querySelector<HTMLInputElement>(".book-shelf-new");
    const create = async (): Promise<void> => {
        if (!input || !container) return;
        const name = input.value.trim();
        if (!name) return;
        try {
            const shelf = await api<{ id: number }>("POST", "/api/shelves", { name });
            await api("PUT", `/api/shelves/${shelf.id}/books/${uuid}`);
            await loadControls(container, true);
            container.querySelector(".book-shelves")?.setAttribute("open", "");
        } catch (err) {
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

function scan(root: Node): void {
    if (!(root instanceof HTMLElement)) return;
    // Controls that arrived.
    if (root.matches(".book-user-controls")) bind(root);
    root.querySelectorAll<HTMLElement>(".book-user-controls").forEach(bind);
    // Book pages that arrived: load their controls.
    const containers = root.matches(".book-user-actions") ? [root] : [...root.querySelectorAll<HTMLElement>(".book-user-actions")];
    for (const c of containers) {
        if (c.dataset["loading"]) continue;
        c.dataset["loading"] = "1";
        void loadControls(c);
    }
}

new MutationObserver((records) => {
    for (const r of records) for (const n of r.addedNodes) scan(n);
}).observe(document.documentElement, { childList: true, subtree: true });

/**
 * Coming back to a book page or popup that's still showing (Back from the
 * reader restores the page as it was; another tab was in front): what was
 * read meanwhile changed its status and progress, so load the controls
 * again, fresh.
 */
const REFRESH_AFTER_MS = 5_000; // hidden at least this long
let hiddenAt = 0;
function refreshShownControls(): void {
    prefetched.clear();
    for (const c of document.querySelectorAll<HTMLElement>(".book-user-actions")) void loadControls(c, true);
}
window.addEventListener("pageshow", (e) => {
    if (e.persisted) refreshShownControls();
});
document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") hiddenAt = Date.now();
    else if (hiddenAt && Date.now() - hiddenAt >= REFRESH_AFTER_MS) refreshShownControls();
});

scan(document.documentElement);

// The book page's "Remove from library…" (admins): book.html, in the popup
// and on its own.
document.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>("[data-delete-book]") : null;
    if (!btn) return;
    const removed = await deleteBooks([
        { uuid: btn.dataset["deleteBook"] ?? "", title: btn.dataset["title"] ?? "", authors: btn.dataset["authors"] ?? "" },
    ]);
    if (!removed) return;
    // The popup's grid (without the #book/ entry), or the library.
    if (location.hash.startsWith("#book/")) location.replace(location.pathname + location.search);
    else location.href = "/";
});
