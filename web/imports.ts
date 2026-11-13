// The import queue (/import): books the watch folder took in, waiting for an
// admin to review them (Go: imports.go). Each row opens the review page, the
// metadata editor pointed at that book; Discard deletes its files. The list
// follows the folder: it's asked for again every few seconds while the page
// is in view.

import { api, byId, el, errorMessage, statusLine } from "./dom.js";
import { formatBytes } from "./cover-sources.js";
import type { DuplicateCheck, ImportQueue, ImportSummary } from "./types.js";

const POLL_MS = 5000;

const say = statusLine("imports-status");
const list = byId("imports-list");
const empty = byId("imports-empty");
const folder = byId("imports-folder");
const count = byId("imports-count");

/** The row's warning that the book may already be in the library. */
function duplicateNote(dup: DuplicateCheck): HTMLElement {
    const sameFile = dup.books.some((b) => b.same_file);
    const note = el(
        "div",
        { className: `import-duplicate${sameFile ? " import-duplicate-same" : ""}` },
        sameFile ? "Already in the library (identical file): " : "Possibly already in the library: ",
    );
    dup.books.forEach((b, i) => {
        if (i) note.append(", ");
        const by = (b.authors ?? []).join(" & ");
        note.append(el("a", { href: `/book/get/${encodeURIComponent(b.uuid)}`, target: "_blank" }, `“${b.title}”${by ? ` by ${by}` : ""}`));
    });
    return note;
}

function row(item: ImportSummary): HTMLLIElement {
    const review = `/import/${encodeURIComponent(item.id)}`;
    const cover = item.has_cover
        ? el("img", { className: "import-cover", src: `${review}/cover?t=${Date.parse(item.added) || 0}`, alt: "", loading: "lazy" })
        : el("span", { className: "import-cover import-cover-none", title: "No cover yet: the review page picks one from the book" }, item.files[0]?.format ?? "");
    const files = item.files.map((f) => `${f.name} (${formatBytes(f.size)})`).join(" · ");
    const added = new Date(item.added);
    const discard = el("button", { type: "button", className: "btn-standard import-discard" }, "Discard");
    discard.addEventListener("click", async () => {
        const names = item.files.map((f) => f.name).join(", ");
        if (!confirm(`Discard “${item.title}”?\n\nThis deletes ${names} from the import queue. It can't be undone.`)) return;
        discard.disabled = true;
        try {
            await api("POST", `${review}/discard`);
            say(`Discarded “${item.title}”.`);
            await refresh();
        } catch (err) {
            discard.disabled = false;
            say(errorMessage(err), true);
        }
    });
    return el(
        "li",
        { className: "import-row" },
        el("a", { href: review, className: "import-cover-link", tabIndex: -1, ariaHidden: "true" }, cover),
        el(
            "div",
            { className: "import-info" },
            el("a", { href: review, className: "import-title" }, item.title || "Untitled"),
            el("div", { className: "import-authors" }, (item.authors ?? []).join(" & ")),
            el("div", { className: "auth-muted import-files", title: files }, files),
            el("div", { className: "auth-muted" }, `${item.from ? `From ${item.from} · ` : ""}Added ${isNaN(added.getTime()) ? "" : added.toLocaleString()}`),
            ...(item.into
                ? [el("div", { className: "duplicate-note" }, "New file for ", el("a", { href: `/book/get/${encodeURIComponent(item.into)}` }, `“${item.into_title || "a library book"}”`), ": review it to add it to that book.")]
                : []),
            ...(item.duplicate?.books.length ? [duplicateNote(item.duplicate)] : []),
        ),
        el("div", { className: "import-actions" }, el("a", { href: review, className: "btn-standard" }, "Review"), discard),
    );
}

let shown = "";
function render(q: ImportQueue): void {
    const json = JSON.stringify(q);
    if (json === shown) return; // nothing new: leave the page (and focus) alone
    shown = json;

    const n = q.items.length;
    count.textContent = n ? `${n} book${n === 1 ? "" : "s"} waiting for review` : "Nothing waiting for review";
    list.replaceChildren(...q.items.map(row));
    list.hidden = n === 0;
    empty.hidden = n > 0;

    const parts: string[] = [];
    if (q.watching) {
        parts.push(`Watching ${q.folder ?? "the watch folder"}: books put there appear here ${q.polling ? "within a minute (the folder is checked every 30 seconds)" : "within a few seconds"}.`);
        if (q.settling) {
            parts.push(q.problem
                ? `${q.settling} file${q.settling === 1 ? "" : "s"} there can't be taken.`
                : `${q.settling} file${q.settling === 1 ? " is" : "s are"} still arriving.`);
        }
    } else {
        parts.push("No folder is being watched: start the server with -watch-dir and -allow-writes.");
    }
    folder.textContent = parts.join(" ");
    const problem = byId("imports-problem");
    problem.hidden = !q.problem;
    problem.textContent = q.problem ? `Watch folder: ${q.problem}` : "";
}

async function refresh(): Promise<void> {
    try {
        render(await api<ImportQueue>("GET", "/api/imports"));
    } catch (err) {
        say(`Couldn't load the queue: ${errorMessage(err)}`, true);
    }
}

void refresh();
setInterval(() => {
    if (document.visibilityState === "visible") void refresh();
}, POLL_MS);
document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void refresh();
});
