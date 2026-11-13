// The book page's Files row (admins, book.html): remove one format of a book
// to the Recycling Bin, or add new files to it (Go: recycle_formats.go,
// add_format.go). Removing the last file is removing the book, so that is
// the book's own removal, with a warning.

import { el, errorMessage } from "./dom.js";
import { deleteBooks } from "./delete-books.js";
import type { RecycleBatch, RestoreResponse } from "./types.js";

/** The book page again, fresh (?applied= skips its cache). */
function reloadBook(uuid: string): void {
    location.assign(`/book/get/${encodeURIComponent(uuid)}?applied=${Date.now()}`);
}

/** A request's error: the JSON's error, or the text. */
async function failure(res: Response): Promise<{ message: string; body: Record<string, unknown> }> {
    const text = (await res.text()).trim();
    try {
        const body = JSON.parse(text) as Record<string, unknown>;
        return { message: String(body["error"] ?? text), body };
    } catch {
        return { message: text || `HTTP ${res.status}`, body: {} };
    }
}

/** Ask, remove the format, and offer Undo. Resolves true when the book changed. */
function removeFormat(uuid: string, title: string, format: string, others: string[]): Promise<boolean> {
    return new Promise((resolve) => {
        const ext = format.toUpperCase();
        let changed = false;
        const heading = el("h2", { id: "remove-format-title" }, `Remove the ${ext} from “${title}”?`);
        const warning = el(
            "div",
            { className: "delete-warning" },
            el("p", {}, `The ${ext} file leaves the book and the library folder; the book keeps its ${others.map((f) => f.toUpperCase()).join(", ")}.`),
            el("p", {}, `It goes to the Recycling Bin, with readers' places, bookmarks and highlights in it, and can be restored from there until the server's owner empties it.`),
        );
        const status = el("p", { className: "auth-error", role: "alert", hidden: true });
        const cancel = el("button", { type: "button", className: "btn-action" }, "Cancel");
        const go = el("button", { type: "submit", className: "btn-action btn-danger" }, `Remove ${ext}`);
        const actions = el("div", { className: "delete-actions" }, cancel, go);
        const form = el("form", { method: "dialog" }, heading, warning, status, actions);
        const dialog = el("dialog", { className: "delete-dialog" }, form);
        dialog.setAttribute("aria-labelledby", "remove-format-title");
        const fail = (msg: string): void => {
            status.hidden = false;
            status.textContent = msg;
        };
        cancel.addEventListener("click", () => dialog.close());
        dialog.addEventListener("close", () => {
            dialog.remove();
            resolve(changed);
        });

        const done = (batch: RecycleBatch): void => {
            changed = true;
            heading.textContent = `Moved the ${ext} to the Recycling Bin`;
            warning.replaceChildren(
                el("p", {}, "Removed by mistake? Undo puts it back now, or restore it from the ", el("a", { href: "/recycle-bin" }, "Recycling Bin"), " later."),
            );
            const undo = el("button", { type: "button", className: "btn-action" }, "Undo");
            const ok = el("button", { type: "button", className: "btn-action btn-action-read" }, "Done");
            actions.replaceChildren(undo, ok);
            ok.addEventListener("click", () => dialog.close());
            undo.addEventListener("click", async () => {
                undo.disabled = ok.disabled = true;
                status.hidden = true;
                try {
                    const res = await fetch(`/api/recycle-bin/${encodeURIComponent(batch.id)}/restore`, {
                        method: "POST",
                        headers: { "content-type": "application/json" },
                        body: JSON.stringify({ uuids: [], formats: [] }),
                    });
                    if (!res.ok) throw new Error((await failure(res)).message);
                    const out = (await res.json()) as RestoreResponse;
                    const failed = out.results.filter((r) => r.error);
                    if (failed.length) throw new Error(failed.map((r) => r.error).join("\n"));
                    heading.textContent = `Restored the ${ext}`;
                    warning.replaceChildren(el("p", {}, "The book has it again."));
                    actions.replaceChildren(ok);
                } catch (err) {
                    fail(errorMessage(err));
                }
                ok.disabled = false;
                ok.focus();
            });
            ok.focus();
        };

        form.addEventListener("submit", async (e) => {
            e.preventDefault();
            go.disabled = cancel.disabled = true;
            status.hidden = true;
            try {
                const res = await fetch(`/api/library/books/${encodeURIComponent(uuid)}/formats/${encodeURIComponent(format)}/remove`, {
                    method: "POST",
                });
                if (!res.ok) {
                    const f = await failure(res);
                    if (f.body["last"]) {
                        // The server sees no other file: it's the book's removal.
                        dialog.close();
                        return;
                    }
                    throw new Error(f.message);
                }
                done((await res.json()) as RecycleBatch);
            } catch (err) {
                fail(errorMessage(err));
                go.disabled = cancel.disabled = false;
            }
        });

        document.body.append(dialog);
        dialog.showModal();
        cancel.focus();
    });
}

document.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>("[data-remove-format]") : null;
    const row = btn?.closest<HTMLElement>("[data-book-files]");
    if (!btn || !row) return;
    const uuid = row.dataset["bookFiles"] ?? "";
    const title = row.dataset["title"] ?? "";
    const format = btn.dataset["removeFormat"] ?? "";
    const others = [...row.querySelectorAll<HTMLElement>("[data-remove-format]")]
        .map((b) => b.dataset["removeFormat"] ?? "")
        .filter((f) => f && f !== format);
    if (others.length === 0) {
        // The last file: removing it removes the book.
        const removed = await deleteBooks([{ uuid, title, authors: row.dataset["authors"] ?? "" }], {
            note: `The ${format.toUpperCase()} is this book's only file: removing it removes the whole book from the library.`,
        });
        if (!removed) return;
        if (location.hash.startsWith("#book/")) location.replace(location.pathname + location.search);
        else location.href = "/";
        return;
    }
    if (await removeFormat(uuid, title, format, others)) reloadBook(uuid);
});

document.addEventListener("change", async (e) => {
    const input = e.target instanceof HTMLInputElement && e.target.matches("[data-add-files]") ? e.target : null;
    const row = input?.closest<HTMLElement>("[data-book-files]");
    if (!input || !row || !input.files?.length) return;
    const uuid = row.dataset["bookFiles"] ?? "";
    const status = row.querySelector<HTMLElement>(".book-files-status");
    const say = (text: string, error = false): void => {
        if (!status) return;
        status.textContent = text;
        status.classList.toggle("auth-error-text", error);
    };
    const body = new FormData();
    for (const f of input.files) body.append("files", f, f.name);
    input.disabled = true;
    say(`Uploading ${input.files.length === 1 ? input.files[0]!.name : `${input.files.length} files`}…`);
    try {
        const res = await fetch(`/book/add-format/${encodeURIComponent(uuid)}`, { method: "POST", body, credentials: "same-origin" });
        if (!res.ok) throw new Error((await failure(res)).message);
        const out = (await res.json()) as { url: string; same?: string[] };
        // Review the new files against the book (the merge editor).
        location.assign(out.url);
    } catch (err) {
        say(`Not added: ${errorMessage(err)}`, true);
    } finally {
        input.disabled = false;
        input.value = "";
    }
});
