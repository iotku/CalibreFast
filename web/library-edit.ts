// The bulk metadata editor (/library/edit; Go: bulk_edit.go). Every book is
// a row; the table is filtered, sorted and searched here, in the browser,
// so finding duplicates and faulty tags or languages is instant. Changes go
// to POST /api/library/bulk, which answers the changed rows.
//
// Only the rows on screen are in the DOM: every row is ROW_H tall, so row
// i sits at i × ROW_H in a body as tall as all of them.
//
// The filters, sort and duplicate mode are kept in the URL (replaceState),
// so coming back (with Back) finds the same view.

import { api, byId, closeOnBackdropClick, el, errorMessage, modalOpen } from "./dom.js";
import { attachSuggest } from "./suggest.js";
import { deleteBooks } from "./delete-books.js";
import { languageChipLabel, languageKey, loadLanguageCodes } from "./languages.js";
import type { BulkEdit, BulkResult, NotDuplicateSet, TableBook } from "./types.js";

const ROW_H = 34; // px; .lt-row's height in library-edit.css
const OVERSCAN = 8; // rows drawn beyond the screen, each way
/** sessionStorage: books the grid's "Edit in table" picked (select-books.ts). */
const BULK_SELECTION_KEY = "cf-bulk-selection";
/** At most this many books are merged at once (Go: mergeMaxBooks). */
const MERGE_MAX = 20;

type ColKey = "title" | "authors" | "series" | "publisher" | "pubdate" | "languages" | "tags" | "formats" | "added";
/** The bulk edit field a column's cells edit. */
type EditField = "title" | "authors" | "publisher" | "date" | "tags" | "languages";

interface Column {
    key: ColKey;
    label: string;
    /** The cell holds several values (chips). */
    list?: boolean;
    edit?: EditField;
    /** Its text, for searching and sorting. */
    text(b: TableBook): string;
    items?(b: TableBook): string[];
}

const COLUMNS: Column[] = [
    { key: "title", label: "Title", edit: "title", text: (b) => b.title },
    { key: "authors", label: "Authors", edit: "authors", text: (b) => b.authors },
    { key: "series", label: "Series", text: (b) => (b.series ? `${b.series} [${b.series_index}]` : "") },
    { key: "publisher", label: "Publisher", edit: "publisher", text: (b) => b.publisher },
    { key: "pubdate", label: "Published", edit: "date", text: (b) => b.pubdate },
    { key: "languages", label: "Languages", list: true, edit: "languages", text: (b) => b.languages.join(", "), items: (b) => b.languages },
    { key: "tags", label: "Tags", list: true, edit: "tags", text: (b) => b.tags.join(", "), items: (b) => b.tags },
    { key: "formats", label: "Formats", list: true, text: (b) => b.formats.join(", "), items: (b) => b.formats },
    { key: "added", label: "Added", text: (b) => b.added },
];

/** Lower case, without accents: what searches compare. */
function fold(s: string): string {
    return s.normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();
}

interface Row {
    b: TableBook;
    /** Each column's text, folded. */
    hay: Record<ColKey, string>;
    /** List columns' values, folded. */
    items: Partial<Record<ColKey, string[]>>;
    all: string;
    dupKey: string;
    group: number; // duplicate group, for shading
}

function makeRow(b: TableBook): Row {
    const hay = {} as Record<ColKey, string>;
    const items: Partial<Record<ColKey, string[]>> = {};
    for (const c of COLUMNS) {
        hay[c.key] = fold(c.text(b));
        if (c.items) items[c.key] = c.items(b).map(fold);
    }
    return { b, hay, items, all: Object.values(hay).join("\n") + "\n" + b.isbn, dupKey: "", group: 0 };
}

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

type Test = (value: string, items: string[] | undefined) => boolean;

/** A column filter (see the page's Filter tips); null for an empty box. */
function compileFilter(raw: string): Test | null {
    let s = raw.trim();
    if (!s) return null;
    let negate = false;
    if (s.startsWith("!")) {
        negate = true;
        s = s.slice(1).trim();
    }
    const alternatives: Test[] = s.split("|").map((part): Test => {
        const p = part.trim();
        if (p.startsWith("=")) {
            const want = fold(p.slice(1).trim());
            return (v, items) => (items ? (want === "" ? items.length === 0 : items.includes(want)) : v === want);
        }
        const cmp = /^([<>]=?)\s*(.+)$/.exec(p);
        if (cmp) {
            const [, op, w] = cmp as unknown as [string, string, string];
            const want = fold(w);
            return (v) => v !== "" && (op === "<" ? v < want : op === "<=" ? v <= want : op === ">" ? v > want : v >= want);
        }
        const want = fold(p);
        return (v, items) => (items ? items.some((i) => i.includes(want)) : v.includes(want));
    });
    const test: Test = (v, items) => alternatives.some((t) => t(v, items));
    return negate ? (v, items) => !test(v, items) : test;
}

/** Duplicate keys: books with the same key look like one book. */
function titleKey(title: string): string {
    return fold(title)
        .replace(/[([{][^)\]}]*[)\]}]/g, " ") // "(epub)", "[Retail]"
        .replace(/[^\p{L}\p{N}]+/gu, " ")
        .trim()
        .replace(/^(the|a|an) /, "");
}
function authorsKey(authors: string): string {
    return authors
        .split("&")
        .map((a) => fold(a).replace(/[^\p{L}\p{N}]+/gu, ""))
        .filter(Boolean)
        .sort()
        .join("&");
}
function dupKeyOf(b: TableBook, mode: string): string {
    switch (mode) {
        case "title":
            return titleKey(b.title);
        case "title-authors":
            return `${titleKey(b.title)}\n${authorsKey(b.authors)}`;
        case "isbn":
            return b.isbn.replace(/[^0-9xX]/g, "").toUpperCase();
    }
    return "";
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

let rows: Row[] = [];
let shown: Row[] = [];
const selected = new Set<string>();
let lastClicked: string | null = null; // for shift-click ranges
const filters: Partial<Record<ColKey, string>> = {};
let search = "";
let dupMode = ""; // "", a dupKeyOf mode, or MARKED
/** dupMode for the groups marked as not duplicates (Go: not_duplicates.go). */
const MARKED = "marked";
let notDupSets: NotDuplicateSet[] = [];

/** The table's answer: the rows, and the sets marked as not duplicates. */
interface TableData {
    books: TableBook[];
    not_duplicates: NotDuplicateSet[];
}
let onlySelected = false;
let sortKey: ColKey = "title";
let sortDesc = false;

const scroller = byId("lt-scroll");
const head = byId("lt-head");
const body = byId("lt-rows");
const table = byId("lt-table");
const countEl = byId("lt-count");
const statusEl = byId("lt-status");
const emptyEl = byId("lt-empty");
const searchInput = byId("lt-search", HTMLInputElement);
const dupSelect = byId("lt-dupes", HTMLSelectElement);
const onlySelectedBox = byId("lt-only-selected", HTMLInputElement);
const bar = byId("lt-bar");
const selectedEl = byId("lt-selected");
const mergeBtn = byId("lt-merge", HTMLButtonElement);

function say(text: string, kind: "ok" | "error" | "" = ""): void {
    statusEl.hidden = !text;
    statusEl.textContent = statusEl.title = text;
    statusEl.className = `lt-status${kind ? ` ${kind}` : ""}`;
}

const n = (k: number): string => k.toLocaleString();
const plural = (k: number, one: string, many = `${one}s`): string => `${n(k)} ${k === 1 ? one : many}`;

// ---------------------------------------------------------------------------
// Header: titles that sort, and a filter box per column
// ---------------------------------------------------------------------------

const allBox = el("input", { type: "checkbox", title: "Select every book shown", ariaLabel: "Select every book shown" });
const sortButtons = new Map<ColKey, HTMLButtonElement>();
const filterInputs = new Map<ColKey, HTMLInputElement>();
const datalists = new Map<ColKey, HTMLDataListElement>();

function buildHead(): void {
    const titles = el("div", { className: "lt-row lt-head-row", role: "row" }, el("span", { className: "lt-cell lt-check" }, allBox));
    const boxes = el("div", { className: "lt-row lt-filter-row", role: "row" }, el("span", { className: "lt-cell lt-check" }));
    for (const c of COLUMNS) {
        const btn = el("button", { type: "button", className: "lt-sort", title: `Sort by ${c.label.toLowerCase()}` }, c.label);
        btn.addEventListener("click", () => {
            if (sortKey === c.key) sortDesc = !sortDesc;
            else [sortKey, sortDesc] = [c.key, false];
            refresh();
        });
        sortButtons.set(c.key, btn);
        titles.append(el("span", { className: `lt-cell lt-col-${c.key}`, role: "columnheader" }, btn));

        const input = el("input", {
            type: "search",
            className: "lt-filter",
            placeholder: c.list ? "contains, =exact" : "filter",
            ariaLabel: `Filter ${c.label.toLowerCase()}`,
            autocomplete: "off",
            spellcheck: false,
            value: filters[c.key] ?? "",
        });
        if (c.list || c.key === "publisher" || c.key === "series") {
            const list = el("datalist", { id: `lt-values-${c.key}` });
            datalists.set(c.key, list);
            input.setAttribute("list", list.id);
            boxes.append(list);
        }
        input.addEventListener("input", () => {
            filters[c.key] = input.value;
            scheduleRefresh();
        });
        filterInputs.set(c.key, input);
        boxes.append(el("span", { className: `lt-cell lt-col-${c.key}` }, input));
    }
    titles.append(el("span", { className: "lt-cell lt-actions", role: "columnheader" }));
    boxes.append(el("span", { className: "lt-cell lt-actions" }));
    head.replaceChildren(titles, boxes);

    allBox.addEventListener("change", () => {
        const on = allBox.checked;
        for (const r of shown) {
            if (on) selected.add(r.b.uuid);
            else selected.delete(r.b.uuid);
        }
        selectionChanged();
    });
}

/** The values a list column's box suggests: "=value", with how many books have it. */
function fillDatalists(): void {
    for (const [key, list] of datalists) {
        const col = COLUMNS.find((c) => c.key === key)!;
        const counts = new Map<string, number>();
        for (const r of rows) {
            const values = col.items ? col.items(r.b) : key === "series" ? [r.b.series] : [col.text(r.b)];
            for (const v of values) if (v) counts.set(v, (counts.get(v) ?? 0) + 1);
        }
        const sorted = [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, 2000);
        list.replaceChildren(
            ...sorted.map(([v, k]) => el("option", { value: `=${v}`, label: `${key === "languages" ? languageChipLabel(v) : v} · ${plural(k, "book")}` })),
        );
    }
}

// ---------------------------------------------------------------------------
// Filtering, sorting, drawing
// ---------------------------------------------------------------------------

function compare(a: Row, b: Row): number {
    if (sortKey === "series") {
        const s = a.hay.series.localeCompare(b.hay.series) || a.b.series_index - b.b.series_index;
        if (s) return s;
    }
    const x = a.hay[sortKey];
    const y = b.hay[sortKey];
    // Empty values last, whichever way.
    if (!x !== !y) return !x ? 1 : -1;
    const c = x.localeCompare(y, undefined, { numeric: true });
    return (sortDesc ? -c : c) || a.hay.title.localeCompare(b.hay.title) || a.b.id - b.b.id;
}

function applyFilters(): void {
    const tests = COLUMNS.map((c) => [c, compileFilter(filters[c.key] ?? "")] as const).filter(([, t]) => t !== null) as [Column, Test][];
    const words = fold(search).split(/\s+/).filter(Boolean);

    let groups: Set<string> | null = null;
    if (dupMode === MARKED) {
        // Each marked set with at least two books still in the library.
        const present = new Set(rows.map((r) => r.b.uuid));
        const keyOf = new Map<string, string>();
        for (const s of notDupSets) {
            const here = s.uuids.filter((u) => present.has(u));
            if (here.length < 2) continue;
            for (const u of here) if (!keyOf.has(u)) keyOf.set(u, `set:${s.id}`);
        }
        for (const r of rows) r.dupKey = keyOf.get(r.b.uuid) ?? "";
        groups = new Set(keyOf.values());
    } else if (dupMode) {
        // Over the whole library: a book's duplicate may be filtered out.
        const members = new Map<string, string[]>();
        for (const r of rows) {
            r.dupKey = dupKeyOf(r.b, dupMode);
            if (r.dupKey) members.set(r.dupKey, [...(members.get(r.dupKey) ?? []), r.b.uuid]);
        }
        const sets = notDupSets.map((s) => new Set(s.uuids));
        // A group whose books were all marked as not duplicates of each other isn't one.
        const marked = (uuids: string[]): boolean => sets.some((s) => uuids.every((u) => s.has(u)));
        groups = new Set([...members].filter(([, us]) => us.length > 1 && !marked(us)).map(([key]) => key));
    }

    shown = rows.filter((r) => {
        if (onlySelected && !selected.has(r.b.uuid)) return false;
        if (groups && !groups.has(r.dupKey)) return false;
        for (const w of words) if (!r.all.includes(w)) return false;
        for (const [c, t] of tests) if (!t(r.hay[c.key], r.items[c.key])) return false;
        return true;
    });
    shown.sort(compare);
    if (groups) {
        // Each group together, in the order its first book sorts.
        const first = new Map<string, number>();
        shown.forEach((r, i) => {
            if (!first.has(r.dupKey)) first.set(r.dupKey, i);
        });
        shown.sort((a, b) => first.get(a.dupKey)! - first.get(b.dupKey)! || compare(a, b));
        let g = 0;
        shown.forEach((r, i) => {
            if (i > 0 && r.dupKey !== shown[i - 1]!.dupKey) g++;
            r.group = g;
        });
    }
}

let refreshTimer = 0;
function scheduleRefresh(): void {
    clearTimeout(refreshTimer);
    refreshTimer = window.setTimeout(refresh, 120);
}

function refresh(): void {
    clearTimeout(refreshTimer);
    applyFilters();
    for (const [key, btn] of sortButtons) {
        btn.dataset["sort"] = key === sortKey ? (sortDesc ? "desc" : "asc") : "";
        btn.closest(".lt-cell")?.setAttribute("aria-sort", key === sortKey ? (sortDesc ? "descending" : "ascending") : "none");
    }
    const groupsShown = dupMode && shown.length ? shown[shown.length - 1]!.group + 1 : 0;
    countEl.textContent =
        `${n(shown.length)} of ${plural(rows.length, "book")}` +
        (dupMode === MARKED ? ` · ${plural(groupsShown, "group")} marked not duplicates` : dupMode ? ` · ${plural(groupsShown, "group")} of look-alikes` : "");
    table.setAttribute("aria-rowcount", String(shown.length));
    body.style.height = `${shown.length * ROW_H}px`;
    emptyEl.hidden = shown.length > 0 || rows.length === 0;
    saveView();
    draw(true);
    selectionChanged();
}

let drawn = { from: -1, to: -1 };
function draw(force = false): void {
    const top = Math.max(0, scroller.scrollTop - body.offsetTop);
    const from = Math.max(0, Math.floor(top / ROW_H) - OVERSCAN);
    const to = Math.min(shown.length, Math.ceil((top + scroller.clientHeight) / ROW_H) + OVERSCAN);
    if (!force && from === drawn.from && to === drawn.to) return;
    drawn = { from, to };
    const out: HTMLElement[] = [];
    for (let i = from; i < to; i++) out.push(rowElement(shown[i]!, i));
    body.replaceChildren(...out);
}

function chip(col: ColKey, value: string): HTMLElement {
    return el(
        "button",
        { type: "button", className: "lt-chip", title: `Show only books with ${value}` },
        col === "languages" ? languageChipLabel(value) : value,
    );
}

function rowElement(r: Row, i: number): HTMLElement {
    const b = r.b;
    const on = selected.has(b.uuid);
    const row = el("div", { className: "lt-row", role: "row" });
    row.dataset["uuid"] = b.uuid;
    row.style.top = `${i * ROW_H}px`;
    row.setAttribute("aria-rowindex", String(i + 1));
    row.setAttribute("aria-selected", String(on));
    if (dupMode) row.classList.add(r.group % 2 ? "lt-group-odd" : "lt-group-even");
    if (dupMode && i > 0 && shown[i - 1]!.dupKey !== r.dupKey) row.classList.add("lt-group-start");

    const box = el("input", { type: "checkbox", checked: on, ariaLabel: `Select “${b.title}”` });
    row.append(el("span", { className: "lt-cell lt-check" }, box));
    for (const c of COLUMNS) {
        const cell = el("span", { className: `lt-cell lt-col-${c.key}`, role: "gridcell" });
        cell.dataset["col"] = c.key;
        if (c.items) {
            const values = c.items(b);
            for (const v of values) {
                const ch = chip(c.key, v);
                ch.dataset["value"] = v;
                cell.append(ch);
            }
            cell.title = values.join(", ");
        } else {
            const text = c.text(b);
            cell.textContent = text;
            cell.title = text;
        }
        if (c.edit) cell.classList.add("lt-editable");
        row.append(cell);
    }
    const actions = el("span", { className: "lt-cell lt-actions" });
    actions.append(
        el("a", { href: `/book/edit/${encodeURIComponent(b.uuid)}`, target: "_blank", className: "lt-action", title: "Open in the metadata editor (new tab)", ariaLabel: `Edit “${b.title}”` }, "Edit"),
    );
    if (dupMode === MARKED && (i === 0 || shown[i - 1]!.dupKey !== r.dupKey)) {
        const un = el("button", { type: "button", className: "lt-action lt-unmark", title: "They may be duplicates after all: show them in the duplicates view again" }, "Unmark");
        un.dataset["set"] = r.dupKey.replace(/^set:/, "");
        actions.append(un);
    } else if (dupMode && (i === 0 || shown[i - 1]!.dupKey !== r.dupKey)) {
        const group = groupOf(r);
        const size = group.length;
        const notDup = el("button", { type: "button", className: "lt-action lt-notdup", title: "These books only look alike: leave them out of the duplicates view (Duplicates → Marked not duplicates undoes it)" }, "Not duplicates");
        actions.append(notDup);
        if (size > 1 && size <= MERGE_MAX) {
            // A link, so a middle click (or Ctrl/⌘-click) opens the merge in a new tab.
            actions.append(
                el("a", { href: mergeLink(group.map((x) => x.b.uuid)), className: "lt-action lt-merge-group", title: `Merge these ${size} books into one` }, `Merge ${size}`),
            );
        }
    }
    row.append(actions);
    return row;
}

/** The shown rows in r's duplicate group. */
function groupOf(r: Row): Row[] {
    return shown.filter((x) => x.dupKey === r.dupKey);
}

scroller.addEventListener("scroll", () => requestAnimationFrame(() => draw()), { passive: true });
new ResizeObserver(() => draw()).observe(scroller);

// ---------------------------------------------------------------------------
// Selection
// ---------------------------------------------------------------------------

function selectionChanged(): void {
    const k = selected.size;
    bar.hidden = k === 0;
    selectedEl.textContent = `${plural(k, "book")} selected`;
    mergeBtn.disabled = k < 2 || k > MERGE_MAX;
    mergeBtn.title = k > MERGE_MAX ? `At most ${MERGE_MAX} books at a time` : "Make the selected books one book, with every format";
    const shownSelected = shown.reduce((t, r) => t + (selected.has(r.b.uuid) ? 1 : 0), 0);
    allBox.checked = shown.length > 0 && shownSelected === shown.length;
    allBox.indeterminate = shownSelected > 0 && shownSelected < shown.length;
    for (const row of body.querySelectorAll<HTMLElement>(".lt-row")) {
        const on = selected.has(row.dataset["uuid"] ?? "");
        row.setAttribute("aria-selected", String(on));
        const box = row.querySelector<HTMLInputElement>(".lt-check input");
        if (box) box.checked = on;
    }
    document.body.classList.toggle("lt-has-selection", k > 0);
}

function toggle(uuid: string, on: boolean, range: boolean): void {
    if (range && lastClicked) {
        const a = shown.findIndex((r) => r.b.uuid === lastClicked);
        const b = shown.findIndex((r) => r.b.uuid === uuid);
        if (a >= 0 && b >= 0) {
            for (let i = Math.min(a, b); i <= Math.max(a, b); i++) {
                const u = shown[i]!.b.uuid;
                if (on) selected.add(u);
                else selected.delete(u);
            }
            lastClicked = uuid;
            selectionChanged();
            return;
        }
    }
    if (on) selected.add(uuid);
    else selected.delete(uuid);
    lastClicked = uuid;
    selectionChanged();
}

function selectedBooks(): TableBook[] {
    return rows.filter((r) => selected.has(r.b.uuid)).map((r) => r.b);
}

function mergeLink(uuids: string[]): string {
    return `/book/merge?${new URLSearchParams({ ids: uuids.join(",") })}`;
}

body.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    const row = target.closest<HTMLElement>(".lt-row");
    const uuid = row?.dataset["uuid"];
    if (!row || !uuid) return;
    if (target instanceof HTMLInputElement && target.type === "checkbox") {
        toggle(uuid, target.checked, e.shiftKey);
        return;
    }
    if (target.closest(".lt-notdup")) {
        const r = rows.find((x) => x.b.uuid === uuid);
        // The whole group, filtered out or not: that's what the view compares.
        if (r) void markNotDuplicates(rows.filter((x) => x.dupKey === r.dupKey).map((x) => x.b.uuid));
        return;
    }
    const un = target.closest<HTMLElement>(".lt-unmark");
    if (un) {
        void unmarkNotDuplicates(Number(un.dataset["set"]));
        return;
    }
    const ch = target.closest<HTMLElement>(".lt-chip");
    if (ch) {
        const col = ch.closest<HTMLElement>(".lt-cell")?.dataset["col"] as ColKey | undefined;
        const input = col && filterInputs.get(col);
        if (col && input) {
            input.value = filters[col] = `=${ch.dataset["value"] ?? ""}`;
            refresh();
        }
        return;
    }
});

async function markNotDuplicates(uuids: string[]): Promise<void> {
    try {
        const set = await api<NotDuplicateSet>("POST", "/api/library/not-duplicates", { uuids });
        notDupSets.push(set);
        say(`Marked ${plural(uuids.length, "book")} as not duplicates. Duplicates → Marked not duplicates lists them, to unmark.`, "ok");
        refresh();
    } catch (err) {
        say(`Couldn't mark them: ${errorMessage(err)}`, "error");
    }
}

async function unmarkNotDuplicates(id: number): Promise<void> {
    try {
        await api("DELETE", `/api/library/not-duplicates/${id}`);
        notDupSets = notDupSets.filter((s) => s.id !== id);
        say("Unmarked: they show in the duplicates view again.", "ok");
        refresh();
    } catch (err) {
        say(`Couldn't unmark them: ${errorMessage(err)}`, "error");
    }
}

byId("lt-select-shown").addEventListener("click", () => {
    for (const r of shown) selected.add(r.b.uuid);
    selectionChanged();
});
byId("lt-select-none").addEventListener("click", () => {
    selected.clear();
    if (onlySelected) refresh();
    else selectionChanged();
});
mergeBtn.addEventListener("click", () => {
    // In the table's order: the first is the record kept, unless changed there.
    const order = shown.filter((r) => selected.has(r.b.uuid)).map((r) => r.b.uuid);
    const rest = [...selected].filter((u) => !order.includes(u));
    location.assign(mergeLink([...order, ...rest]));
});
byId("lt-remove").addEventListener("click", async () => {
    const books = selectedBooks();
    if (!(await deleteBooks(books))) return;
    const gone = new Set(books.map((b) => b.uuid));
    rows = rows.filter((r) => !gone.has(r.b.uuid));
    selected.clear();
    say(`Removed ${plural(gone.size, "book")}. They're in the Recycling Bin.`, "ok");
    refresh();
});

// ---------------------------------------------------------------------------
// Edits: in a cell, or to every selected book (the dialog)
// ---------------------------------------------------------------------------

/** Send an edit; the changed rows come back. */
async function sendEdit(uuids: string[], edit: BulkEdit): Promise<BulkResult> {
    const res = await api<BulkResult>("POST", "/api/library/bulk", { uuids, edit });
    const fresh = new Map(res.books.map((b) => [b.uuid, b]));
    rows = rows.map((r) => {
        const b = fresh.get(r.b.uuid);
        return b ? makeRow(b) : r;
    });
    return res;
}

function resultText(res: BulkResult): string {
    const parts = [`Changed ${plural(res.changed, "book")}`];
    if (res.unchanged) parts.push(`${n(res.unchanged)} already as asked`);
    if (res.failed.length) parts.push(`${n(res.failed.length)} failed: ${res.failed.slice(0, 3).map((f) => f.error).join("; ")}`);
    return parts.join(" · ") + ".";
}

function cellValue(b: TableBook, field: EditField): string {
    switch (field) {
        case "title":
            return b.title;
        case "authors":
            return b.authors;
        case "publisher":
            return b.publisher;
        case "date":
            return b.pubdate;
        case "tags":
            return b.tags.join(", ");
        case "languages":
            return b.languages.join(", ");
    }
}

let editing: { cell: HTMLElement; input: HTMLInputElement } | null = null;

body.addEventListener("dblclick", (e) => {
    const cell = (e.target as HTMLElement).closest<HTMLElement>(".lt-editable");
    const uuid = cell?.closest<HTMLElement>(".lt-row")?.dataset["uuid"];
    const col = COLUMNS.find((c) => c.key === cell?.dataset["col"]);
    const r = rows.find((x) => x.b.uuid === uuid);
    if (!cell || !uuid || !col?.edit || !r || editing) return;
    e.preventDefault();
    const field = col.edit;
    const before = cellValue(r.b, field);
    const input = el("input", { type: field === "date" ? "date" : "text", className: "lt-cell-input", value: before, ariaLabel: col.label });
    if (field === "tags") attachSuggest(input, { category: "tag", multi: true });
    if (field === "languages") attachSuggest(input, { category: "language", multi: true, key: languageKey });
    if (field === "authors") attachSuggest(input, { category: "author", multi: true, separators: "&" });
    if (field === "publisher") attachSuggest(input, { category: "publisher" });
    cell.replaceChildren(input);
    cell.classList.add("lt-editing");
    editing = { cell, input };
    input.focus();
    input.select();

    let done = false;
    const finish = async (save: boolean): Promise<void> => {
        if (done) return;
        done = true;
        editing = null;
        const value = input.value.trim();
        if (!save || value === before.trim() || (field === "title" && !value)) {
            draw(true);
            return;
        }
        input.disabled = true;
        try {
            const res = await sendEdit([uuid], { [field]: value });
            say(res.failed.length ? resultText(res) : `Saved “${res.books[0]?.title ?? r.b.title}”.`, res.failed.length ? "error" : "ok");
        } catch (err) {
            say(`Couldn't save: ${errorMessage(err)}`, "error");
        }
        refresh();
    };
    input.addEventListener("keydown", (ev) => {
        if (ev.key === "Enter" && !ev.defaultPrevented) {
            ev.preventDefault();
            void finish(true);
        } else if (ev.key === "Escape") {
            ev.preventDefault();
            ev.stopPropagation();
            void finish(false);
        }
    });
    // Leaving the box saves, as in a spreadsheet; a suggestion click comes back to it.
    input.addEventListener("blur", () => {
        setTimeout(() => {
            if (document.activeElement !== input) void finish(true);
        }, 150);
    });
});

// The dialog.
const dialog = byId("bulk-dialog", HTMLDialogElement);
const form = byId("bulk-form", HTMLFormElement);
const summary = byId("bulk-summary");
const dialogError = byId("bulk-error");
const applyBtn = byId("bulk-apply", HTMLButtonElement);
const removeTags = new Set<string>();
const removeLanguages = new Set<string>();
closeOnBackdropClick(dialog);

const field = (name: string): HTMLInputElement => form.elements.namedItem(name) as HTMLInputElement;
const list = (s: string, sep = ","): string[] =>
    s
        .split(sep)
        .map((x) => x.trim())
        .filter(Boolean);

attachSuggest(field("add_tags"), { category: "tag", multi: true });
attachSuggest(field("tags"), { category: "tag", multi: true });
attachSuggest(field("add_languages"), { category: "language", multi: true, key: languageKey });
attachSuggest(field("languages"), { category: "language", multi: true, key: languageKey });
attachSuggest(field("authors"), { category: "author", multi: true, separators: "&" });
attachSuggest(field("publisher"), { category: "publisher" });
// Typing in a "set" box ticks it.
for (const name of ["tags", "languages", "authors", "publisher", "date"]) {
    field(name).addEventListener("input", () => {
        field(`${name}_on`).checked = true;
        updateSummary();
    });
}
form.addEventListener("input", updateSummary);
form.addEventListener("change", updateSummary);

/** Chips of the values the selected books have, with counts; a click marks one for removal. */
function presentChips(box: HTMLElement, values: Map<string, number>, marked: Set<string>, label: (v: string) => string): void {
    const sorted = [...values].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
    box.replaceChildren(
        ...(sorted.length
            ? sorted.map(([v, k]) => {
                  const b = el("button", { type: "button", className: "lt-chip lt-chip-count" }, label(v), el("span", {}, String(k)));
                  b.setAttribute("aria-pressed", String(marked.has(v)));
                  b.title = `Remove from ${plural(k, "book")}`;
                  b.addEventListener("click", () => {
                      if (marked.has(v)) marked.delete(v);
                      else marked.add(v);
                      b.setAttribute("aria-pressed", String(marked.has(v)));
                      updateSummary();
                  });
                  return b;
              })
            : [el("span", { className: "lt-hint" }, "None.")]),
    );
}

function buildEdit(): BulkEdit {
    const edit: BulkEdit = {};
    if (removeTags.size) edit.remove_tags = [...removeTags];
    const addTags = list(field("add_tags").value);
    if (addTags.length) edit.add_tags = addTags;
    if (field("tags_on").checked) edit.tags = field("tags").value;
    if (removeLanguages.size) edit.remove_languages = [...removeLanguages];
    const addLangs = list(field("add_languages").value);
    if (addLangs.length) edit.add_languages = addLangs;
    if (field("languages_on").checked) edit.languages = field("languages").value;
    if (field("authors_on").checked && field("authors").value.trim()) edit.authors = field("authors").value;
    if (field("publisher_on").checked) edit.publisher = field("publisher").value;
    if (field("date_on").checked) edit.date = field("date").value;
    return edit;
}

function updateSummary(): void {
    const e = buildEdit();
    const parts: string[] = [];
    if (e.tags !== undefined) parts.push(e.tags.trim() ? `tags become “${e.tags.trim()}”` : "all tags removed");
    if (e.remove_tags) parts.push(`remove ${plural(e.remove_tags.length, "tag")}`);
    if (e.add_tags) parts.push(`add ${plural(e.add_tags.length, "tag")}`);
    if (e.languages !== undefined) parts.push(`languages become “${e.languages.trim()}”`);
    if (e.remove_languages) parts.push(`remove ${plural(e.remove_languages.length, "language")}`);
    if (e.add_languages) parts.push(`add ${plural(e.add_languages.length, "language")}`);
    if (e.authors !== undefined) parts.push("set the authors");
    if (e.publisher !== undefined) parts.push(e.publisher.trim() ? "set the publisher" : "clear the publisher");
    if (e.date !== undefined) parts.push(e.date ? "set the date" : "clear the date");
    summary.textContent = parts.length ? `For ${plural(selected.size, "book")}: ${parts.join(", ")}.` : "Nothing to change yet.";
    applyBtn.disabled = parts.length === 0;
}

byId("lt-edit").addEventListener("click", () => {
    const books = selectedBooks();
    if (!books.length) return;
    form.reset();
    removeTags.clear();
    removeLanguages.clear();
    dialogError.hidden = true;
    const tags = new Map<string, number>();
    const langs = new Map<string, number>();
    for (const b of books) {
        for (const t of b.tags) tags.set(t, (tags.get(t) ?? 0) + 1);
        for (const l of b.languages) langs.set(l, (langs.get(l) ?? 0) + 1);
    }
    presentChips(byId("bulk-tags"), tags, removeTags, (v) => v);
    presentChips(byId("bulk-languages"), langs, removeLanguages, languageChipLabel);
    byId("bulk-title").textContent = `Edit ${plural(books.length, "book")}`;
    applyBtn.textContent = `Apply to ${plural(books.length, "book")}`;
    updateSummary();
    dialog.showModal();
});
byId("bulk-cancel").addEventListener("click", () => dialog.close());

form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const edit = buildEdit();
    const uuids = [...selected];
    if (!uuids.length || !Object.keys(edit).length) return;
    applyBtn.disabled = true;
    const label = applyBtn.textContent;
    applyBtn.textContent = `Saving ${plural(uuids.length, "book")}…`;
    dialogError.hidden = true;
    try {
        const res = await sendEdit(uuids, edit);
        dialog.close();
        say(resultText(res), res.failed.length ? "error" : "ok");
        refresh();
    } catch (err) {
        dialogError.hidden = false;
        dialogError.textContent = errorMessage(err);
    } finally {
        applyBtn.disabled = false;
        applyBtn.textContent = label;
    }
});

// ---------------------------------------------------------------------------
// Toolbar, the URL, keys
// ---------------------------------------------------------------------------

searchInput.addEventListener("input", () => {
    search = searchInput.value;
    scheduleRefresh();
});
dupSelect.addEventListener("change", () => {
    dupMode = dupSelect.value;
    refresh();
});
onlySelectedBox.addEventListener("change", () => {
    onlySelected = onlySelectedBox.checked;
    refresh();
});
byId("lt-clear").addEventListener("click", () => {
    for (const c of COLUMNS) {
        delete filters[c.key];
        filterInputs.get(c.key)!.value = "";
    }
    search = searchInput.value = "";
    dupMode = dupSelect.value = "";
    onlySelected = onlySelectedBox.checked = false;
    refresh();
});
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !modalOpen() && !editing && selected.size && !(e.target instanceof HTMLInputElement && e.target.value)) {
        selected.clear();
        selectionChanged();
    }
});

function saveView(): void {
    const q = new URLSearchParams();
    if (search) q.set("q", search);
    if (dupMode) q.set("dupes", dupMode);
    if (sortKey !== "title" || sortDesc) q.set("sort", `${sortDesc ? "-" : ""}${sortKey}`);
    for (const c of COLUMNS) if (filters[c.key]?.trim()) q.set(`f.${c.key}`, filters[c.key]!);
    const s = q.toString();
    history.replaceState(history.state, "", s ? `?${s}` : location.pathname);
}

function loadView(): void {
    const q = new URLSearchParams(location.search);
    search = searchInput.value = q.get("q") ?? "";
    dupMode = q.get("dupes") ?? "";
    dupSelect.value = dupMode;
    const sort = q.get("sort") ?? "";
    const key = sort.replace(/^-/, "") as ColKey;
    if (COLUMNS.some((c) => c.key === key)) [sortKey, sortDesc] = [key, sort.startsWith("-")];
    for (const c of COLUMNS) {
        const f = q.get(`f.${c.key}`);
        if (f) filters[c.key] = f;
    }
    if (q.get("selected")) {
        // From the grid's "Edit in table".
        try {
            const uuids = JSON.parse(sessionStorage.getItem(BULK_SELECTION_KEY) ?? "[]") as unknown;
            if (Array.isArray(uuids)) for (const u of uuids) if (typeof u === "string") selected.add(u);
            sessionStorage.removeItem(BULK_SELECTION_KEY);
        } catch {
            /* no storage: nothing preselected */
        }
        onlySelected = onlySelectedBox.checked = selected.size > 0;
    }
}

async function start(): Promise<void> {
    loadView();
    buildHead();
    try {
        const [data] = await Promise.all([api<TableData>("GET", "/api/library/table"), loadLanguageCodes()]);
        rows = data.books.map(makeRow);
        notDupSets = data.not_duplicates;
    } catch (err) {
        countEl.textContent = "";
        say(`Couldn't load the library: ${errorMessage(err)}`, "error");
        return;
    }
    loaded();
}

/** After the rows (re)load: selected books no longer in the library don't count. */
function loaded(): void {
    const known = new Set(rows.map((r) => r.b.uuid));
    for (const u of selected) if (!known.has(u)) selected.delete(u);
    fillDatalists();
    refresh();
}

// Merges and edits done in other tabs (a Merge link opened in a new tab,
// say): the rows again when this tab is looked at again, unless something
// is being edited here.
let reloading = false;
let reloadedAt = 0;
async function reloadRows(): Promise<void> {
    // Switching tabs fires both events: one fetch.
    if (document.visibilityState !== "visible" || reloading || editing || modalOpen() || rows.length === 0) return;
    if (Date.now() - reloadedAt < 2000) return;
    reloading = true;
    reloadedAt = Date.now();
    try {
        const data = await api<TableData>("GET", "/api/library/table");
        if (editing || modalOpen()) return;
        const before = rows.length;
        rows = data.books.map(makeRow);
        notDupSets = data.not_duplicates;
        loaded();
        if (rows.length < before) say(`${plural(before - rows.length, "book")} fewer since this page loaded (merged or removed elsewhere).`, "ok");
    } catch {
        /* keep what's shown */
    } finally {
        reloading = false;
    }
}
document.addEventListener("visibilitychange", () => void reloadRows());
window.addEventListener("focus", () => void reloadRows());

void start();
