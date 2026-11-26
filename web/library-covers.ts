// The cover updater (/library/covers, Go: cover_refresh.go). The books come
// from the grid's selection (select-books.ts, through sessionStorage). Each
// book is looked up in turn, by ISBN and then by title and author, on
// Hardcover and then Open Library (/metadata/lookup). The cover found is
// shown next to the current one, and the ticked ones are saved
// (/api/covers/{uuid}/apply). Each saved cover is an entry in the edit log,
// so it can be undone on its own, here or later under Recent cover updates.
//
// Before looking anything up, the page compares what the run could cost
// with what's left of each source's requests today (/api/covers/budget),
// and asks first when the run would use most of it.

import { api, byId, el, errorMessage, statusLine } from "./dom.js";
import type { CoverBudget, CoverBudgets, CoverRefreshBook, CoverUpdate, LookupResult } from "./types.js";

/** sessionStorage: the books picked in the grid (select-books.ts). */
const COVER_SELECTION_KEY = "cf-cover-selection";
/** Warn when a run could use more than this share of what's left. */
const WARN_SHARE = 0.8;
/** Wait at most this long for a source's short-term limit, then move on. */
const MAX_WAIT_MS = 60_000;

type Provider = "hardcover" | "openlibrary";
const PROVIDER_NAMES: Record<Provider, string> = { hardcover: "Hardcover", openlibrary: "Open Library" };

interface Candidate {
    provider: Provider;
    /** /metadata/lookup/cover?src=…&id=… */
    url: string;
    src: string;
    id: string;
    byTitle: boolean;
    matchTitle: string;
    matchAuthors: string;
    blobUrl: string;
    width: number;
    height: number;
}

interface Row {
    book: CoverRefreshBook;
    li: HTMLLIElement;
    current: HTMLElement;
    found: HTMLElement;
    check: HTMLInputElement;
    status: HTMLElement;
    actions: HTMLElement;
    /** The current cover's image, null for none. */
    curImg: HTMLImageElement | null;
    candidate: Candidate | null;
    saved: boolean;
    /** Why the last lookup of the book failed, if one did. */
    lastError: string;
}

/** The session is gone (signed out, or no longer an editor): the run can't go on. */
class SessionLost extends Error {}

const say = statusLine("cu-status");
const sayHistory = statusLine("cu-history-status");
const max = Number(byId("cu-books").closest("main")?.dataset["max"] ?? "200");
const list = byId<HTMLUListElement>("cu-books");
const empty = byId("cu-empty");
const toolbar = byId("cu-toolbar");
const allBox = byId("cu-all", HTMLInputElement);
const scanBtn = byId("cu-scan", HTMLButtonElement);
const applyBtn = byId("cu-apply", HTMLButtonElement);
const warning = byId("cu-warning");
const progress = byId("cu-progress");
const progressBar = byId("cu-progress-bar", HTMLProgressElement);
const progressText = byId("cu-progress-text");
const stopBtn = byId("cu-stop", HTMLButtonElement);
const budgetEl = byId("cu-budget");
const historyList = byId<HTMLUListElement>("cu-history");
const historyEmpty = byId("cu-history-empty");

const rows: Row[] = [];
let budgets: CoverBudgets | null = null;
let busy = false;
let stopping = false;

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

// ---- Budgets ---------------------------------------------------------------------

function left(b: CoverBudget): number {
    return Math.max(0, b.limit - b.used);
}

/** A source we can ask: on, not paused, with requests left today. */
function usable(b: CoverBudget): boolean {
    return b.enabled && !b.paused && left(b) > 0;
}

function budgetLine(name: string, b: CoverBudget): HTMLElement {
    const row = el("div", { className: "cu-budget-row" }, el("span", { className: "cu-budget-name" }, name));
    if (!b.enabled) {
        row.append(el("span", { className: "auth-muted" }, "turned off on this server"));
        return row;
    }
    const meter = el("meter", { min: 0, max: Math.max(1, b.limit), value: b.used, low: b.limit * 0.5, high: b.limit * WARN_SHARE, optimum: 0 });
    meter.setAttribute("aria-label", `${name}: ${b.used} of ${b.limit} requests used today`);
    const parts = [`${b.used} of ${b.limit} used today`];
    if (b.resets_in) parts.push(`resets in ${b.resets_in}`);
    row.append(meter, el("span", {}, parts.join(" · ")));
    if (b.paused) row.append(el("span", { className: "auth-error-text" }, ` paused until ${b.paused}`));
    return row;
}

async function loadBudgets(): Promise<void> {
    try {
        budgets = await api<CoverBudgets>("GET", "/api/covers/budget");
    } catch (err) {
        budgetEl.replaceChildren(el("span", { className: "auth-error-text" }, `Couldn't read the lookup limits: ${errorMessage(err)}`));
        return;
    }
    budgetEl.replaceChildren(
        budgetLine(PROVIDER_NAMES.hardcover, budgets.hardcover),
        budgetLine(PROVIDER_NAMES.openlibrary, budgets.openlibrary),
        el("p", { className: "auth-muted cu-budget-note" }, "Hardcover's requests are shared with series and author lookups."),
    );
}

/** The most requests a book's lookup can take from each source, the way findCover asks. */
function bookCost(book: CoverRefreshBook, use: Record<Provider, boolean>): Record<Provider, number> {
    const c = budgets!.cost;
    const tries = (book.isbn ? 1 : 0) + (book.title ? 1 : 0);
    return {
        hardcover: use.hardcover ? tries * c.hardcover_lookup : 0,
        openlibrary: use.openlibrary ? tries * c.openlibrary_lookup + c.openlibrary_cover : 0,
    };
}

interface Estimate {
    cost: Record<Provider, number>;
    /** How many books, from the top, are sure to fit in what's left. */
    fit: number;
    warn: Provider[];
}

function estimate(books: CoverRefreshBook[], use: Record<Provider, boolean>): Estimate {
    const remaining: Record<Provider, number> = { hardcover: left(budgets!.hardcover), openlibrary: left(budgets!.openlibrary) };
    const cost: Record<Provider, number> = { hardcover: 0, openlibrary: 0 };
    let fit = 0;
    for (const b of books) {
        const c = bookCost(b, use);
        cost.hardcover += c.hardcover;
        cost.openlibrary += c.openlibrary;
        if (cost.hardcover <= remaining.hardcover && cost.openlibrary <= remaining.openlibrary) fit++;
    }
    const warn = (["hardcover", "openlibrary"] as Provider[]).filter((p) => use[p] && cost[p] > remaining[p] * WARN_SHARE);
    return { cost, fit, warn };
}

/** Asks before a run that could use most of a day's requests; resolves to how many books to look up (0: none). */
function confirmRun(books: CoverRefreshBook[], use: Record<Provider, boolean>): Promise<number> {
    const est = estimate(books, use);
    if (!est.warn.length) return Promise.resolve(books.length);
    const lines = est.warn.map((p) => {
        const b = budgets![p];
        return el("li", {}, `${PROVIDER_NAMES[p]}: up to ${est.cost[p]} requests, with ${left(b)} of ${b.limit} left today${b.resets_in ? ` (resets in ${b.resets_in})` : ""}.`);
    });
    return new Promise((resolve) => {
        const done = (n: number): void => {
            warning.hidden = true;
            warning.replaceChildren();
            resolve(n);
        };
        const anyway = el("button", { type: "button", className: "btn-standard" }, "Look up anyway");
        const some = el("button", { type: "button", className: "btn-standard btn-primary" }, `Look up the first ${est.fit}`);
        const cancel = el("button", { type: "button", className: "btn-standard" }, "Cancel");
        anyway.addEventListener("click", () => done(books.length));
        some.addEventListener("click", () => done(est.fit));
        cancel.addEventListener("click", () => done(0));
        const buttons = el("div", { className: "cu-warning-actions" }, anyway);
        if (est.fit > 0 && est.fit < books.length) buttons.append(some);
        buttons.append(cancel);
        warning.replaceChildren(
            el("p", {}, `Looking up ${books.length} ${books.length === 1 ? "book" : "books"} could use most of what's left of today's lookups:`),
            el("ul", {}, ...lines),
            el(
                "p",
                { className: "auth-muted" },
                "That's the most it could take: a book found early (by ISBN, or on Hardcover) costs less. " +
                    "If a source runs out partway, the rest are looked up on the other one.",
            ),
            buttons,
        );
        warning.hidden = false;
        (est.fit > 0 && est.fit < books.length ? some : anyway).focus();
    });
}

// ---- Looking up ------------------------------------------------------------------

/** The wait a source's "try again in 1m5s" asks for, in ms; null if it isn't one. */
function retryAfter(message: string): number | null {
    const m = /try again in ((?:\d+h)?(?:\d+m)?(?:\d+(?:\.\d+)?s)?)/.exec(message);
    if (!m?.[1]) return null;
    let ms = 0;
    for (const [, n, unit] of m[1].matchAll(/(\d+(?:\.\d+)?)([hms])/g)) {
        ms += Number(n) * (unit === "h" ? 3_600_000 : unit === "m" ? 60_000 : 1000);
    }
    return ms || null;
}

/** Daily limit used up, or told to slow down for a long while: don't ask this source again this run. */
function outOfBudget(message: string): boolean {
    return /used up|asked to slow down/i.test(message);
}

/** Sources not asked this run: off, out of requests, or dropped partway (in dropped too). */
const disabled = new Set<Provider>();
/** Sources skipped for a reason worth saying: out of requests, paused, or dropped partway. */
const dropped = new Set<Provider>();

function dropProvider(p: Provider, why: string): void {
    if (disabled.has(p)) return;
    disabled.add(p);
    dropped.add(p);
    say(`${PROVIDER_NAMES[p]} is skipped for the rest of this run: ${why}`, true);
}

/** Waits for a source's short-term limit (with the progress line saying so); false if stopped. */
async function waitFor(p: Provider, ms: number, row: Row): Promise<boolean> {
    const until = Date.now() + ms;
    while (Date.now() < until) {
        if (stopping) return false;
        row.status.textContent = `Waiting for ${PROVIDER_NAMES[p]}'s limit (${Math.ceil((until - Date.now()) / 1000)} s)…`;
        await sleep(Math.min(1000, until - Date.now()));
    }
    return true;
}

async function lookup(p: Provider, params: Record<string, string>, row: Row): Promise<LookupResult | null> {
    for (let attempt = 0; attempt < 4; attempt++) {
        if (stopping || disabled.has(p)) return null;
        let res: LookupResult;
        try {
            res = await api<LookupResult>("GET", `/metadata/lookup?${new URLSearchParams({ source: p, ...params })}`);
        } catch (err) {
            const status = (err as { status?: number }).status;
            if (status === 401 || status === 404) throw new SessionLost(errorMessage(err));
            if (status === 403) dropProvider(p, errorMessage(err));
            else row.lastError = `${PROVIDER_NAMES[p]}: ${errorMessage(err)}`;
            return null;
        }
        if (!res.error) return res;
        const wait = retryAfter(res.error);
        if (outOfBudget(res.error) || (wait !== null && wait > MAX_WAIT_MS)) {
            dropProvider(p, res.error);
            return null;
        }
        if (wait === null) {
            row.lastError = `${PROVIDER_NAMES[p]}: ${res.error}`;
            return null;
        }
        if (!(await waitFor(p, wait, row))) return null;
    }
    return null;
}

/** The candidate cover's image, fetched once (the server keeps a copy for saving it). */
async function fetchImage(p: Provider, url: string, row: Row): Promise<{ blobUrl: string; width: number; height: number } | null> {
    for (let attempt = 0; attempt < 4; attempt++) {
        if (stopping) return null;
        const resp = await fetch(url, { credentials: "same-origin" });
        if (resp.status === 401) throw new SessionLost(await resp.text());
        if (resp.ok) {
            const blobUrl = URL.createObjectURL(await resp.blob());
            const img = new Image();
            img.src = blobUrl;
            try {
                await img.decode();
            } catch {
                URL.revokeObjectURL(blobUrl);
                return null;
            }
            return { blobUrl, width: img.naturalWidth, height: img.naturalHeight };
        }
        const text = (await resp.text()).trim();
        const wait = retryAfter(text);
        if (wait === null || wait > MAX_WAIT_MS) {
            if (outOfBudget(text)) dropProvider(p, text);
            else row.lastError = `${PROVIDER_NAMES[p]}'s cover: ${text || resp.status}`;
            return null;
        }
        if (!(await waitFor(p, wait, row))) return null;
    }
    return null;
}

function firstAuthor(authors: string): string {
    return authors.split("&")[0]?.trim() ?? "";
}

/** Hardcover then Open Library by ISBN, then both by title and author: the first match with a cover. */
async function findCover(row: Row): Promise<Candidate | null> {
    const b = row.book;
    row.lastError = "";
    const steps: [Provider, Record<string, string>, boolean][] = [];
    if (b.isbn) {
        steps.push(["hardcover", { isbn: b.isbn }, false], ["openlibrary", { isbn: b.isbn }, false]);
    }
    if (b.title) {
        const q = { title: b.title, author: firstAuthor(b.authors) };
        steps.push(["hardcover", q, true], ["openlibrary", q, true]);
    }
    for (const [p, params, byTitle] of steps) {
        if (stopping) return null;
        if (disabled.has(p)) continue;
        row.status.textContent = `Looking on ${PROVIDER_NAMES[p]} by ${byTitle ? "title" : "ISBN"}…`;
        const res = await lookup(p, params, row);
        const match = res?.matches?.find((m) => m.cover);
        if (!match?.cover) continue;
        const u = new URL(match.cover, location.origin);
        const src = u.searchParams.get("src") ?? "";
        const id = u.searchParams.get("id") ?? "";
        if (u.origin !== location.origin || u.pathname !== "/metadata/lookup/cover" || !src || !id) continue;
        const url = `/metadata/lookup/cover?${new URLSearchParams({ src, id })}`;
        const img = await fetchImage(p, url, row);
        if (!img) continue;
        return {
            provider: p,
            url,
            src,
            id,
            byTitle,
            matchTitle: match.option.Title,
            matchAuthors: (match.option.Creators ?? []).join(" & "),
            ...img,
        };
    }
    return null;
}

// ---- Rows ------------------------------------------------------------------------

function coverBox(src: string | null, label: string, size?: string, lazy = false): HTMLElement {
    const box = el("figure", { className: "cu-cover" });
    if (src) {
        const img = el("img", { src, alt: label, loading: lazy ? "lazy" : "eager" });
        box.append(img);
    } else {
        box.append(el("div", { className: "cu-cover-none" }, "No cover"));
    }
    box.append(el("figcaption", {}, label, ...(size ? [el("br"), size] : [])));
    return box;
}

function dims(w: number, h: number): string {
    return w && h ? `${w}×${h}` : "";
}

function showCurrent(row: Row, src: string | null, label = "Current"): void {
    const box = coverBox(src, label);
    row.current.replaceChildren(box);
    const img = box.querySelector("img");
    row.curImg = img;
    if (!img) return;
    const caption = box.querySelector("figcaption")!;
    img.addEventListener("load", () => caption.replaceChildren(label, el("br"), dims(img.naturalWidth, img.naturalHeight)));
}

function makeRow(book: CoverRefreshBook): Row {
    const check = el("input", { type: "checkbox", disabled: true });
    check.setAttribute("aria-label", `Save the new cover for ${book.title}`);
    const title = el("a", { href: `/book/get/${encodeURIComponent(book.uuid)}`, className: "cu-title" }, book.title || book.uuid);
    const status = el("span", { className: "cu-row-status auth-muted" }, "Waiting");
    const actions = el("span", { className: "cu-row-actions" });
    const meta = el(
        "div",
        { className: "cu-meta" },
        title,
        el("span", { className: "auth-muted" }, book.authors),
        el("span", { className: "auth-muted cu-isbn" }, book.isbn ? `ISBN ${book.isbn}` : "No ISBN: found by title and author"),
        status,
        actions,
    );
    const current = el("div", { className: "cu-slot" });
    const found = el("div", { className: "cu-slot" }, coverBox(null, "New"));
    const arrow = el("span", { className: "cu-arrow", ariaHidden: "true" }, "→");
    const li = el("li", { className: "cu-book" }, el("label", { className: "cu-check" }, check), current, arrow, found, meta);
    const row: Row = { book, li, current, found, check, status, actions, curImg: null, candidate: null, saved: false, lastError: "" };
    showCurrent(row, book.cover ?? null);
    check.addEventListener("change", updateToolbar);
    return row;
}

function setCandidate(row: Row, c: Candidate | null): void {
    if (row.candidate) URL.revokeObjectURL(row.candidate.blobUrl);
    row.candidate = c;
    row.saved = false;
    row.actions.replaceChildren();
    if (!c) {
        row.found.replaceChildren(coverBox(null, "Nothing found"));
        row.check.checked = false;
        row.check.disabled = true;
        row.status.textContent = row.lastError ? `No cover found (${row.lastError})` : "No cover found";
        return;
    }
    const box = coverBox(c.blobUrl, `New, from ${PROVIDER_NAMES[c.provider]}`, dims(c.width, c.height));
    const link = el("a", { href: c.url, target: "_blank", rel: "noopener", className: "cu-full" }, "Full size");
    box.append(link);
    row.found.replaceChildren(box);
    const cur = row.curImg;
    const smaller = !!cur && cur.naturalWidth > 0 && c.width * c.height < cur.naturalWidth * cur.naturalHeight;
    row.check.disabled = false;
    // Found by ISBN and no smaller than the current one: ticked; anything
    // else is for the editor to look at first.
    row.check.checked = !c.byTitle && !smaller;
    const notes: string[] = [];
    if (c.byTitle) notes.push(`Matched by title: “${c.matchTitle}”${c.matchAuthors ? ` by ${c.matchAuthors}` : ""}. Check it's the same book.`);
    else notes.push(`Matched by ISBN on ${PROVIDER_NAMES[c.provider]}.`);
    if (smaller) notes.push("Smaller than the current cover.");
    row.status.textContent = notes.join(" ");
    row.li.classList.toggle("cu-weak", c.byTitle || smaller);
}

function ticked(): Row[] {
    return rows.filter((r) => r.candidate && !r.saved && r.check.checked && !r.check.disabled);
}

function updateToolbar(): void {
    const n = ticked().length;
    applyBtn.disabled = busy || n === 0;
    applyBtn.textContent = n ? `Save ${n} ${n === 1 ? "cover" : "covers"}` : "Save covers";
    scanBtn.disabled = busy || !rows.some((r) => !r.saved && !r.candidate);
    const choosable = rows.filter((r) => r.candidate && !r.saved);
    allBox.disabled = busy || choosable.length === 0;
    allBox.checked = choosable.length > 0 && choosable.every((r) => r.check.checked);
    allBox.indeterminate = !allBox.checked && choosable.some((r) => r.check.checked);
}

allBox.addEventListener("change", () => {
    for (const r of rows) if (r.candidate && !r.saved) r.check.checked = allBox.checked;
    updateToolbar();
});

// ---- Runs ------------------------------------------------------------------------

function onBeforeUnload(e: BeforeUnloadEvent): void {
    e.preventDefault();
}

function startRun(total: number): void {
    busy = true;
    stopping = false;
    progressBar.max = total;
    progressBar.value = 0;
    progress.hidden = false;
    stopBtn.disabled = false;
    window.addEventListener("beforeunload", onBeforeUnload);
    updateToolbar();
}

function endRun(): void {
    busy = false;
    progress.hidden = true;
    window.removeEventListener("beforeunload", onBeforeUnload);
    updateToolbar();
}

stopBtn.addEventListener("click", () => {
    stopping = true;
    stopBtn.disabled = true;
    progressText.textContent = "Stopping…";
});

async function scan(): Promise<void> {
    byId("cu-status").hidden = true;
    await loadBudgets();
    if (!budgets) return;
    disabled.clear();
    dropped.clear();
    const use: Record<Provider, boolean> = { hardcover: usable(budgets.hardcover), openlibrary: usable(budgets.openlibrary) };
    for (const p of ["hardcover", "openlibrary"] as Provider[]) {
        if (use[p]) continue;
        disabled.add(p);
        const b = budgets[p];
        if (b.enabled) dropped.add(p);
        if (b.enabled) say(`${PROVIDER_NAMES[p]} is skipped: ${b.paused ? `paused until ${b.paused}` : "no requests left today"}.`, true);
    }
    if (!use.hardcover && !use.openlibrary) {
        say("Neither Hardcover nor Open Library can be asked right now: see the limits above.", true);
        return;
    }
    // Books already looked up keep what they found: only the rest are asked about again.
    const todo = rows.filter((r) => !r.saved && !r.candidate);
    if (!todo.length) {
        say("Every book has a cover found or saved already.");
        return;
    }
    scanBtn.disabled = true;
    const n = await confirmRun(
        todo.map((r) => r.book),
        use,
    );
    scanBtn.disabled = false;
    if (!n) {
        for (const r of todo) if (!r.candidate) r.status.textContent = "Not looked up";
        return;
    }
    const run = todo.slice(0, n);
    for (const r of todo.slice(n)) if (!r.candidate) r.status.textContent = "Not looked up";
    startRun(run.length);
    let found = 0;
    let looked = 0;
    try {
        for (const [i, row] of run.entries()) {
            if (stopping) {
                for (const r of run.slice(i)) if (!r.candidate) r.status.textContent = "Not looked up";
                break;
            }
            progressText.textContent = `${i + 1} of ${run.length} · ${row.book.title}`;
            row.li.scrollIntoView({ block: "nearest" });
            let c: Candidate | null;
            try {
                c = await findCover(row);
            } catch (err) {
                if (!(err instanceof SessionLost)) throw err;
                for (const r of run.slice(i)) if (!r.candidate) r.status.textContent = "Not looked up";
                say(`Stopped: ${err.message || "you're no longer signed in"}. Sign in again, then reload this page.`, true);
                return;
            }
            await row.curImg?.decode().catch(() => undefined); // its size, to compare
            if (stopping && !c) {
                for (const r of run.slice(i)) if (!r.candidate) r.status.textContent = "Not looked up";
                break;
            }
            looked++;
            setCandidate(row, c);
            if (c) found++;
            progressBar.value = i + 1;
            updateToolbar();
            void loadBudgets();
            if (disabled.has("hardcover") && disabled.has("openlibrary")) {
                for (const r of run.slice(i + 1)) r.status.textContent = "Not looked up";
                break;
            }
        }
    } finally {
        endRun();
        await loadBudgets();
    }
    const msg = `Found ${found} ${found === 1 ? "cover" : "covers"} for ${looked} of ${run.length} ${run.length === 1 ? "book" : "books"}${stopping ? " before stopping" : ""}. Untick any that look wrong, then save.`;
    if (!dropped.size) say(msg);
    else say(`${msg} Skipped: ${[...dropped].map((p) => PROVIDER_NAMES[p]).join(", ")}.`, true);
}

scanBtn.addEventListener("click", () => void scan());

async function apply(): Promise<void> {
    const todo = ticked();
    if (!todo.length) return;
    startRun(todo.length);
    let saved = 0;
    const failed: string[] = [];
    try {
        for (const [i, row] of todo.entries()) {
            if (stopping) break;
            const c = row.candidate!;
            progressText.textContent = `Saving ${i + 1} of ${todo.length} · ${row.book.title}`;
            try {
                const res = await api<{ edit_id: number; cover: string }>("POST", `/api/covers/${encodeURIComponent(row.book.uuid)}/apply`, { src: c.src, id: c.id });
                markSaved(row, res.edit_id, res.cover);
                saved++;
            } catch (err) {
                row.status.textContent = `Not saved: ${errorMessage(err)}`;
                failed.push(row.book.title);
            }
            progressBar.value = i + 1;
        }
    } finally {
        endRun();
        void loadBudgets();
        void loadHistory();
    }
    if (failed.length) say(`Saved ${saved}. Couldn't save ${failed.length}: ${failed.join("; ")}`, true);
    else say(`Saved ${saved} ${saved === 1 ? "cover" : "covers"}. Each can be undone below.`);
}

applyBtn.addEventListener("click", () => void apply());

function markSaved(row: Row, editId: number, cover: string): void {
    row.saved = true;
    row.check.checked = false;
    row.check.disabled = true;
    row.li.classList.add("cu-saved");
    row.li.classList.remove("cu-weak");
    showCurrent(row, cover, "Saved");
    row.status.textContent = "Saved.";
    if (!editId) {
        row.status.textContent = "Saved (it was the same image as before).";
        row.actions.replaceChildren();
        return;
    }
    const undo = el("button", { type: "button", className: "btn-standard" }, "Undo");
    undo.addEventListener("click", async () => {
        undo.disabled = true;
        const res = await undoUpdate(editId);
        if (res === null) {
            undo.disabled = false;
            row.status.textContent = "Couldn't undo: see the message above.";
            return;
        }
        markRestored(row, res);
    });
    row.actions.replaceChildren(undo);
    updateToolbar();
}

/** After an undo: the row shows the cover put back, and offers the one found again. */
function markRestored(row: Row, cover: string): void {
    showCurrent(row, cover || null, "Restored");
    row.saved = false;
    row.actions.replaceChildren();
    row.li.classList.remove("cu-saved");
    row.li.classList.toggle("cu-weak", !!row.candidate?.byTitle);
    row.check.disabled = !row.candidate;
    row.status.textContent = "Previous cover restored.";
    updateToolbar();
}

/** Undoes a saved cover; the book's cover now ("" for none), or null when refused. */
async function undoUpdate(id: number): Promise<string | null> {
    try {
        const res = await api<{ cover: string }>("POST", `/api/covers/history/${encodeURIComponent(String(id))}/revert`, {});
        void loadHistory();
        return res.cover;
    } catch (err) {
        sayHistory(`Couldn't undo: ${errorMessage(err)}`, true);
        say(`Couldn't undo: ${errorMessage(err)}`, true);
        return null;
    }
}

// ---- History ---------------------------------------------------------------------

function historyRow(u: CoverUpdate): HTMLLIElement {
    const base = `/api/covers/history/${encodeURIComponent(String(u.id))}`;
    const before = coverBox(u.had_cover ? `${base}/before` : null, "Before", undefined, true);
    const after = coverBox(`${base}/after`, "After", undefined, true);
    const when = new Date(u.at * 1000).toLocaleString();
    const title = u.in_library
        ? el("a", { href: `/book/get/${encodeURIComponent(u.uuid)}`, className: "cu-title" }, u.title || u.uuid)
        : el("span", { className: "cu-title" }, `${u.title || u.uuid} (no longer in the library)`);
    const info = el("div", { className: "cu-meta" }, title, el("span", { className: "auth-muted" }, `${when} · ${u.username}`));
    if (u.reverted_at) {
        info.append(el("span", { className: "auth-muted" }, `Undone ${new Date(u.reverted_at * 1000).toLocaleString()}${u.reverted_by ? ` by ${u.reverted_by}` : ""}`));
    } else if (u.in_library) {
        const undo = el("button", { type: "button", className: "btn-standard" }, "Undo");
        undo.addEventListener("click", async () => {
            undo.disabled = true;
            const res = await undoUpdate(u.id);
            if (res === null) {
                undo.disabled = false;
                return;
            }
            sayHistory(`Restored the previous cover of “${u.title}”.`);
            const row = rows.find((r) => r.book.uuid === u.uuid);
            if (row?.saved) markRestored(row, res);
        });
        info.append(el("span", { className: "cu-row-actions" }, undo));
    }
    const li = el("li", { className: "cu-book" }, before, el("span", { className: "cu-arrow", ariaHidden: "true" }, "→"), after, info);
    if (u.reverted_at) li.classList.add("cu-undone");
    return li;
}

async function loadHistory(): Promise<void> {
    try {
        const res = await api<{ updates: CoverUpdate[] }>("GET", "/api/covers/history");
        historyList.replaceChildren(...res.updates.map(historyRow));
        historyEmpty.hidden = res.updates.length > 0;
    } catch (err) {
        sayHistory(`Couldn't load the recent updates: ${errorMessage(err)}`, true);
    }
}

// ---- Start -----------------------------------------------------------------------

function pickedUUIDs(): string[] {
    try {
        const raw = sessionStorage.getItem(COVER_SELECTION_KEY);
        const ids: unknown = raw ? JSON.parse(raw) : [];
        return Array.isArray(ids) ? ids.filter((x): x is string => typeof x === "string") : [];
    } catch {
        return [];
    }
}

async function start(): Promise<void> {
    void loadHistory();
    const uuids = pickedUUIDs();
    if (!uuids.length) {
        empty.hidden = false;
        await loadBudgets();
        return;
    }
    if (uuids.length > max) {
        say(`At most ${max} books at a time: only the first ${max} are shown.`, true);
        uuids.length = max;
    }
    let books: CoverRefreshBook[];
    try {
        books = (await api<{ books: CoverRefreshBook[] }>("POST", "/api/covers/books", { uuids })).books;
    } catch (err) {
        say(errorMessage(err), true);
        return;
    }
    if (!books.length) {
        empty.hidden = false;
        await loadBudgets();
        return;
    }
    for (const b of books) {
        const row = makeRow(b);
        rows.push(row);
        list.append(row.li);
    }
    toolbar.hidden = false;
    updateToolbar();
    await scan();
}

void start();
