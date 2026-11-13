// The Recycling Bin (/recycle-bin): books removed from the library, by
// removal, newest first (Go: recycle.go). Each can be restored, or a whole
// removal at once. Emptying the bin is only possible on the server.

import { api, byId, el, errorMessage, statusLine } from "./dom.js";
import { formatBytes } from "./cover-sources.js";
import type { RecycleBatch, RecycleBin, RecycledBook, RecycledFormat, RestoreResponse } from "./types.js";

const say = statusLine("bin-status");
const list = byId("bin-list");
const empty = byId("bin-empty");

function when(iso: string | undefined): string {
    const d = new Date(iso ?? "");
    return isNaN(d.getTime()) ? "" : d.toLocaleString();
}

async function restore(batch: RecycleBatch, books: RecycledBook[], buttons: HTMLButtonElement[], files: RecycledFormat[] = []): Promise<void> {
    for (const b of buttons) b.disabled = true;
    try {
        const res = await api<RestoreResponse>("POST", `/api/recycle-bin/${encodeURIComponent(batch.id)}/restore`, {
            uuids: books.map((b) => b.uuid),
            formats: files.map((f) => f.key),
        });
        const name = (r: RestoreResponse["results"][number]): string => `“${r.title || r.uuid}”${r.format ? `'s ${r.format}` : ""}`;
        const failed = res.results.filter((r) => r.error);
        const ok = res.results.length - failed.length;
        if (failed.length) say(`${ok ? `Restored ${ok}. ` : ""}Couldn't restore ${failed.map((r) => `${name(r)}: ${r.error}`).join("; ")}`, true);
        else say(ok === 1 ? `Restored ${name(res.results[0]!)}.` : `Restored ${ok} items.`);
    } catch (err) {
        say(errorMessage(err), true);
    }
    await refresh();
}

const READER_DATA: [string, string, string][] = [
    ["annotations", "highlight or note", "highlights and notes"],
    ["bookmarks", "bookmark", "bookmarks"],
    ["reading_progress", "reading position", "reading positions"],
];

/** "3 highlights and notes, 1 bookmark" kept for readers. */
function readerData(counts: Record<string, number> | undefined): string {
    return READER_DATA.filter(([k]) => counts?.[k])
        .map(([k, one, many]) => `${counts![k]} ${counts![k] === 1 ? one : many}`)
        .join(", ");
}

function bookRow(batch: RecycleBatch, b: RecycledBook): HTMLLIElement {
    const kept = readerData(b.reader_data);
    const details = [b.formats.map((f) => f.toUpperCase()).join(", "), b.no_files ? "no files" : formatBytes(b.size), kept && `readers' ${kept} kept`]
        .filter(Boolean)
        .join(" · ");
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

/** A single file taken from a book that stayed in the library. */
function formatRow(batch: RecycleBatch, f: RecycledFormat): HTMLLIElement {
    const kept = readerData(f.reader_data);
    const details = [
        f.replaced ? `${f.format} replaced by a new file` : `${f.format} removed`,
        f.file,
        formatBytes(f.size),
        kept && `readers' ${kept} kept with it`,
    ]
        .filter(Boolean)
        .join(" · ");
    let action: HTMLElement;
    if (f.restored_at) {
        action = el("span", { className: "auth-muted" }, `Restored ${when(f.restored_at)}${f.restored_by ? ` by ${f.restored_by}` : ""}`);
    } else if (!f.in_bin) {
        action = el("span", { className: "auth-error-text" }, "File emptied from the bin");
    } else {
        const btn = el("button", { type: "button", className: "btn-standard", title: `Put the ${f.format} back in the book (it mustn't have a ${f.format} now)` }, `Restore ${f.format}`);
        btn.addEventListener("click", () => void restore(batch, [], [btn], [f]));
        action = btn;
    }
    return el(
        "li",
        { className: "import-row bin-row" + (f.in_bin ? "" : " bin-row-gone") },
        el(
            "div",
            { className: "import-info" },
            el("a", { className: "import-title", href: `/book/get/${encodeURIComponent(f.uuid)}` }, f.title || "Untitled"),
            el("div", { className: "import-authors" }, f.authors),
            el("div", { className: "auth-muted import-files" }, details),
        ),
        el("div", { className: "import-actions" }, action),
    );
}

function batchSection(batch: RecycleBatch): HTMLElement {
    const files = batch.formats ?? [];
    const waiting = batch.books.filter((b) => b.in_bin);
    const waitingFiles = files.filter((f) => f.in_bin && !f.restored_at);
    const merged = batch.merged_into;
    const what = [
        batch.books.length ? `${batch.books.length} book${batch.books.length === 1 ? "" : "s"}` : "",
        files.length ? `${files.length} file${files.length === 1 ? "" : "s"}` : "",
    ]
        .filter(Boolean)
        .join(", ");
    const verb = merged?.added_files?.length ? "Files added" : merged ? "Merged" : batch.books.length ? "Removed" : "File removed";
    const head = el(
        "div",
        { className: "bin-batch-head" },
        el("h2", {}, `${verb} ${when(batch.deleted_at)}`),
        el("span", { className: "auth-muted" }, `by ${batch.deleted_by || "?"} · ${what}`),
    );
    const note = merged?.added_files?.length
        ? el(
              "p",
              { className: "auth-muted bin-merge-note" },
              `New ${merged.added_files.join(", ")} added to `,
              el("a", { href: `/book/get/${encodeURIComponent(merged.uuid)}` }, `“${merged.title}”`),
              ". The book's own files they replaced are here.",
          )
        : merged
        ? el(
              "p",
              { className: "auth-muted bin-merge-note" },
              "Merged into ",
              el("a", { href: `/book/get/${encodeURIComponent(merged.uuid)}` }, `“${merged.title}”`),
              ". Restoring one brings it back as its own book again; the merged book keeps the files it took.",
              ...(merged.replaced?.length
                  ? [` Its own files that the merge replaced (${merged.replaced.join(", ")}) are in this batch's replaced folder on the server.`]
                  : []),
          )
        : null;
    const rows = [...batch.books.map((b) => bookRow(batch, b)), ...files.map((f) => formatRow(batch, f))];
    if (waiting.length + waitingFiles.length > 1) {
        const all = el("button", { type: "button", className: "btn-standard" }, `Restore all ${waiting.length + waitingFiles.length}`);
        all.addEventListener("click", () => {
            const buttons = [all, ...rows.flatMap((r) => [...r.querySelectorAll("button")])];
            void restore(batch, waiting, buttons, waitingFiles);
        });
        head.append(all);
    }
    return el("section", { className: "bin-batch" }, head, ...(note ? [note] : []), el("ul", { className: "import-list" }, ...rows));
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
