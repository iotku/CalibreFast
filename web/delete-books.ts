// Removing books from the library (Go: recycle.go). The books go to the
// Recycling Bin, from which an admin can restore them until the server's
// owner empties it; nothing here can empty it.
//
// The confirmation can't be clicked through: it lists the books and the
// button only works once "delete" is typed. Afterwards the same dialog
// offers Undo, which restores the batch straight away.

import { api, el, errorMessage } from "./dom.js";
import type { RecycleBatch, RestoreResponse } from "./types.js";

export interface BookToDelete {
    uuid: string;
    title: string;
    authors: string;
}

const CONFIRM_WORD = "delete";
const LISTED = 50; // titles shown; the rest are counted

function plural(n: number, one: string, many = `${one}s`): string {
    return `${n} ${n === 1 ? one : many}`;
}

/**
 * Ask, remove, and offer Undo. Resolves true when the library changed (the
 * books were removed and not put back), so the caller can reload.
 */
export function deleteBooks(books: BookToDelete[]): Promise<boolean> {
    return new Promise((resolve) => {
        if (books.length === 0) {
            resolve(false);
            return;
        }
        let changed = false;
        const n = books.length;
        const what = n === 1 ? `“${books[0]!.title}”` : plural(n, "book");

        const title = el("h2", { id: "delete-title" }, `Remove ${what} from the library?`);
        const list = el(
            "ul",
            { className: "delete-list" },
            ...books.slice(0, LISTED).map((b) => el("li", {}, el("strong", {}, b.title || "Untitled"), b.authors ? ` — ${b.authors}` : "")),
            ...(n > LISTED ? [el("li", { className: "auth-muted" }, `…and ${plural(n - LISTED, "more book")}`)] : []),
        );
        const warning = el(
            "div",
            { className: "delete-warning" },
            el("p", {}, `${n === 1 ? "This book disappears" : `These ${n} books disappear`} from the library for everyone: nobody can find, read or download ${n === 1 ? "it" : "them"}, and ${n === 1 ? "its" : "their"} files leave the library folder.`),
            el("p", {}, "They are moved to the Recycling Bin, with a copy of metadata.db from just before, and can be restored from there until the server's owner empties it. Readers' bookmarks, highlights, notes and places are kept and come back with them."),
        );
        const input = el("input", { type: "text", autocomplete: "off", spellcheck: false, ariaLabel: `Type ${CONFIRM_WORD} to confirm` });
        input.setAttribute("autocapitalize", "off");
        const label = el("label", { className: "delete-confirm" }, `Type `, el("code", {}, CONFIRM_WORD), ` to confirm`, input);
        const status = el("p", { className: "auth-error", role: "alert", hidden: true });
        const cancel = el("button", { type: "button", className: "btn-action" }, "Cancel");
        const go = el("button", { type: "submit", className: "btn-action btn-danger", disabled: true }, n === 1 ? "Remove book" : `Remove ${n} books`);
        const actions = el("div", { className: "delete-actions" }, cancel, go);
        const form = el("form", { method: "dialog" }, title, list, warning, label, status, actions);
        const dialog = el("dialog", { className: "delete-dialog" }, form);
        dialog.setAttribute("aria-labelledby", "delete-title");

        const fail = (err: unknown): void => {
            status.hidden = false;
            status.textContent = errorMessage(err);
        };
        input.addEventListener("input", () => {
            go.disabled = input.value.trim().toLowerCase() !== CONFIRM_WORD;
        });
        cancel.addEventListener("click", () => dialog.close());
        dialog.addEventListener("close", () => {
            dialog.remove();
            resolve(changed);
        });

        // After removing: say so, and offer to put them straight back.
        const done = (batch: RecycleBatch): void => {
            changed = true;
            title.textContent = `Moved ${n === 1 ? what : plural(n, "book")} to the Recycling Bin`;
            const binLink = el("a", { href: "/recycle-bin" }, "Recycling Bin");
            warning.replaceChildren(el("p", {}, "Removed by mistake? Undo puts everything back now, or restore from the ", binLink, " later."));
            label.remove();
            const undo = el("button", { type: "button", className: "btn-action" }, "Undo");
            const ok = el("button", { type: "button", className: "btn-action btn-action-read" }, "Done");
            actions.replaceChildren(undo, ok);
            ok.addEventListener("click", () => dialog.close());
            undo.addEventListener("click", async () => {
                undo.disabled = ok.disabled = true;
                status.hidden = true;
                try {
                    const res = await api<RestoreResponse>("POST", `/api/recycle-bin/${encodeURIComponent(batch.id)}/restore`, { uuids: [] });
                    const failed = res.results.filter((r) => r.error);
                    if (failed.length) throw new Error(failed.map((r) => `${r.title}: ${r.error}`).join("\n"));
                    changed = false;
                    title.textContent = n === 1 ? `Restored ${what}` : `Restored ${plural(n, "book")}`;
                    warning.replaceChildren(el("p", {}, "Everything is back in the library."));
                    actions.replaceChildren(ok);
                } catch (err) {
                    fail(err); // some or all are still in the bin: the library changed
                }
                ok.disabled = false;
                ok.focus();
            });
            ok.focus();
        };

        form.addEventListener("submit", async (e) => {
            e.preventDefault();
            if (go.disabled) return;
            go.disabled = cancel.disabled = input.disabled = true;
            status.hidden = true;
            try {
                done(await api<RecycleBatch>("POST", "/api/library/delete", { uuids: books.map((b) => b.uuid) }));
            } catch (err) {
                fail(err);
                cancel.disabled = input.disabled = false;
                go.disabled = input.value.trim().toLowerCase() !== CONFIRM_WORD;
            }
        });

        document.body.append(dialog);
        dialog.showModal();
        cancel.focus(); // Enter cancels until the word is typed
    });
}
