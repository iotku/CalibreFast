/**
 * Behaviour for the per-user controls under a book's read/download buttons
 * (favorite, reading status, shelves). The controls are rendered by the
 * server (/book/me/<uuid>) and loaded by htmx with a throbber; this file only
 * wires up clicks. Where htmx isn't on the page (the standalone /book/get
 * page) it loads the fragment itself.
 */

import { api, errorMessage } from "./dom.js";

interface StatusResponse {
    status: string;
    status_source: string;
}

/** (Re)load the controls into their container. */
async function loadControls(container: HTMLElement): Promise<void> {
    try {
        const res = await fetch(`/book/me/${encodeURIComponent(container.dataset["uuid"] ?? "")}`, { credentials: "same-origin" });
        if (res.ok) container.innerHTML = await res.text();
    } catch {
        /* offline: leave as is */
    }
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
        } catch {
            paint(!want);
        }
    });

    const status = controls.querySelector<HTMLSelectElement>(".book-status");
    const note = controls.querySelector<HTMLElement>(".book-status-note");
    let current = status?.value ?? "";
    status?.addEventListener("change", async () => {
        try {
            const res = await api<StatusResponse>("PUT", `/api/me/books/${uuid}/status`, { status: status.value });
            current = res.status;
            if (note) note.textContent = res.status && res.status_source === "auto" ? "set by the reader" : "";
        } catch {
            status.value = current;
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
            await loadControls(container);
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
    // Controls that arrived (htmx swap, or our own load).
    if (root.matches(".book-user-controls")) bind(root);
    root.querySelectorAll<HTMLElement>(".book-user-controls").forEach(bind);
    // Without htmx (standalone book page) load the controls ourselves; with
    // htmx, step in only if it hasn't loaded them after a few seconds, so the
    // throbber can never spin forever.
    const containers = root.matches(".book-user-actions[hx-get]")
        ? [root]
        : [...root.querySelectorAll<HTMLElement>(".book-user-actions[hx-get]")];
    containers.forEach((c) => {
        if (c.dataset["watching"]) return;
        c.dataset["watching"] = "1";
        const load = (): void => {
            if (c.isConnected && c.querySelector(":scope > .throbber")) void loadControls(c);
        };
        if (typeof htmx === "undefined") load();
        else setTimeout(load, 3000);
    });
}

new MutationObserver((records) => {
    for (const r of records) for (const n of r.addedNodes) scan(n);
}).observe(document.documentElement, { childList: true, subtree: true });

scan(document.documentElement);
