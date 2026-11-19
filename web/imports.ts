// The import queue (/import): books the watch folder took in, waiting for an
// admin to review them (Go: imports.go). Each row opens the review page, the
// metadata editor pointed at that book; Discard deletes its files. The list
// follows the folder: it's asked for again every few seconds while the page
// is in view, and only the rows whose book changed are built again.

import { api, byId, el, errorMessage, maybeId, statusLine } from "./dom.js";
import { pendingImport } from "./book-target.js";
import { epubPages, formatBytes, pdfPages, type CoverChoice } from "./cover-sources.js";
import type { DuplicateCheck, ImportQueue, ImportSummary } from "./types.js";

const POLL_MS = 5000;
/** While books are being taken in or checked: they show up one by one. */
const BUSY_POLL_MS = 1500;

const say = statusLine("imports-status");
const list = byId("imports-list");
const empty = byId("imports-empty");
const folder = byId("imports-folder");
const count = byId("imports-count");
const discardExisting = byId("imports-discard-existing", HTMLButtonElement);
const acceptAll = maybeId("imports-accept-all", HTMLButtonElement); // admins only
/** Why the last mass import left a book waiting, by id. */
const notAdded = new Map<string, string>();
let queue: ImportQueue | null = null;
/** A mass import can add it: not new files for a book, checked, and not maybe in the library already. */
const addable = (it: ImportSummary): boolean => !it.into && !it.checking && !it.duplicate?.books.length;

/** The waiting book's files the library already holds, byte for byte. */
function identicalFiles(item: ImportSummary): Set<string> {
    return new Set(item.duplicate?.books.flatMap((b) => b.same_files ?? []) ?? []);
}

/** The row's warning that the book may already be in the library. */
function duplicateNote(item: ImportSummary, dup: DuplicateCheck): HTMLElement {
    const sameFile = dup.books.some((b) => b.same_file);
    const same = identicalFiles(item);
    // Only some of its formats: name them, the rest are new.
    const some = same.size > 0 && same.size < item.files.length
        ? item.files.filter((f) => same.has(f.name)).map((f) => f.format.toUpperCase()).join(", ")
        : "";
    const note = el(
        "div",
        { className: `import-duplicate${sameFile ? " import-duplicate-same" : ""}` },
        sameFile ? `Already in the library (identical ${some || "file"}): ` : "Possibly already in the library: ",
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
            ...(item.duplicate?.books.length ? [duplicateNote(item, item.duplicate)] : []),
            ...(item.checking ? [el("div", { className: "auth-muted" }, "Checking whether it's already in the library…")] : []),
            // Why the mass import left it, unless the row already says (new files for a book, a duplicate).
            ...(notAdded.has(item.id) && !item.into && !item.duplicate?.books.length ? [el("div", { className: "import-duplicate import-duplicate-same" }, `Not mass imported: ${notAdded.get(item.id)}`)] : []),
        ),
        el("div", { className: "import-actions" }, el("a", { href: review, className: "btn-standard" }, "Review"), discard),
    );
}

/** The rows shown, by id, with what each was built from. */
const rows = new Map<string, { key: string; li: HTMLLIElement }>();

/** Puts these rows in the list, in this order, moving only what's out of place (focus and loaded covers stay). */
function placeRows(lis: HTMLLIElement[]): void {
    let prev: ChildNode | null = null;
    for (const li of lis) {
        const at: ChildNode | null = prev ? prev.nextSibling : list.firstChild;
        if (at !== li) list.insertBefore(li, at);
        prev = li;
    }
    for (let extra = prev ? prev.nextSibling : list.firstChild; extra; extra = prev ? prev.nextSibling : list.firstChild) {
        extra.remove();
    }
}

/** Whether the server is still at work on the queue: taking books in, or checking them. */
const busy = (q: ImportQueue | null): boolean => !!q && (q.staging > 0 || q.items.some((it) => it.checking));

function render(q: ImportQueue): void {
    queue = q;
    const seen = new Set<string>();
    const lis = q.items.map((item) => {
        seen.add(item.id);
        const key = JSON.stringify(item) + (notAdded.get(item.id) ?? "");
        let r = rows.get(item.id);
        if (r?.key !== key) {
            r = { key, li: row(item) };
            rows.set(item.id, r);
        }
        return r.li;
    });
    for (const id of [...rows.keys()]) if (!seen.has(id)) rows.delete(id);
    placeRows(lis);

    const n = q.items.length;
    count.textContent = n ? `${n} book${n === 1 ? "" : "s"} waiting for review` : "Nothing waiting for review";
    list.hidden = n === 0;
    empty.hidden = n > 0 || q.staging > 0;
    const same = q.items.reduce((sum, it) => sum + (it.into ? 0 : identicalFiles(it).size), 0);
    discardExisting.hidden = same === 0;
    if (acceptAll) acceptAll.hidden = !q.items.some(addable);
    discardExisting.title = `Discard the ${same} file${same === 1 ? "" : "s"} the library already has, byte for byte. A book's other formats stay, as new files for the book that has the rest.`;

    const parts: string[] = [];
    if (q.watching) {
        parts.push(`Watching ${q.folder ?? "the watch folder"}: books put there appear here ${q.polling ? "within a minute (the folder is checked every 30 seconds)" : "within a few seconds"}.`);
        if (q.staging) parts.push(`Taking in ${q.staging} book${q.staging === 1 ? "" : "s"}…`);
        // Counted at the end of a scan: while one takes books in, those are among them.
        if (q.settling && !q.staging) {
            parts.push(q.problem
                ? `${q.settling} file${q.settling === 1 ? "" : "s"} there can't be taken.`
                : `${q.settling} file${q.settling === 1 ? " is" : "s are"} still arriving.`);
        }
    } else {
        parts.push("No folder is being watched: start the server with -watch-dir and -allow-writes.");
    }
    const checking = q.items.filter((it) => it.checking).length;
    if (checking) parts.push(`Checking ${checking} for duplicates…`);
    folder.textContent = parts.join(" ");
    const problem = byId("imports-problem");
    problem.hidden = !q.problem;
    problem.textContent = q.problem ? `Watch folder: ${q.problem}` : "";
}

/** The first cover offered by a book's files: the EPUB's (its own cover, else its first page), else the PDF's first page. */
async function defaultCover(item: ImportSummary): Promise<boolean> {
    const book = pendingImport(item.id);
    const formats = new Set(item.files.map((f) => f.format));
    const sources: ((b: typeof book) => AsyncGenerator<CoverChoice>)[] = [];
    if (formats.has("epub")) sources.push(epubPages);
    if (formats.has("pdf")) sources.push(pdfPages);
    for (const pages of sources) {
        try {
            for await (const c of pages(book)) {
                if (!c.save) continue;
                await c.save();
                return true; // leaving the loop closes the book
            }
        } catch (err) {
            console.warn(`No cover from ${item.title}:`, err);
        }
    }
    return false;
}

/** Library pages are rewritten after this many books, and at the end. */
const REFRESH_EVERY = 25;
/**
 * Mass import: covers are found this many books ahead (fetched and drawn
 * here) while the server adds the book before them. The adds themselves go
 * one at a time, each checked against the library as it is after the last.
 */
const COVERS_AHEAD = 2;
const progress = byId("imports-progress");
const progressBar = byId("imports-progress-bar", HTMLProgressElement);
const progressText = byId("imports-progress-text");
const progressStop = byId("imports-progress-stop", HTMLButtonElement);
let running = false;
let stopping = false;
progressStop.addEventListener("click", () => {
    stopping = true;
    progressStop.disabled = true;
    progressStop.textContent = "Stopping…";
});
window.addEventListener("beforeunload", (e) => {
    if (running) e.preventDefault(); // leaving stops it part way
});

/** Shows the progress bar for a run over total books, one request each, with Stop. */
function startRun(total: number): void {
    running = true;
    stopping = false;
    discardExisting.disabled = true;
    if (acceptAll) acceptAll.disabled = true;
    progressStop.disabled = false;
    progressStop.textContent = "Stop";
    progressBar.max = total;
    progressBar.value = 0;
    progress.hidden = false;
    byId("imports-status").hidden = true;
}

function endRun(): void {
    running = false;
    progress.hidden = true;
    discardExisting.disabled = false;
    if (acceptAll) acceptAll.disabled = false;
}

// One book per request, as the mass import: a long queue never makes one
// request outlast the proxy's timeout, and the bar shows how far it got.
// Each is checked against metadata.db again before anything goes.
discardExisting.addEventListener("click", async () => {
    if (running) return;
    const items = (queue?.items ?? []).filter((it) => !it.into && identicalFiles(it).size > 0);
    if (!items.length || !confirm(`${discardExisting.title}\n\nDiscard them?`)) return;
    startRun(items.length);
    let files = 0;
    let books = 0;
    const errors: string[] = [];
    try {
        for (const [i, it] of items.entries()) {
            if (stopping) break;
            progressText.textContent = `${i + 1} of ${items.length} · ${files} file${files === 1 ? "" : "s"} discarded · “${it.title}”…`;
            try {
                // No title or authors: checked as its files say it is, as the list was.
                const res = await api<{ discarded: number }>("POST", `/import/${encodeURIComponent(it.id)}/discard-identical`, {});
                files += res.discarded;
                if (res.discarded) books++;
            } catch (err) {
                errors.push(`“${it.title}”: ${errorMessage(err)}`);
            }
            progressBar.value = i + 1;
        }
        const stopped = stopping ? "Stopped. " : "";
        say(`${stopped}Discarded ${files} file${files === 1 ? "" : "s"} already in the library, from ${books} book${books === 1 ? "" : "s"}.${errors.length ? ` Not done: ${errors.join("; ")}` : ""} An admin can undo it from Activity.`, errors.length > 0);
    } finally {
        endRun();
        await refresh();
    }
});

acceptAll?.addEventListener("click", async () => {
    const items = (queue?.items ?? []).filter(addable);
    if (!items.length || running) return;
    const waiting = queue?.items.length ?? 0;
    if (!confirm(`Add ${items.length} book${items.length === 1 ? "" : "s"} to the library with the metadata their files offer, without review?${waiting > items.length ? `\n\n${waiting - items.length} other${waiting - items.length === 1 ? "" : "s"} (maybe already in the library, new files for a library book, or not checked yet) stay here for review.` : ""}`)) return;
    startRun(items.length);
    notAdded.clear();
    let added = 0;
    let sinceRefresh = 0;
    // The library's pages, rewritten in the background while books go on being added.
    let refreshing: Promise<unknown> = Promise.resolve();
    const covers = new Map<string, Promise<boolean>>();
    const coverOf = (it: ImportSummary): Promise<boolean> => {
        let c = covers.get(it.id);
        if (!c) {
            // The browser renders what the server can't (a PDF page of text, an EPUB's first page).
            c = it.has_cover ? Promise.resolve(true) : defaultCover(it).catch(() => false);
            covers.set(it.id, c);
        }
        return c;
    };
    const plural = (n: number): string => `${n} book${n === 1 ? "" : "s"}`;
    const counts = (): string => `${plural(added)} added${notAdded.size ? `, ${notAdded.size} left for review` : ""}`;
    try {
        for (const [i, it] of items.entries()) {
            if (stopping) break;
            const step = `${i + 1} of ${items.length} · ${counts()} · `;
            for (const next of items.slice(i, i + 1 + COVERS_AHEAD)) void coverOf(next);
            if (!it.has_cover) {
                progressText.textContent = `${step}Finding a cover for “${it.title}”…`;
                await coverOf(it);
            }
            progressText.textContent = `${step}Adding “${it.title}”…`;
            try {
                const res = await api<{ uuid: string; reason: string }>("POST", `/import/${encodeURIComponent(it.id)}/accept`);
                if (res.uuid) {
                    added++;
                    if (++sinceRefresh >= REFRESH_EVERY) {
                        sinceRefresh = 0;
                        // Not waited for; one at a time.
                        refreshing = refreshing.then(() => api("POST", "/api/imports/accept-done")).catch(() => undefined);
                    }
                } else if (res.reason) {
                    notAdded.set(it.id, res.reason);
                }
            } catch (err) {
                notAdded.set(it.id, errorMessage(err)); // on to the next one
            }
            progressBar.value = i + 1;
        }
        progressText.textContent = `${counts()} · showing them in the library…`;
        await refreshing;
        if (added) await api("POST", "/api/imports/accept-done");
        const stopped = stopping ? "Stopped. " : "";
        say(`${stopped}Added ${plural(added)} to the library.${notAdded.size ? ` ${notAdded.size} left here for review: each says why.` : ""}`);
    } catch (err) {
        say(errorMessage(err), true);
    } finally {
        endRun();
        await refresh();
    }
});

// The last "couldn't load" message shown, cleared by the next load that works
// (the server was only restarting, say), unless something else was said since.
let loadError = "";
async function refresh(): Promise<void> {
    const status = byId("imports-status");
    try {
        render(await api<ImportQueue>("GET", "/api/imports"));
        if (loadError && status.textContent === loadError) status.hidden = true;
        loadError = "";
    } catch (err) {
        loadError = `Couldn't load the queue: ${errorMessage(err)}`;
        say(loadError, true);
    }
}

// Not while a run (mass import, discard) goes on: it refreshes once at the end.
let timer = 0;
async function poll(): Promise<void> {
    clearTimeout(timer);
    if (document.visibilityState === "visible" && !running) await refresh();
    timer = window.setTimeout(() => void poll(), busy(queue) ? BUSY_POLL_MS : POLL_MS);
}
void poll();
document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void poll();
});
