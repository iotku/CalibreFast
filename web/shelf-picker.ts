// "Add to shelf…" for the books chosen in a grid (select-books.ts): a dialog
// listing the shelves the user can add to (their own, those they help
// with, global ones for admins), and a new one. Picking a shelf adds the books in one request
// (POST /api/shelves/{id}/books, shelves.go).
//
// Shelf names are whatever their owners typed: shown with textContent only.

import { api, closeOnBackdropClick, el, errorMessage } from "./dom.js";

interface PickShelf {
    id: number;
    name: string;
    global: boolean;
    can_edit: boolean;
    /** May change its books: the user's own, global ones for admins, and those they help with. */
    can_add: boolean;
    owner?: string;
    count: number;
}

interface ShelvesResponse {
    mine: PickShelf[];
    global: PickShelf[];
    member?: PickShelf[];
}

/** Opens the dialog for these books; resolves with what was said to the user, or null if cancelled. */
export function addToShelf(uuids: string[]): Promise<string | null> {
    return new Promise((resolve) => {
        let result: string | null = null;
        const n = uuids.length;
        const books = n === 1 ? "1 book" : `${n} books`;

        const title = el("h2", { id: "shelf-pick-title" }, `Add ${books} to a shelf`);
        const list = el("ul", { className: "shelf-pick-list" }, el("li", { className: "auth-muted" }, "Loading shelves…"));
        const status = el("p", { className: "auth-error", role: "alert", hidden: true });
        const name = el("input", { type: "text", maxLength: 100, placeholder: "New shelf…", autocomplete: "off" });
        name.setAttribute("aria-label", "New shelf name");
        const create = el("button", { type: "submit", className: "btn-action" }, "Create and add");
        const createForm = el("form", { className: "shelf-pick-create" }, name, create);
        const cancel = el("button", { type: "button", className: "btn-action" }, "Cancel");
        const actions = el("div", { className: "dialog-actions" }, cancel);
        const dialog = el("dialog", { className: "dialog shelf-pick-dialog" }, title, list, createForm, status, actions);
        dialog.setAttribute("aria-labelledby", "shelf-pick-title");
        closeOnBackdropClick(dialog);

        const fail = (err: unknown): void => {
            status.textContent = errorMessage(err);
            status.hidden = false;
        };
        const busy = (on: boolean): void => {
            for (const b of dialog.querySelectorAll("button")) b.disabled = on;
            name.disabled = on;
        };

        async function addTo(id: number, shelfName: string): Promise<void> {
            busy(true);
            try {
                const res = await api<{ added: number }>("POST", `/api/shelves/${encodeURIComponent(String(id))}/books`, { uuids });
                const skipped = n - res.added;
                result = `Added ${res.added === 1 ? "1 book" : `${res.added} books`} to “${shelfName}”${skipped ? ` (${skipped} already on it)` : ""}.`;
                dialog.close();
            } catch (err) {
                fail(err);
                busy(false);
            }
        }

        cancel.addEventListener("click", () => dialog.close());
        createForm.addEventListener("submit", async (e) => {
            e.preventDefault();
            const shelfName = name.value.trim();
            if (!shelfName) return;
            busy(true);
            try {
                const shelf = await api<PickShelf>("POST", "/api/shelves", { name: shelfName });
                await addTo(shelf.id, shelf.name);
            } catch (err) {
                fail(err);
                busy(false);
            }
        });

        dialog.addEventListener("close", () => {
            dialog.remove();
            resolve(result);
        });
        document.body.append(dialog);
        dialog.showModal();

        api<ShelvesResponse>("GET", "/api/shelves").then(
            (res) => {
                const shelves = [...res.mine, ...(res.member ?? []), ...res.global].filter((s) => s.can_add);
                if (!shelves.length) {
                    list.replaceChildren(el("li", { className: "auth-muted" }, "No shelves yet: name a new one below."));
                    name.focus();
                    return;
                }
                list.replaceChildren(
                    ...shelves.map((s) => {
                        const b = el(
                            "button",
                            { type: "button", className: "shelf-pick" },
                            el("span", { className: "shelf-name" }, s.name),
                            el("span", { className: "shelf-count" }, `${s.count} ${s.count === 1 ? "book" : "books"}${s.global ? " · global" : s.owner ? ` · ${s.owner}'s` : ""}`),
                        );
                        b.addEventListener("click", () => void addTo(s.id, s.name));
                        return el("li", {}, b);
                    }),
                );
                list.querySelector<HTMLButtonElement>("button")?.focus();
            },
            (err: unknown) => {
                list.replaceChildren();
                fail(err);
            },
        );
    });
}
