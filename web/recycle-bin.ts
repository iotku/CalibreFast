// The Recycling Bin (/recycle-bin): books removed from the library, by
// removal, newest first (Go: recycle.go). Each can be restored, or a whole
// removal at once. Emptying the bin is only possible on the server.

import { api, byId, el, errorMessage, statusLine } from "./dom.js";
import { formatBytes } from "./cover-sources.js";
import type { RecycleBatch, RecycleBin, RecycledBook, RestoreResponse } from "./types.js";

const say = statusLine("bin-status");
const list = byId("bin-list");
const empty = byId("bin-empty");

function when(iso: string | undefined): string {
    const d = new Date(iso ?? "");
    return isNaN(d.getTime()) ? "" : d.toLocaleString();
}

async function restore(batch: RecycleBatch, books: RecycledBook[], buttons: HTMLButtonElement[]): Promise<void> {
    for (const b of buttons) b.disabled = true;
    try {
        const res = await api<RestoreResponse>("POST", `/api/recycle-bin/${encodeURIComponent(batch.id)}/restore`, {
            uuids: books.map((b) => b.uuid),
        });
        const failed = res.results.filter((r) => r.error);
        const ok = res.results.length - failed.length;
        if (failed.length) say(`${ok ? `Restored ${ok}. ` : ""}Couldn't restore ${failed.map((r) => `“${r.title || r.uuid}”: ${r.error}`).join("; ")}`, true);
        else say(ok === 1 ? `Restored “${res.results[0]!.title}”.` : `Restored ${ok} books.`);
    } catch (err) {
        say(errorMessage(err), true);
    }
    await refresh();
}

function bookRow(batch: RecycleBatch, b: RecycledBook): HTMLLIElement {
    const details = [b.formats.map((f) => f.toUpperCase()).join(", "), b.no_files ? "no files" : formatBytes(b.size)].filter(Boolean).join(" · ");
    let action: HTMLElement;
    if (b.restored_at) {
        action = el("span", { className: "auth-muted" }, `Restored ${when(b.restored_at)}${b.restored_by ? ` by ${b.restored_by}` : ""}`);
    } else if (!b.in_bin) {
        action = el("span", { className: "auth-error-text" }, "Files emptied from the bin");
    } else {
        const btn = el("button", { type: "button", className: "btn-standard" }, "Restore");
        btn.addEventListener("click", () => void restore(batch, [b], [btn]));
        action = btn;
    }
    return el(
        "li",
        { className: "import-row bin-row" + (b.in_bin ? "" : " bin-row-gone") },
        el(
            "div",
            { className: "import-info" },
            b.in_bin || !b.restored_at
                ? el("span", { className: "import-title" }, b.title || "Untitled")
                : el("a", { className: "import-title", href: `/book/get/${encodeURIComponent(b.uuid)}` }, b.title || "Untitled"),
            el("div", { className: "import-authors" }, b.authors),
            el("div", { className: "auth-muted import-files", title: b.path }, details),
        ),
        el("div", { className: "import-actions" }, action),
    );
}

function batchSection(batch: RecycleBatch): HTMLElement {
    const waiting = batch.books.filter((b) => b.in_bin);
    const head = el(
        "div",
        { className: "bin-batch-head" },
        el("h2", {}, `Removed ${when(batch.deleted_at)}`),
        el("span", { className: "auth-muted" }, `by ${batch.deleted_by || "?"} · ${batch.books.length} book${batch.books.length === 1 ? "" : "s"}`),
    );
    const rows = batch.books.map((b) => bookRow(batch, b));
    if (waiting.length > 1) {
        const all = el("button", { type: "button", className: "btn-standard" }, `Restore all ${waiting.length}`);
        all.addEventListener("click", () => {
            const buttons = [all, ...rows.flatMap((r) => [...r.querySelectorAll("button")])];
            void restore(batch, waiting, buttons);
        });
        head.append(all);
    }
    return el("section", { className: "bin-batch" }, head, el("ul", { className: "import-list" }, ...rows));
}

async function refresh(): Promise<void> {
    try {
        const bin = await api<RecycleBin>("GET", "/api/recycle-bin");
        byId("bin-folder").textContent = bin.folder;
        list.replaceChildren(...bin.batches.map(batchSection));
        empty.hidden = bin.batches.length > 0;
    } catch (err) {
        say(`Couldn't load the Recycling Bin: ${errorMessage(err)}`, true);
    }
}

void refresh();
