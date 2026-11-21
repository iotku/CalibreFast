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
 *
 * A slow or missing server doesn't hold them up: if they haven't arrived
 * shortly, the favorite and status controls are built here from the marks
 * kept on this device (reading.ts), and replaced by the server's once they
 * come (shelves need the server). Favorite and status changes are noted
 * and sent now or once the server answers (pending-marks.ts, send-pending.ts),
 * and laid over whatever the server renders until it has them.
 */

import { startBookSync } from "./book-sync.js";
import { api, el, errorMessage } from "./dom.js";
import { serverHtml } from "./trusted.js";
import { deleteBooks } from "./delete-books.js";
import "./book-files.js"; // the book page's Files row (admins)
import "./series-shelf.js"; // "Add series to shelf" (also the series page's, via app-htmx.ts)
import "./offline-control.js"; // "On this device" and keeping the book (book.html)
import "./read-link.js"; // the cover opens the format last read (book.html)
import { ICONS, cachedMarks, changeWaits, statusOf, type BookChange, type BookState } from "./reading.js";
import { lastKnownUser } from "./local-keys.js";
import { noteMark, pendingMark } from "./pending-marks.js";
import { sendPending } from "./send-pending.js";
import { onServerBack } from "./server-watch.js";

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

/** How long the server's controls are waited for before the ones built here show. */
const LOCAL_AFTER_MS = 300;

/** (Re)load the controls into their container. A prefetch is used once; fresh skips it. */
async function loadControls(container: HTMLElement, fresh = false): Promise<void> {
    const uuid = container.dataset["uuid"] ?? "";
    const hit = prefetched.get(uuid);
    prefetched.delete(uuid);
    const answer = !fresh && hit && hit.expires > Date.now() ? hit.html : fetchControls(uuid);
    const timer = setTimeout(() => showLocal(container, uuid), LOCAL_AFTER_MS);
    const html = await answer;
    clearTimeout(timer);
    if (!container.isConnected) return;
    if (html === null) {
        showLocal(container, uuid); // offline: what this device knows
        return;
    }
    serverHtml(container, html); // html/template output from our server
    const controls = container.querySelector<HTMLElement>(".book-user-controls");
    if (controls) applyPending(controls, uuid);
    reportState(container, uuid);
}

/**
 * The favorite and status controls as this device knows them (cached marks,
 * with what waits laid over), while the server's are slow or can't come.
 * Same markup as book-user-actions.html, so bind() wires them up the same.
 * Only when the marks say someone is signed in and nothing is showing yet.
 */
function showLocal(container: HTMLElement, uuid: string): void {
    const m = cachedMarks();
    if (!container.isConnected || !m.signedIn || container.querySelector(".book-user-controls")) return;
    const fav = m.favorites.has(uuid);
    const status = statusOf(m, uuid);
    const star = el("button", { type: "button", className: `btn-action btn-favorite${fav ? " is-favorite" : ""}` });
    paintStar(star, fav);
    const select = el("select", { className: "book-status", title: "Reading status" });
    select.setAttribute("aria-label", "Reading status");
    for (const [value, label] of [["", "Not reading"], ["wanted", "Saved"], ["reading", "Reading"], ["finished", "Finished"]] as const) {
        select.append(el("option", { value, textContent: label, selected: value === status }));
    }
    const read = m.reading.get(uuid);
    const progress = status === "reading" && read != null ? `${Math.round(read * 100)}% read` : "";
    const note = el("span", { className: "book-status-note", textContent: progress });
    note.dataset["progress"] = progress;
    const shelves = el(
        "details",
        { className: "book-shelves" },
        el("summary", { textContent: "Shelves" }),
        el("div", { className: "book-shelves-list" }, el("p", { className: "book-shelves-wait", textContent: "Shelves show once the server answers." })),
    );
    const controls = el(
        "div",
        { className: "book-user-controls" },
        el("div", { className: "book-user-row" }, star, el("div", { className: "book-status-split" }, el("button", { type: "button", className: "btn-action btn-want" }), select)),
        note,
        shelves,
    );
    controls.dataset["uuid"] = uuid;
    controls.dataset["local"] = "1";
    container.replaceChildren(controls);
}

/** Paint the favorite button. */
function paintStar(star: HTMLButtonElement, on: boolean): void {
    star.setAttribute("aria-pressed", String(on));
    star.classList.toggle("is-favorite", on);
    star.textContent = `${on ? "★" : "☆"} Favorite`;
    star.title = on ? "Remove from favorites" : "Add to favorites";
}

/** Lay the changes the server doesn't have yet over its controls (before bind() reads them). */
function applyPending(controls: HTMLElement, uuid: string): void {
    const p = pendingMark(lastKnownUser(), uuid);
    if (!p) return;
    const star = controls.querySelector<HTMLButtonElement>(".btn-favorite");
    if (star && p.favorite !== undefined) paintStar(star, p.favorite);
    const select = controls.querySelector<HTMLSelectElement>(".book-status");
    if (select && p.status !== undefined) {
        select.value = p.status;
        const note = controls.querySelector<HTMLElement>(".book-status-note");
        if (note) note.textContent = note.dataset["progress"] ?? "";
    }
}

// A change the server took, or turned down for a later one made elsewhere
// (pending-marks.ts), or made on a cover: shown controls follow, unless a
// newer change to that field still waits here.
document.addEventListener("cf:status", (e) => {
    const c = (e as CustomEvent<BookChange>).detail;
    const user = lastKnownUser();
    for (const controls of document.querySelectorAll<HTMLElement>(".book-user-controls")) {
        if (controls.dataset["uuid"] !== c.uuid) continue;
        const star = controls.querySelector<HTMLButtonElement>(".btn-favorite");
        if (star && c.favorite !== undefined && !changeWaits(user, c.uuid, "favorite")) paintStar(star, c.favorite);
        const select = controls.querySelector<HTMLSelectElement>(".book-status");
        if (select && c.status !== undefined && !changeWaits(user, c.uuid, "status")) {
            select.value = c.status;
            const btn = controls.querySelector<HTMLButtonElement>(".btn-want");
            if (btn) paintWant(btn, c.status);
        }
    }
});

// The server answers again: controls built here give way to its own (shelves too).
onServerBack(() => {
    for (const c of document.querySelectorAll<HTMLElement>(".book-user-controls[data-local]")) {
        const container = c.closest<HTMLElement>(".book-user-actions");
        if (container) void loadControls(container, true);
    }
});

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

/** Paint the Save button for a status ("wanted" is shown as Saved). */
function paintWant(btn: HTMLButtonElement, status: string): void {
    const toggle = status === "" || status === "wanted";
    const on = status === "wanted";
    const icon = status === "reading" ? ICONS.read : status === "finished" ? ICONS.done : on ? ICONS.wanted : ICONS.want;
    const label = status === "reading" ? "Reading" : status === "finished" ? "Finished" : on ? "Saved" : "Save";
    btn.replaceChildren(icon(), el("span", {}, label));
    btn.classList.toggle("is-on", status !== "");
    if (toggle) btn.setAttribute("aria-pressed", String(on));
    else btn.removeAttribute("aria-pressed");
    btn.title = on ? "Remove from Saved" : toggle ? "Save for later" : "Change reading status";
}

function bind(controls: HTMLElement): void {
    if (controls.dataset["bound"]) return;
    controls.dataset["bound"] = "1";
    const uuid = controls.dataset["uuid"] ?? "";
    const container = controls.closest<HTMLElement>(".book-user-actions");

    // Changes show at once and are noted, then sent (pending-marks.ts). Once
    // the server has one it says so ("cf:status"); one still waiting (slow,
    // or no server) is told to the grid here, for its marks.
    const user = lastKnownUser();
    const sendNoted = async (change: BookChange, field: "favorite" | "status"): Promise<void> => {
        if (!user) return;
        await sendPending(user);
        if (changeWaits(user, uuid, field)) reportChange(change);
    };

    const star = controls.querySelector<HTMLButtonElement>(".btn-favorite");
    star?.addEventListener("click", () => {
        if (!user) return;
        const want = star.getAttribute("aria-pressed") !== "true";
        paintStar(star, want);
        noteMark(user, uuid, { favorite: want });
        void sendNoted({ uuid, favorite: want }, "favorite");
    });

    const status = controls.querySelector<HTMLSelectElement>(".book-status");
    const want = controls.querySelector<HTMLButtonElement>(".btn-want");
    const note = controls.querySelector<HTMLElement>(".book-status-note");
    if (want && status) paintWant(want, status.value);
    const save = async (value: string): Promise<void> => {
        if (!status || !user) return;
        status.value = value;
        if (want) paintWant(want, value);
        if (note) note.textContent = note.dataset["progress"] ?? ""; // set here, not by the reader
        noteMark(user, uuid, { status: value });
        await sendNoted({ uuid, status: value }, "status");
    };
    status?.addEventListener("change", () => void save(status.value));
    // Like the cover's button: toggles Saved for a book not started.
    // Once reading or finished it shows that, and opens the menu.
    want?.addEventListener("click", () => {
        if (!status) return;
        if (status.value === "" || status.value === "wanted") {
            void save(status.value === "wanted" ? "" : "wanted");
            return;
        }
        try {
            status.showPicker();
        } catch {
            status.focus(); // no showPicker for selects (older Safari)
        }
    });

    // Track on Hardcover: this book is sent to the user's Hardcover account
    // only once they tick it (hardcover_link.go).
    const hc = controls.querySelector<HTMLInputElement>("input[data-hardcover-share]");
    const hcNote = controls.querySelector<HTMLElement>(".book-hardcover-note");
    hc?.addEventListener("change", async () => {
        const share = hc.checked;
        hc.disabled = true;
        try {
            const got = await api<{ share: boolean }>("PUT", `/api/me/books/${encodeURIComponent(uuid)}/hardcover`, { share });
            hc.checked = got.share;
            if (hcNote) {
                hcNote.textContent = got.share ? "Sent to your Hardcover account as it changes" : "Not sent any more. What was sent stays on Hardcover.";
            }
        } catch (err) {
            hc.checked = !share;
            if (hcNote) hcNote.textContent = errorMessage(err);
        } finally {
            hc.disabled = false;
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
        c.dataset["expect"] = expected(c.dataset["uuid"] ?? "");
        void loadControls(c);
    }
}

/**
 * What the controls will most likely show, from the grid's cached marks
 * (reading.ts): the sign-in hint, or the buttons and Shelves, with a
 * progress note under the cover for a book being read from a known place. The
 * placeholders in book.html take that much room until they arrive. A guess
 * that's wrong only costs the shift there'd have been anyway.
 */
function expected(uuid: string): string {
    const m = cachedMarks();
    if (!m.signedIn) return "out";
    return m.reading.get(uuid) != null ? "in note" : "in"; // a position: "Page 5 of 25 · synced …"
}

/* "+N more" on a book's tags and authors: shows the rest, for good. */
document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".more-toggle") : null;
    const list = btn?.closest(".more-list");
    if (!btn || !list) return;
    list.classList.add("show-all");
    btn.setAttribute("aria-expanded", "true");
});

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

// A library page: books for offline reading that this device is missing (book-sync.ts).
startBookSync();
