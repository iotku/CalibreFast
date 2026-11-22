// The bulk metadata editor (/library/edit; Go: bulk_edit.go). Every book is
// a row; the table is filtered, sorted and searched here, in the browser,
// so finding duplicates and faulty tags or languages is instant. Changes go
// to POST /api/library/bulk, which answers the changed rows.
//
// Only the rows on screen are in the DOM: every row is ROW_H tall, so row
// i sits at i × ROW_H in a body as tall as all of them.
//
// The filters, sort, duplicate mode and tag check are kept in the URL (replaceState),
// so coming back (with Back) finds the same view.
//
// The same page edits the import queue (/import/edit; Go: imports_edit.go)
// when <main> says data-source="queue": the rows are the books waiting to be
// added, edits change the metadata they'll be added with, and a Library
// column shows whether each looks already in the library. Merging, removing
// and marking not duplicates are the library's only.

import { api, byId, closeOnBackdropClick, el, errorMessage, icon, lineIcon, modalOpen } from "./dom.js";
import { attachSuggest } from "./suggest.js";
import { joinAuthors, splitAuthors, tidyAuthors } from "./authors.js";
import { ratingLabel } from "./rating.js";
import { deleteBooks } from "./delete-books.js";
import { previewUrl } from "./formats.js";
import { languageChipLabel, languageKey, loadLanguageCodes } from "./languages.js";
import type { BulkEdit, BulkFailureKept, BulkJob, BulkResult, FormatRepair, NotDuplicateSet, TableBook } from "./types.js";

/** Editing the import queue's waiting books, not the library's. */
const QUEUE = document.querySelector<HTMLElement>(".lt-main")?.dataset["source"] === "queue";
const SOURCE = QUEUE
    ? { table: "/api/imports/table", bulk: "/api/imports/bulk", name: "the import queue" }
    : { table: "/api/library/table", bulk: "/api/library/bulk", name: "the library" };

const ROW_H = 34; // px; .lt-row's height in library-edit.css
const OVERSCAN = 8; // rows drawn beyond the screen, each way
/** sessionStorage: books the grid's "Edit in table" picked (select-books.ts). */
const BULK_SELECTION_KEY = "cf-bulk-selection";
/** At most this many books are merged at once (Go: mergeMaxBooks). */
const MERGE_MAX = 20;

type ColKey = "title" | "authors" | "series" | "publisher" | "pubdate" | "languages" | "tags" | "formats" | "added" | "library";
/** The bulk edit field a column's cells edit. */
type EditField = "title" | "authors" | "series" | "publisher" | "date" | "tags" | "languages";

interface Column {
    key: ColKey;
    label: string;
    /** The cell holds several values (chips). */
    list?: boolean;
    edit?: EditField;
    /** Its text, for searching and sorting. */
    text(b: TableBook): string;
    items?(b: TableBook): string[];
    /** Empty, or the value a book gets when nothing was known ("Unknown", "und"): its checkbox filter. */
    blank(b: TableBook): boolean;
}

const unknown = (s: string): boolean => !s.trim() || s.trim().toLowerCase() === "unknown";

const COLUMNS: Column[] = [
    { key: "title", label: "Title", edit: "title", text: (b) => b.title, blank: (b) => unknown(b.title) },
    { key: "authors", label: "Authors", edit: "authors", text: (b) => b.authors, blank: (b) => b.authors.split("&").every(unknown) },
    { key: "series", label: "Series", edit: "series", text: (b) => (b.series ? `${b.series} [${b.series_index}]` : ""), blank: (b) => !b.series.trim() },
    { key: "publisher", label: "Publisher", edit: "publisher", text: (b) => b.publisher, blank: (b) => unknown(b.publisher) },
    // "" is Calibre's "no date" (year 101) too: tableDate in bulk_edit.go.
    { key: "pubdate", label: "Published", edit: "date", text: (b) => b.pubdate, blank: (b) => !b.pubdate },
    {
        key: "languages", label: "Languages", list: true, edit: "languages", text: (b) => b.languages.join(", "), items: (b) => b.languages,
        blank: (b) => b.languages.every((l) => l.trim().toLowerCase() === "und"),
    },
    { key: "tags", label: "Tags", list: true, edit: "tags", text: (b) => b.tags.join(", "), items: (b) => b.tags, blank: (b) => b.tags.length === 0 },
    { key: "formats", label: "Formats", list: true, text: (b) => b.formats.join(", "), items: (b) => b.formats, blank: (b) => b.formats.length === 0 },
    { key: "added", label: QUEUE ? "Queued" : "Added", text: (b) => b.added, blank: (b) => !b.added },
];
if (QUEUE) {
    // Go: tableBook.Library, the queue's duplicate check. Its tick box: new books.
    COLUMNS.push({ key: "library", label: "Library", text: (b) => b.library ?? "", blank: (b) => b.library === "new" });
}

/**
 * The grid's columns, in order, with their usual widths (as --lt-cols in
 * library-edit.css) and the least each takes. A column dragged wider or
 * narrower gets a width of its own (colWidths), kept in localStorage.
 */
const GRID: { key: "actions" | "check" | ColKey; def: string; min: number }[] = [
    { key: "actions", def: "136px", min: 136 },
    { key: "check", def: "34px", min: 34 },
    { key: "title", def: "minmax(220px, 2.2fr)", min: 220 },
    { key: "authors", def: "minmax(150px, 1.4fr)", min: 150 },
    { key: "series", def: "140px", min: 140 },
    { key: "publisher", def: "150px", min: 150 },
    { key: "pubdate", def: "112px", min: 112 },
    { key: "languages", def: "170px", min: 170 },
    { key: "tags", def: "minmax(180px, 2fr)", min: 180 },
    { key: "formats", def: "110px", min: 110 },
    { key: "added", def: "96px", min: 96 },
];
if (QUEUE) GRID.push({ key: "library", def: "104px", min: 96 });
const COL_MIN = 56;
const COL_MAX = 2000;
const COL_WIDTHS_KEY = QUEUE ? "cf-queue-col-widths" : "cf-bulk-col-widths";

/** What the Formats filter matches for files only in a book's folder, not in metadata.db. */
const FOLDER_ONLY = "folder only";

/** The Library column's values, explained. */
const LIBRARY_TITLES: Record<string, string> = {
    checking: "Being checked against the library",
    "same file": "The library already has one of its files: it can't be added",
    "in library": "A library book has the same title and authors: adding it asks first",
    new: "Not in the library",
};

const PENCIL = () => icon(lineIcon(13), "M12 20h9", "M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z");

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
    // "folder" in the Formats filter finds books whose files metadata.db doesn't list.
    if (b.formats_from_folder) {
        items.formats = [...(items.formats ?? []), FOLDER_ONLY];
        hay.formats += ` ${FOLDER_ONLY}`;
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
/** Columns whose checkbox is ticked: only books with nothing (or the default) there. */
const blankOnly = new Set<ColKey>();
const blankBoxes = new Map<ColKey, HTMLInputElement>();
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
/** Tag checks: "", "unique" (a tag no other book has), "semicolon" (a tag with ;) or "long". */
type TagCheck = "" | "unique" | "semicolon" | "long";
const TAG_CHECKS: TagCheck[] = ["", "unique", "semicolon", "long"];
let tagCheck: TagCheck = "";
const TAG_LEN_DEFAULT = 40;
/** "long": tags longer than this many characters. */
let tagLen = TAG_LEN_DEFAULT;
/** The tag separator "Split on ;" splits on (Go: bulkEdit.SplitTags). */
const SPLIT_SEP = ";";
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
const tagCheckSelect = byId("lt-tagcheck", HTMLSelectElement);
const tagLenInput = byId("lt-taglen", HTMLInputElement);
const tagLenWrap = byId("lt-taglen-wrap");
const splitBtn = byId("lt-split", HTMLButtonElement);
const bar = byId("lt-bar");
const selectedEl = byId("lt-selected");
const mergeBtn = byId("lt-merge", HTMLButtonElement);

const undoBtn = byId("lt-undo", HTMLButtonElement);
/** What the status line's Undo does: the last tag change, until something else is said. */
let undoAction: (() => Promise<void>) | null = null;

function say(text: string, kind: "ok" | "error" | "" = ""): void {
    undoAction = null;
    undoBtn.hidden = true;
    statusEl.hidden = !text;
    statusEl.textContent = statusEl.title = text;
    statusEl.className = `lt-status${kind ? ` ${kind}` : ""}`;
}

function offerUndo(text: string, action: () => Promise<void>): void {
    say(text, "ok");
    undoAction = action;
    undoBtn.hidden = false;
}

undoBtn.addEventListener("click", async () => {
    const action = undoAction;
    if (!action) return;
    undoBtn.disabled = true;
    try {
        await action();
    } finally {
        undoBtn.disabled = false;
    }
});

const n = (k: number): string => k.toLocaleString();
const plural = (k: number, one: string, many = `${one}s`): string => `${n(k)} ${k === 1 ? one : many}`;

// ---------------------------------------------------------------------------
// Header: titles that sort, and a filter box per column
// ---------------------------------------------------------------------------

const allBox = el("input", { type: "checkbox", title: "Select every book shown", ariaLabel: "Select every book shown" });
const sortButtons = new Map<ColKey, HTMLButtonElement>();
const filterInputs = new Map<ColKey, HTMLInputElement>();
const datalists = new Map<ColKey, HTMLDataListElement>();

/**
 * The box with a ✕ after it that empties it (as typing would: an "input"
 * event), shown while there's something to clear. Browsers' own search-box
 * ✕ is hidden (library-edit.css): Firefox has none, and it's tiny.
 */
function clearable(input: HTMLInputElement, label: string): HTMLElement {
    const x = el("button", { type: "button", className: "lt-x", title: label, ariaLabel: label }, "✕");
    x.addEventListener("click", () => {
        input.value = "";
        input.dispatchEvent(new Event("input", { bubbles: true }));
        input.focus();
    });
    return el("span", { className: "lt-clearable" }, input, x);
}

// ---------------------------------------------------------------------------
// Column widths: drag the edge of a column's name (or use ←/→ on it)
// ---------------------------------------------------------------------------

const colWidths: Partial<Record<string, number>> = {};
try {
    const saved = JSON.parse(localStorage.getItem(COL_WIDTHS_KEY) ?? "{}") as unknown;
    if (saved && typeof saved === "object") {
        for (const g of GRID) {
            const w = (saved as Record<string, unknown>)[g.key];
            if (typeof w === "number" && w >= COL_MIN && w <= COL_MAX) colWidths[g.key] = Math.round(w);
        }
    }
} catch {
    /* no storage: the usual widths */
}

function saveColWidths(): void {
    try {
        if (Object.keys(colWidths).length) localStorage.setItem(COL_WIDTHS_KEY, JSON.stringify(colWidths));
        else localStorage.removeItem(COL_WIDTHS_KEY);
    } catch {
        /* this page's widths only */
    }
}

/** The grid's columns, with any widths set here; the table is at least as wide as they need. */
function applyColWidths(): void {
    table.style.setProperty("--lt-cols", GRID.map((g) => (colWidths[g.key] ? `${colWidths[g.key]}px` : g.def)).join(" "));
    table.style.minWidth = `${GRID.reduce((t, g) => t + (colWidths[g.key] ?? g.min), 0)}px`;
}

/** The handle on the right edge of a column's name. */
function resizeHandle(c: Column): HTMLElement {
    const grip = el("span", {
        className: "lt-resize",
        role: "separator",
        tabIndex: 0,
        title: "Drag to resize; double-click for the usual width",
        ariaLabel: `Resize the ${c.label.toLowerCase()} column`,
    });
    grip.setAttribute("aria-orientation", "vertical");
    const width = (): number => grip.parentElement!.getBoundingClientRect().width;
    const set = (w: number): void => {
        colWidths[c.key] = Math.round(Math.min(COL_MAX, Math.max(COL_MIN, w)));
        applyColWidths();
    };
    const done = (): void => {
        saveColWidths();
        draw(true); // chips that fit (or don't) now
    };
    grip.addEventListener("pointerdown", (e) => {
        if (e.button !== 0) return;
        e.preventDefault();
        const x0 = e.clientX;
        const w0 = width();
        grip.setPointerCapture(e.pointerId);
        grip.classList.add("lt-resizing");
        let frame = 0;
        const move = (ev: PointerEvent): void => {
            cancelAnimationFrame(frame);
            frame = requestAnimationFrame(() => set(w0 + ev.clientX - x0));
        };
        const up = (): void => {
            cancelAnimationFrame(frame);
            grip.removeEventListener("pointermove", move);
            grip.removeEventListener("pointerup", up);
            grip.removeEventListener("pointercancel", up);
            grip.classList.remove("lt-resizing");
            done();
        };
        grip.addEventListener("pointermove", move);
        grip.addEventListener("pointerup", up);
        grip.addEventListener("pointercancel", up);
    });
    grip.addEventListener("dblclick", () => {
        delete colWidths[c.key];
        applyColWidths();
        done();
    });
    grip.addEventListener("keydown", (e) => {
        if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
        e.preventDefault();
        set(width() + (e.key === "ArrowRight" ? 16 : -16));
        done();
    });
    return grip;
}

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
        // Beside the label: only books with nothing (or the default) in this column.
        const blank = el("input", {
            type: "checkbox",
            className: "lt-blank",
            checked: blankOnly.has(c.key),
            title: `Only books with no ${c.label.toLowerCase()}, or the default one (Unknown, und)`,
            ariaLabel: `Only books with an empty or default ${c.label.toLowerCase()}`,
        });
        blank.addEventListener("change", () => {
            if (blank.checked) blankOnly.add(c.key);
            else blankOnly.delete(c.key);
            refresh();
        });
        blankBoxes.set(c.key, blank);
        titles.append(el("span", { className: `lt-cell lt-col-${c.key} lt-head-cell`, role: "columnheader" }, btn, blank, resizeHandle(c)));

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
        boxes.append(el("span", { className: `lt-cell lt-col-${c.key}` }, clearable(input, `Clear the ${c.label.toLowerCase()} filter`)));
    }
    // The actions come first, then the tick box: no scrolling sideways to reach them.
    titles.prepend(el("span", { className: "lt-cell lt-actions", role: "columnheader" }));
    boxes.prepend(el("span", { className: "lt-cell lt-actions" }));
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
    const blankCols = COLUMNS.filter((c) => blankOnly.has(c.key));

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

    const tagTest = tagCheckTest();
    shown = rows.filter((r) => {
        if (onlySelected && !selected.has(r.b.uuid)) return false;
        if (groups && !groups.has(r.dupKey)) return false;
        if (tagTest && !r.b.tags.some(tagTest)) return false;
        for (const w of words) if (!r.all.includes(w)) return false;
        for (const [c, t] of tests) if (!t(r.hay[c.key], r.items[c.key])) return false;
        for (const c of blankCols) if (!c.blank(r.b)) return false;
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
    shownSet = new Set(shown.map((r) => r.b.uuid));
    updatePinned();
}

/**
 * Ticked books the filters hide, pinned above the list under a heading: an
 * edit to the selected books reaches them too, so they stay in sight.
 */
let pinned: Row[] = [];
let shownSet = new Set<string>();
/** The rows above the shown ones: the heading and the pinned rows (none if nothing's pinned). */
const pinOffset = (): number => (pinned.length ? pinned.length + 1 : 0);

/** Work out the pinned rows again; true if they changed. */
function updatePinned(): boolean {
    const next = selected.size ? rows.filter((r) => selected.has(r.b.uuid) && !shownSet.has(r.b.uuid)).sort(compare) : [];
    const same = next.length === pinned.length && next.every((r, i) => r === pinned[i]);
    pinned = next;
    return !same;
}

/** The body's height and row count, for the pinned rows and the shown ones. */
function sizeBody(): void {
    const total = pinOffset() + shown.length;
    table.setAttribute("aria-rowcount", String(total));
    body.style.height = `${total * ROW_H}px`;
}

/** The heading over the pinned rows, with a button to untick them. */
function pinHeading(): HTMLElement {
    const untick = el("button", { type: "button", className: "lt-action", title: "Untick the selected books the filters hide" }, "Untick them");
    untick.addEventListener("click", () => {
        for (const r of pinned) selected.delete(r.b.uuid);
        selectionChanged();
    });
    const k = pinned.length;
    const head = el(
        "div",
        { className: "lt-row lt-pin-head", role: "row" },
        el(
            "span",
            { className: "lt-pin-label", role: "gridcell" },
            `${plural(k, "selected book")} the filters hide (still selected: edits reach ${k === 1 ? "it" : "them"} too)`,
            untick,
        ),
    );
    head.style.top = "0px";
    head.setAttribute("aria-rowindex", "1");
    return head;
}

/** The test a tag must pass for its book to show, under the tag check; null: no check. */
function tagCheckTest(): ((tag: string) => boolean) | null {
    switch (tagCheck) {
        case "unique": {
            // How many books have each tag, in any case.
            const books = new Map<string, number>();
            for (const r of rows) {
                for (const t of new Set(r.b.tags.map((t) => t.toLowerCase()))) books.set(t, (books.get(t) ?? 0) + 1);
            }
            return (t) => books.get(t.toLowerCase()) === 1;
        }
        case "semicolon":
            return (t) => t.includes(SPLIT_SEP);
        case "long":
            return (t) => [...t].length > tagLen;
    }
    return null;
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
    sizeBody();
    emptyEl.hidden = shown.length > 0 || rows.length === 0;
    saveView();
    draw(true);
    selectionChanged();
}

let drawn = { from: -1, to: -1 };
function draw(force = false): void {
    const top = Math.max(0, scroller.scrollTop - body.offsetTop);
    const from = Math.max(0, Math.floor(top / ROW_H) - OVERSCAN);
    const pins = pinOffset();
    const to = Math.min(pins + shown.length, Math.ceil((top + scroller.clientHeight) / ROW_H) + OVERSCAN);
    if (!force && from === drawn.from && to === drawn.to) return;
    drawn = { from, to };
    const out: HTMLElement[] = [];
    for (let j = from; j < to; j++) {
        if (j >= pins) out.push(rowElement(shown[j - pins]!, j - pins));
        else if (j === 0) out.push(pinHeading());
        else out.push(rowElement(pinned[j - 1]!, j - 1, true));
    }
    closeExpanded();
    closeCellMenu();
    body.replaceChildren(...out);
    for (const cell of body.querySelectorAll<HTMLElement>(".lt-list-cell")) addMore(cell);
}

// ---------------------------------------------------------------------------
// Tags and languages that don't fit: "+N" opens them all over the rows below
// (rows keep one height, which the drawing relies on)
// ---------------------------------------------------------------------------

/**
 * If a list cell's chips don't all fit: the first is cut short ("Engl…"),
 * those that don't fit after it are hidden, and a button at the end shows
 * how many ("+3", or "⋯" when only the first is cut) and opens them all.
 */
function addMore(cell: HTMLElement): void {
    const chips = [...cell.querySelectorAll<HTMLElement>(".lt-chip")];
    const cut = (c: HTMLElement): boolean => {
        const t = c.querySelector<HTMLElement>(".lt-chip-label") ?? c;
        return t.scrollWidth > t.clientWidth + 1;
    };
    if (chips.length === 0 || (cell.scrollWidth <= cell.clientWidth + 1 && !chips.some(cut))) return;
    cell.classList.add("lt-has-more"); // room for the button
    const limit = cell.getBoundingClientRect().right - parseFloat(getComputedStyle(cell).paddingRight);
    let hidden = 0;
    for (const c of chips.slice(1)) {
        if (hidden || c.getBoundingClientRect().right > limit) {
            c.hidden = true;
            hidden++;
        }
    }
    // The first chip whole before any after it: hide those from the end.
    for (let i = chips.length - hidden - 1; i > 0 && cut(chips[0]!); i--) {
        chips[i]!.hidden = true;
        hidden++;
    }
    const more = el("button", { type: "button", className: "lt-more" }, hidden ? `+${hidden}` : "⋯");
    more.title = `Show all ${chips.length}`;
    more.ariaLabel = `Show all ${chips.length} ${cell.dataset["col"] ?? ""}`;
    cell.append(more);
}

let expanded: HTMLElement | null = null;

function closeExpanded(): void {
    expanded?.closest(".lt-row")?.classList.remove("lt-row-open");
    expanded?.remove();
    expanded = null;
}

/** Whether the "+N" panel open is cell's. */
const isExpanded = (cell: HTMLElement): boolean =>
    expanded !== null && expanded.dataset["col"] === cell.dataset["col"] && expanded.closest(".lt-row") === cell.closest(".lt-row");

/** Open the book's "+N" panel for key, once a box open elsewhere is saved (that redraws the rows). */
async function expandAt(uuid: string, key: ColKey): Promise<void> {
    if (editing) {
        await editing.finish(true);
        if (editing) return;
    }
    const cell = rowOnScreen(uuid)?.querySelector<HTMLElement>(`.lt-cell[data-col="${key}"]`);
    if (cell) expand(cell);
}

/** Every chip of a list cell, wrapped, in a panel over the rows below it. */
function expand(cell: HTMLElement): void {
    closeExpanded();
    const row = cell.closest<HTMLElement>(".lt-row");
    if (!row) return;
    const panel = el("div", { className: "lt-expanded" });
    panel.dataset["col"] = cell.dataset["col"] ?? "";
    for (const node of cell.querySelectorAll<HTMLElement>(".lt-cell-edit, .lt-chip")) {
        const copy = node.cloneNode(true) as HTMLElement;
        copy.hidden = false;
        panel.append(copy);
    }
    panel.style.left = `${cell.offsetLeft}px`;
    panel.style.minWidth = `${cell.offsetWidth}px`;
    row.classList.add("lt-row-open");
    row.append(panel);
    expanded = panel;
}

document.addEventListener("click", (e) => {
    const t = e.target as HTMLElement;
    // Not for a click on something since removed (a tag the click redrew): it was in the table.
    if (expanded && t.isConnected && !expanded.contains(t) && !t.closest(".lt-more")) closeExpanded();
});

function chip(col: ColKey, value: string): HTMLElement {
    return el(
        "button",
        { type: "button", className: "lt-chip", title: `${value}: right-click for options` },
        col === "languages" ? languageChipLabel(value) : value,
    );
}

/**
 * Tags removed with a chip's ✕, by book: shown struck through at the end of
 * the book's tags, with Undo, as in the metadata editor (while the book
 * doesn't have them again).
 */
const removedTags = new Map<string, string[]>();
const sameTag = (a: string, b: string): boolean => a.toLowerCase() === b.toLowerCase();

/** A tag in a row: its name filters by it, its ✕ removes it from the book. */
function tagChip(value: string): HTMLElement {
    const c = el(
        "span",
        { className: "lt-chip lt-chip-tag" },
        el("button", { type: "button", className: "lt-chip-label", title: `${value}: click to edit; right-click for options (filter, split…)` }, value),
        el("button", { type: "button", className: "lt-chip-x", title: `Remove ${value} from this book`, ariaLabel: `Remove the tag ${value}` }, "✕"),
    );
    c.dataset["value"] = value;
    return c;
}

/** A tag removed with its ✕: Undo puts it back. */
function removedChip(value: string): HTMLElement {
    const c = el(
        "span",
        { className: "lt-chip lt-chip-tag lt-chip-removed", title: `Removed from this book` },
        el("span", { className: "lt-chip-label" }, value),
        el("button", { type: "button", className: "lt-chip-x lt-chip-undo", title: `Put ${value} back`, ariaLabel: `Put the tag ${value} back` }, "Undo"),
    );
    c.dataset["value"] = value;
    return c;
}

/** Row i of the shown ones, or (pin) of the pinned ones. */
function rowElement(r: Row, i: number, pin = false): HTMLElement {
    const b = r.b;
    const on = selected.has(b.uuid);
    const row = el("div", { className: "lt-row", role: "row" });
    row.dataset["uuid"] = b.uuid;
    const at = pin ? i + 1 : i + pinOffset();
    row.style.top = `${at * ROW_H}px`;
    row.setAttribute("aria-rowindex", String(at + 1));
    if (pin) row.classList.add("lt-row-pinned");
    if (pin && i === pinned.length - 1) row.classList.add("lt-pin-last");
    // The duplicate groups are the shown rows'.
    const dup = pin ? "" : dupMode;
    row.setAttribute("aria-selected", String(on));
    if (dup) row.classList.add(r.group % 2 ? "lt-group-odd" : "lt-group-even");
    if (dup && i > 0 && shown[i - 1]!.dupKey !== r.dupKey) row.classList.add("lt-group-start");

    const box = el("input", { type: "checkbox", checked: on, ariaLabel: `Select “${b.title}”` });
    row.append(el("span", { className: "lt-cell lt-check" }, box));
    for (const c of COLUMNS) {
        const cell = el("span", { className: `lt-cell lt-col-${c.key}`, role: "gridcell" });
        cell.dataset["col"] = c.key;
        if (c.items) {
            cell.classList.add("lt-list-cell");
            const values = c.items(b);
            const unlisted = c.key === "formats" && b.formats_from_folder;
            if (c.edit) {
                // A click on a chip edits that one: this edits them all (as a click beside them does).
                const pen = el("button", { type: "button", className: "lt-cell-edit", title: `Edit ${c.label.toLowerCase()}`, ariaLabel: `Edit the ${c.label.toLowerCase()} of “${b.title}”` });
                pen.append(PENCIL());
                cell.append(pen);
            }
            for (const v of values) {
                const ch = c.key === "tags" ? tagChip(v) : chip(c.key, v);
                ch.dataset["value"] = v;
                if (unlisted) ch.classList.add("lt-chip-unlisted");
                cell.append(ch);
            }
            if (c.key === "tags") {
                for (const t of removedTags.get(b.uuid) ?? []) if (!values.some((v) => sameTag(v, t))) cell.append(removedChip(t));
            }
            cell.title = unlisted
                ? `${values.join(", ")}: in the book's folder, but metadata.db lists no files for this book, so Calibre doesn't show them (CalibreFast reads the folder). Filter Formats by “folder” to find these books.`
                : values.join(", ");
        } else {
            const text = c.text(b);
            cell.textContent = text;
            cell.title = text;
            if (c.key === "library") {
                cell.title = LIBRARY_TITLES[text] ?? text;
                if (text === "checking") cell.classList.add("lt-lib-checking");
                if (text === "in library" || text === "same file") cell.classList.add("lt-lib-dup");
            }
        }
        if (c.edit) cell.classList.add("lt-editable");
        if (selectedCell?.uuid === b.uuid && selectedCell.col === c.key) cell.classList.add("lt-cell-selected");
        row.append(cell);
    }
    const actions = el("span", { className: "lt-cell lt-actions" });
    actions.append(
        QUEUE
            ? el("a", { href: `/import/${encodeURIComponent(b.uuid)}`, target: "_blank", className: "lt-action", title: "Open its review page (new tab)", ariaLabel: `Review “${b.title}”` }, "Review")
            : el("a", { href: `/book/edit/${encodeURIComponent(b.uuid)}`, target: "_blank", className: "lt-action", title: "Open in the metadata editor (new tab)", ariaLabel: `Edit “${b.title}”` }, "Edit"),
    );
    // A group's buttons: Merge on its first row, Not Dupe on its second (a
    // group has at least two books, so the column stays narrow), unless a
    // filter leaves only one of them shown.
    const first = !QUEUE && dup !== "" && (i === 0 || shown[i - 1]!.dupKey !== r.dupKey);
    const second = !QUEUE && dup !== "" && !first && (i === 1 || shown[i - 2]!.dupKey !== r.dupKey);
    // Nothing acts on a group outside the duplicates view: the space the
    // group buttons take there opens the book in the reader instead.
    if (!dup) {
        const href = previewUrl(b.uuid, b.formats, QUEUE);
        if (href) {
            actions.append(
                el("a", { href, target: "_blank", className: "lt-action", title: "Open it in the reader to look something up; nothing is saved (new tab)", ariaLabel: `Preview “${b.title}”` }, "Preview"),
            );
        }
    }
    if (dup === MARKED && first) {
        const un = el("button", { type: "button", className: "lt-action lt-unmark", title: "They may be duplicates after all: show them in the duplicates view again" }, "Unmark");
        un.dataset["set"] = r.dupKey.replace(/^set:/, "");
        actions.append(un);
    } else if (dup && dup !== MARKED && (first || second)) {
        const group = groupOf(r);
        const size = group.length;
        if (first && size > 1 && size <= MERGE_MAX) {
            // A link, so a middle click (or Ctrl/⌘-click) opens the merge in a new tab.
            actions.append(
                el("a", { href: mergeLink(group.map((x) => x.b.uuid)), className: "lt-action lt-merge-group", title: `Merge these ${size} books into one` }, `Merge ${size}`),
            );
        }
        if (second || size === 1) {
            actions.append(
                el("button", { type: "button", className: "lt-action lt-notdup", title: "Not duplicates: these books only look alike. Leaves them out of the duplicates view (Duplicates → Marked not duplicates undoes it)" }, "Not Dupe"),
            );
        }
    }
    // First, then the tick box and the title.
    row.prepend(actions);
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
    if (updatePinned()) {
        sizeBody();
        draw(true);
    }
    const k = selected.size;
    bar.hidden = k === 0;
    selectedEl.textContent = `${plural(k, "book")} selected`;
    mergeBtn.disabled = k < 2 || k > MERGE_MAX;
    const books = selectedBooks();
    if (tagCheck === "semicolon") {
        splitBtn.textContent = `Split on ${SPLIT_SEP}`;
        splitBtn.title = `Make each tag with a ${SPLIT_SEP} in it the tags it lists (Undo puts them back)`;
        splitBtn.disabled = !books.some((b) => b.tags.some((t) => t.includes(SPLIT_SEP)));
    } else {
        splitBtn.textContent = "Split tags…";
        splitBtn.title = "Split the selected books' tags on a separator you choose (Undo puts them back)";
        splitBtn.disabled = !books.some((b) => b.tags.length > 0);
    }
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

// A click edits a cell, or one tag or language; Ctrl/⌘-click ticks the row,
// Shift-click a range (as in a file manager). On a touch screen a tap ticks
// the row. The menu (filter by it, edit, copy…) is a right-click or a long press.
body.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    if (cellMenu?.contains(target)) return; // its items handle themselves
    const hadMenu = cellMenu !== null;
    deselectCell();
    // The click that ends a long press (its menu is up), or a Ctrl-click done already.
    if (Date.now() < quietUntil || ctrlTicked) return;
    const row = target.closest<HTMLElement>(".lt-row");
    const uuid = row?.dataset["uuid"];
    if (!row || !uuid) return;
    if (target instanceof HTMLInputElement && target.type === "checkbox") {
        toggle(uuid, target.checked, e.shiftKey);
        return;
    }
    if (editing?.area.contains(target)) return;
    // A ✕ while a box is open only closes the box (it may be a slip).
    if (editing && target.closest(".lt-chip-x")) return;
    const pen = target.closest<HTMLElement>(".lt-cell-edit");
    if (pen) {
        // In the row, or in its expanded panel: the row's cell for that column.
        const col = pen.closest<HTMLElement>("[data-col]")?.dataset["col"] as ColKey | undefined;
        closeExpanded();
        if (col) void editAt(uuid, col);
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
    const x = target.closest<HTMLElement>(".lt-chip-x");
    if (x) {
        const tag = x.closest<HTMLElement>(".lt-chip")?.dataset["value"];
        if (tag !== undefined) void (x.classList.contains("lt-chip-undo") ? putTagBack(uuid, tag) : removeTag(uuid, tag));
        return;
    }
    const more = target.closest<HTMLElement>(".lt-more");
    if (more) {
        const key = more.closest<HTMLElement>(".lt-cell")?.dataset["col"] as ColKey | undefined;
        const open = expanded !== null && expanded.dataset["col"] === key && expanded.closest(".lt-row") === row;
        if (open) closeExpanded();
        else if (key) void expandAt(uuid, key);
        return;
    }
    if (target.closest("a, .lt-actions")) return;
    // A click away from a menu only closes it.
    if (hadMenu) return;
    if (e.shiftKey) {
        toggle(uuid, true, true);
        return;
    }
    if (e.ctrlKey || e.metaKey || touchTap) {
        toggle(uuid, !selected.has(uuid), false);
        return;
    }
    const key = target.closest<HTMLElement>("[data-col]")?.dataset["col"];
    const col = COLUMNS.find((c) => c.key === key);
    if (!col?.edit) return;
    const ch = target.closest<HTMLElement>(".lt-chip:not(.lt-chip-removed)");
    // A tag in the "+N" panel is edited there (editAt keeps it open).
    const inPanel = ch !== null && expanded !== null && expanded.contains(ch);
    if (!inPanel) closeExpanded();
    void editAt(uuid, col.key, ch?.dataset["value"], inPanel);
});

/** A Ctrl-click under way: on a Mac that's a right-click (contextmenu, no click), but here it ticks the row. */
let ctrlDown = false;
/** That Ctrl-click ticked its row already (at contextmenu), should a click come too. */
let ctrlTicked = false;

// Shift-click picks a range, not the text in it.
body.addEventListener("mousedown", (e) => {
    ctrlDown = e.button === 0 && e.ctrlKey;
    ctrlTicked = false;
    if (e.shiftKey && !(e.target as HTMLElement).closest("input")) e.preventDefault();
});

/** Open the menu for what was right-clicked or long-pressed: a chip's, or its cell's. */
function menuAt(target: HTMLElement): void {
    const row = target.closest<HTMLElement>(".lt-row");
    const uuid = row?.dataset["uuid"];
    const key = target.closest<HTMLElement>("[data-col]")?.dataset["col"];
    const col = COLUMNS.find((c) => c.key === key);
    if (!uuid || !col) return;
    const ch = target.closest<HTMLElement>(".lt-chip:not(.lt-chip-removed)");
    const value = ch?.dataset["value"];
    // The same one again closes its menu.
    const again = cellMenu !== null && menuFor === menuKey(uuid, col.key, value);
    deselectCell();
    if (again) return;
    if (ch) selectChip(uuid, col.key, value ?? "", ch);
    else if (!target.closest(".lt-expanded")) selectCell(uuid, col.key);
}

/** Whether the click under way is a tap on a touch screen (it ticks the row). */
let touchTap = false;
/** Clicks before this (ms) end a long press, whose menu is up. */
let quietUntil = 0;
let pressTimer = 0;
let pressFrom: { x: number; y: number } | null = null;
/** How long a press is long, in ms, and how far (px) a finger may move in it. */
const LONG_PRESS = 500;
const PRESS_SLOP = 10;
/** When a long press last opened a menu (Android's contextmenu then comes too). */
let pressedAt = 0;

body.addEventListener("pointerdown", (e) => {
    touchTap = e.pointerType === "touch" || e.pointerType === "pen";
    ctrlTicked = false;
    clearTimeout(pressTimer);
    const target = e.target as HTMLElement;
    if (!touchTap || editing?.area.contains(target) || target.closest(".lt-cell-menu, input, a, .lt-actions, .lt-chip-x, .lt-more, .lt-cell-edit")) return;
    pressFrom = { x: e.clientX, y: e.clientY };
    pressTimer = window.setTimeout(() => {
        pressFrom = null;
        pressedAt = Date.now();
        quietUntil = Infinity;
        menuAt(target);
    }, LONG_PRESS);
});
body.addEventListener("pointermove", (e) => {
    if (pressFrom && Math.hypot(e.clientX - pressFrom.x, e.clientY - pressFrom.y) > PRESS_SLOP) clearTimeout(pressTimer);
});
const pressEnded = (): void => {
    clearTimeout(pressTimer);
    pressFrom = null;
    // A long press's own click (if the browser sends one) comes right after.
    if (quietUntil === Infinity) quietUntil = Date.now() + 400;
};
body.addEventListener("pointerup", pressEnded);
body.addEventListener("pointercancel", pressEnded);

body.addEventListener("contextmenu", (e) => {
    const target = e.target as HTMLElement;
    // A link's, a box's and the menu's own: the browser's.
    if (cellMenu?.contains(target) || editing?.area.contains(target) || target.closest("input, a")) return;
    const uuid = target.closest<HTMLElement>(".lt-row")?.dataset["uuid"];
    if (ctrlDown && uuid) {
        ctrlDown = false;
        e.preventDefault();
        deselectCell();
        toggle(uuid, !selected.has(uuid), false);
        ctrlTicked = true;
        return;
    }
    if (!target.closest("[data-col]")) return;
    e.preventDefault();
    clearTimeout(pressTimer);
    // Android sends one at the end of a long press, which opened the menu already.
    if (Date.now() - pressedAt < 1500) return;
    menuAt(target);
});

// ---------------------------------------------------------------------------
// A right-clicked (or long-pressed) cell or chip: selected, with a menu
// (filter by it, hide it, edit, copy; a tag can also be split or removed)
// ---------------------------------------------------------------------------

let selectedCell: { uuid: string; col: ColKey } | null = null;
let cellMenu: HTMLElement | null = null;
/** What the open menu is for (menuKey). */
let menuFor = "";
/** A cell's menu, or (with value) a chip's. */
const menuKey = (uuid: string, col: ColKey, value?: string): string => JSON.stringify([uuid, col, value ?? null]);

function setFilter(key: ColKey, value: string): void {
    const input = filterInputs.get(key);
    if (!input) return;
    input.value = filters[key] = value;
    refresh();
}

function closeCellMenu(): void {
    cellMenu?.closest(".lt-row")?.classList.remove("lt-row-open");
    cellMenu?.remove();
    cellMenu = null;
    menuFor = "";
}

function deselectCell(): void {
    closeCellMenu();
    selectedCell = null;
    for (const c of body.querySelectorAll(".lt-cell-selected, .lt-chip-selected")) c.classList.remove("lt-cell-selected", "lt-chip-selected");
}

type MenuItem = [label: string, run: () => void];

/**
 * A menu under (or above) an item in a row, at left (px from the row's left);
 * with below, under (or above) that element instead of the row (a tag on a
 * lower line of the "+N" panel).
 */
function openMenu(row: HTMLElement, left: number, label: string, items: MenuItem[], key: string, below?: HTMLElement): void {
    if (!items.length) return;
    const buttons = items.map(([text, run]) => {
        const btn = el("button", { type: "button", className: "lt-menu-item", role: "menuitem", title: text }, text);
        btn.addEventListener("click", () => {
            deselectCell();
            run();
        });
        return btn;
    });
    const menu = el("div", { className: "lt-cell-menu", role: "menu", ariaLabel: label }, ...buttons);
    menu.style.left = `${left}px`;
    row.classList.add("lt-row-open");
    row.append(menu);
    cellMenu = menu;
    menuFor = key;
    const rowBox = row.getBoundingClientRect();
    const anchor = below?.getBoundingClientRect();
    if (anchor) menu.style.top = `${anchor.bottom - rowBox.top + 2}px`;
    // Up, if it would go past the bottom of the table.
    if (menu.getBoundingClientRect().bottom > scroller.getBoundingClientRect().bottom) {
        menu.classList.add("lt-menu-up");
        if (anchor) {
            menu.style.top = "auto";
            menu.style.bottom = `${rowBox.bottom - anchor.top + 2}px`;
        }
    }
}

const copyItem = (text: string): MenuItem => [
    "Copy",
    () => {
        navigator.clipboard?.writeText(text).then(
            () => say("Copied.", "ok"),
            () => say("Couldn't copy: the browser didn't allow it.", "error"),
        );
    },
];

/** The row drawn for a book, if it's on screen. */
const rowOnScreen = (uuid: string): HTMLElement | undefined => [...body.querySelectorAll<HTMLElement>(".lt-row")].find((x) => x.dataset["uuid"] === uuid);


/** A right-clicked tag, language or format (in the row or its "+N" panel): its own menu. */
function selectChip(uuid: string, key: ColKey, value: string, chipEl: HTMLElement): void {
    deselectCell();
    if (!chipEl.isConnected) {
        // Redrawn since the click: the same chip in the new row (or, if it's
        // one the "+N" hides, its cell).
        const cell = rowOnScreen(uuid)?.querySelector<HTMLElement>(`.lt-cell[data-col="${key}"]`);
        const again = [...(cell?.querySelectorAll<HTMLElement>(".lt-chip") ?? [])].find((c) => c.dataset["value"] === value);
        const found = again && !again.hidden ? again : cell;
        if (!found) return;
        chipEl = found;
    }
    const row = chipEl.closest<HTMLElement>(".lt-row");
    const r = rows.find((x) => x.b.uuid === uuid);
    const col = COLUMNS.find((c) => c.key === key);
    if (!row || !r || !col) return;
    selectedCell = { uuid, col: key };
    chipEl.classList.add("lt-chip-selected");
    const name = shortText(key === "languages" ? languageChipLabel(value) : value);
    const items: MenuItem[] = [];
    // | separates alternatives in a filter.
    if (!value.includes("|")) {
        items.push([`Only books with “${name}”`, () => setFilter(key, `=${value}`)]);
        items.push([`Hide books with “${name}”`, () => setFilter(key, `!=${value}`)]);
    }
    if (col.edit) items.push([`Edit “${name}”…`, () => void editAt(uuid, key, value)]);
    if (key === "tags") {
        items.push([`Split “${name}”…`, () => openSplitDialog({ uuid, tag: value })]);
        items.push(["Remove from this book", () => void removeTag(uuid, value)]);
    }
    items.push(copyItem(value));
    const left = chipEl.getBoundingClientRect().left - row.getBoundingClientRect().left;
    const inPanel = expanded?.contains(chipEl) ? chipEl : undefined;
    openMenu(row, left, `${col.label}: ${value}, of “${r.b.title}”`, items, menuKey(uuid, key, value), inPanel);
}

const shortText = (s: string): string => (s.length > 40 ? `${s.slice(0, 39)}…` : s);

function selectCell(uuid: string, key: ColKey): void {
    deselectCell();
    closeExpanded();
    const row = [...body.querySelectorAll<HTMLElement>(".lt-row")].find((x) => x.dataset["uuid"] === uuid);
    const cell = row?.querySelector<HTMLElement>(`.lt-cell[data-col="${key}"]`);
    const r = rows.find((x) => x.b.uuid === uuid);
    const col = COLUMNS.find((c) => c.key === key);
    if (!row || !cell || !r || !col) return;
    selectedCell = { uuid, col: key };
    cell.classList.add("lt-cell-selected");

    const b = r.b;
    const text = col.text(b);
    const blank = col.blank(b);
    const items: MenuItem[] = [];
    // A list's values have their own menus (click one); | separates alternatives in a filter.
    if (!col.list && !blank && !text.includes("|")) {
        // A series: every book in it, whatever its number.
        const value = col.key === "series" ? b.series : text;
        const exact = col.key === "series" ? value : `=${value}`;
        items.push([`Only “${shortText(value)}”`, () => setFilter(key, exact)]);
        items.push([`Hide “${shortText(value)}”`, () => setFilter(key, col.key === "series" ? `!${value}` : `!${exact}`)]);
    }
    if (blank) {
        items.push([
            `Only books with no ${col.label.toLowerCase()}`,
            () => {
                blankOnly.add(key);
                blankBoxes.get(key)!.checked = true;
                refresh();
            },
        ]);
    }
    if (col.edit) items.push([blank && !text ? "Add…" : "Edit…", () => void editAt(uuid, key)]);
    if (key === "authors") items.push(...authorItems(b));
    if (text) items.push(copyItem(text));
    openMenu(row, cell.offsetLeft, `${col.label} of “${b.title}”`, items, menuKey(uuid, key));
}

document.addEventListener("click", (e) => {
    if (selectedCell && !body.contains(e.target as Node)) deselectCell();
});

async function markNotDuplicates(uuids: string[]): Promise<void> {
    try {
        const set = await api<NotDuplicateSet>("POST", "/api/library/not-duplicates", { uuids });
        notDupSets.push(set);
        changedHere++;
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
        changedHere++;
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
    changedHere++;
    selected.clear();
    say(`Removed ${plural(gone.size, "book")}. They're in the Recycling Bin.`, "ok");
    refresh();
});

// ---------------------------------------------------------------------------
// Edits: in a cell, or to every selected book (the dialog)
// ---------------------------------------------------------------------------

/**
 * Send an edit; the changed rows come back. The library's edits are queued
 * by the server, which answers before writing them, with the rows as they
 * will be (Go: bulk_queue.go): followJob watches them get written.
 */
async function sendEdit(uuids: string[], edit: BulkEdit): Promise<BulkResult> {
    const res = await api<BulkResult>("POST", SOURCE.bulk, { uuids, edit });
    swapRows(res.books);
    // The changed books are ticked, so one the change takes out of the
    // filters stays in sight (pinned above the list) instead of vanishing.
    for (const b of res.books) selected.add(b.uuid);
    if (res.job) followJob(res.job, uuids);
    pollChecking();
    return res;
}

/** Put these rows in place of the books' own, in place: a change needn't move or hide its row. */
function swapRows(books: TableBook[]): void {
    if (!books.length) return;
    const fresh = new Map(books.map((b) => [b.uuid, b]));
    const made = new Map<string, Row>();
    const swap = (r: Row): Row => {
        const b = fresh.get(r.b.uuid);
        if (!b) return r;
        let x = made.get(b.uuid);
        if (!x) {
            x = makeRow(b);
            x.dupKey = r.dupKey;
            x.group = r.group;
            made.set(b.uuid, x);
        }
        return x;
    };
    rows = rows.map(swap);
    shown = shown.map(swap);
    changedHere++;
}

// ---------------------------------------------------------------------------
// Queued edits: written by the server after it answered
// ---------------------------------------------------------------------------

const savingEl = byId("lt-saving");
/** Edits sent from this page the server hasn't written yet: id → their books. */
const queuedJobs = new Map<number, Set<string>>();
let jobTimer = 0;

function followJob(id: number, uuids: string[]): void {
    queuedJobs.set(id, new Set(uuids));
    showSaving(null);
    if (!jobTimer) jobTimer = window.setTimeout(pollJobs, 700);
}

/** "Saving…" while edits wait to be written; with how far the one being written has got, when it's long. */
function showSaving(jobs: Record<string, BulkJob> | null): void {
    savingEl.hidden = queuedJobs.size === 0;
    if (savingEl.hidden) return;
    const running = jobs ? Object.values(jobs).find((j) => j.state === "running" && j.total > 20) : undefined;
    const what = queuedJobs.size === 1 ? "Saving…" : `Saving ${n(queuedJobs.size)} changes…`;
    savingEl.textContent = running ? `${what} ${n(running.done)} of ${plural(running.total, "book")}` : what;
    savingEl.title = "The server has these changes and is writing them to the library.";
}

async function pollJobs(): Promise<void> {
    jobTimer = 0;
    if (queuedJobs.size === 0) return;
    let wait = 1000;
    try {
        const ids = [...queuedJobs.keys()].slice(0, 200);
        const data = await api<{ jobs: Record<string, BulkJob>; waiting: number }>("GET", `/api/library/bulk/jobs?ids=${ids.join(",")}`);
        const failed: string[] = [];
        let gone = false;
        for (const id of ids) {
            const job = data.jobs[String(id)];
            if (!job || job.state === "queued" || job.state === "running") continue;
            queuedJobs.delete(id);
            if (job.state === "unknown") {
                gone = true; // forgotten (the server restarted): the table has it as it is
                continue;
            }
            const res = job.result;
            if (!res) continue;
            failed.push(...res.failed.map((f) => f.error));
            for (const b of res.books) written.set(b.uuid, b);
        }
        if (failed.length) {
            say(`Couldn't save ${plural(failed.length, "book")}: ${failed.slice(0, 2).join("; ")}`, "error");
            gone = true; // show the books as they are
            void loadFailures();
        }
        showSaving(data.jobs);
        if (gone) {
            reloadedAt = 0;
            void reloadRows();
        }
    } catch {
        wait = 5000; // the server is away (restarting?): its queue is kept
    }
    showWritten();
    if (queuedJobs.size || written.size) jobTimer = window.setTimeout(pollJobs, wait);
    else showSaving(null);
}

// Kept failures: books a queued edit couldn't be made to, from this page or
// any other, until someone dismisses them (Go: bulk_edit_failures).
const failuresNote = byId("lt-failures");
const failuresDialog = byId("failures-dialog", HTMLDialogElement);
const failuresList = byId("failures-list");
closeOnBackdropClick(failuresDialog);
let failures: BulkFailureKept[] = [];

async function loadFailures(): Promise<void> {
    if (QUEUE) return;
    try {
        const data = await api<{ failures: BulkFailureKept[]; total: number }>("GET", "/api/library/bulk/failures");
        failures = data.failures;
        failuresNote.hidden = data.total === 0;
        byId("lt-failures-text").textContent = `${plural(data.total, "edit")} not saved`;
    } catch {
        /* shown next time */
    }
}

/** What an edit was meant to do, in a few words. */
function describeEdit(e: BulkEdit): string {
    const q = (v: string): string => (v ? `“${v}”` : "none");
    const parts: string[] = [];
    if (e.title !== undefined) parts.push(`title ${q(e.title)}`);
    if (e.authors !== undefined) parts.push(`authors ${q(e.authors)}`);
    if (e.series !== undefined) parts.push(`series ${q(e.series)}`);
    if (e.series_index !== undefined) parts.push(`number ${q(e.series_index)}`);
    if (e.publisher !== undefined) parts.push(`publisher ${q(e.publisher)}`);
    if (e.date !== undefined) parts.push(`published ${q(e.date)}`);
    if (e.tags !== undefined) parts.push(`tags ${q(e.tags)}`);
    if (e.remove_tags?.length) parts.push(`remove tags ${e.remove_tags.map(q).join(", ")}`);
    if (e.add_tags?.length) parts.push(`add tags ${e.add_tags.map(q).join(", ")}`);
    if (e.split_tags) parts.push(`split tags on ${q(e.split_tags)}`);
    if (e.languages !== undefined) parts.push(`languages ${q(e.languages)}`);
    if (e.remove_languages?.length) parts.push(`remove languages ${e.remove_languages.join(", ")}`);
    if (e.add_languages?.length) parts.push(`add languages ${e.add_languages.join(", ")}`);
    return parts.join("; ") || "an edit";
}

byId("lt-failures-show").addEventListener("click", () => {
    failuresList.replaceChildren(
        ...failures.map((f) =>
            el(
                "li",
                {},
                el("div", { className: "lt-failure-title", textContent: f.title }),
                el("div", { textContent: `Meant to set: ${describeEdit(f.edit)}` }),
                el("div", { className: "lt-failure-error", textContent: f.error }),
                el("div", { className: "lt-failure-meta", textContent: `${f.by || "Someone"}, ${new Date(f.at * 1000).toLocaleString()}` }),
            ),
        ),
    );
    failuresDialog.showModal();
});

byId("failures-select").addEventListener("click", () => {
    const known = new Set(rows.map((r) => r.b.uuid));
    selected.clear();
    for (const f of failures) if (known.has(f.uuid)) selected.add(f.uuid);
    failuresDialog.close();
    refresh();
    say(`Selected ${plural(selected.size, "book")} whose edits weren't saved.`, "ok");
});

byId("failures-dismiss").addEventListener("click", async () => {
    try {
        await api("POST", "/api/library/bulk/failures/dismiss", { ids: failures.map((f) => f.id) });
    } catch (err) {
        say(`Couldn't dismiss them: ${errorMessage(err)}`, "error");
        return;
    }
    failuresDialog.close();
    await loadFailures();
});

/** Rows as the server wrote them, to show where they differ from what it foresaw. */
const written = new Map<string, TableBook>();

/** Show the written rows, unless that would close something open (they wait for the next poll). */
function showWritten(): void {
    if (!written.size || editing || modalOpen() || cellMenu || expanded) return;
    // Not over a book a later edit from here is still changing: the server foresaw that one's row.
    const later = new Set([...queuedJobs.values()].flatMap((u) => [...u]));
    const byUuid = new Map(rows.map((r) => [r.b.uuid, r.b]));
    const differ = [...written.values()].filter((b) => {
        const was = byUuid.get(b.uuid);
        return !later.has(b.uuid) && was && JSON.stringify(was) !== JSON.stringify(b);
    });
    written.clear();
    if (!differ.length) return;
    swapRows(differ);
    refresh();
}

// ---------------------------------------------------------------------------
// Tag changes that can be undone: a tag's ✕, and splitting tags
// ---------------------------------------------------------------------------

interface TagChange {
    uuid: string;
    before: string[];
    after: string[];
}

/**
 * After a tag change: filter and sort again, or (refilter false) only redraw,
 * so the rows stay where they are (and a removed tag's Undo in reach), with
 * the cell whose chips were open opened again.
 */
function settle(refilter: boolean): void {
    const open = expanded ? { uuid: expanded.closest<HTMLElement>(".lt-row")?.dataset["uuid"], col: expanded.dataset["col"] } : null;
    fillDatalists();
    if (refilter) refresh();
    else {
        draw(true);
        selectionChanged();
    }
    if (!open) return;
    const cell = [...body.querySelectorAll<HTMLElement>(".lt-row")]
        .find((row) => row.dataset["uuid"] === open.uuid)
        ?.querySelector<HTMLElement>(`.lt-list-cell[data-col="${open.col}"]`);
    if (cell) expand(cell);
}

/** Make a change to the books' tags; the books whose tags changed, or null if it failed. */
async function tagEdit(books: TableBook[], edit: BulkEdit, refilter: boolean): Promise<TagChange[] | null> {
    const before = new Map(books.map((b) => [b.uuid, b.tags]));
    try {
        const res = await sendEdit([...before.keys()], edit);
        const changes = res.books
            .filter((b) => before.has(b.uuid))
            .map((b) => ({ uuid: b.uuid, before: before.get(b.uuid)!, after: b.tags }))
            .filter((c) => c.before.join("\n") !== c.after.join("\n"));
        settle(refilter);
        if (res.failed.length) say(resultText(res), "error");
        return changes;
    } catch (err) {
        say(`Couldn't change the tags: ${errorMessage(err)}`, "error");
        settle(refilter);
        return null;
    }
}

/**
 * Put the books' tags back as they were: take away the tags each change
 * added and add back the ones it took away. Books needing the same edit
 * share a request.
 */
async function undoTags(changes: TagChange[], refilter: boolean): Promise<void> {
    const has = (list: string[], t: string): boolean => list.some((x) => sameTag(x, t));
    const edits = new Map<string, { edit: BulkEdit; uuids: string[] }>();
    for (const c of changes) {
        const edit: BulkEdit = {};
        const remove = c.after.filter((t) => !has(c.before, t));
        const add = c.before.filter((t) => !has(c.after, t));
        if (!remove.length && !add.length) continue;
        // Untouched since (as this page knows it): the tags as they were, in
        // their order. A tag with a comma can't go in that list.
        const now = rows.find((r) => r.b.uuid === c.uuid)?.b.tags;
        if (now && now.join("\n") === c.after.join("\n") && !c.before.some((t) => t.includes(","))) edit.tags = c.before.join(", ");
        else {
            if (remove.length) edit.remove_tags = remove;
            if (add.length) edit.add_tags = add;
        }
        const key = JSON.stringify(edit);
        const e = edits.get(key) ?? { edit, uuids: [] };
        e.uuids.push(c.uuid);
        edits.set(key, e);
    }
    say("Undoing…");
    let changed = 0;
    const failed: string[] = [];
    try {
        for (const { edit, uuids } of edits.values()) {
            const res = await sendEdit(uuids, edit);
            changed += res.changed;
            failed.push(...res.failed.map((f) => f.error));
        }
        say(failed.length ? `Undone for ${plural(changed, "book")}; ${n(failed.length)} failed: ${failed.slice(0, 2).join("; ")}` : `Undone: ${plural(changed, "book")} as before.`, failed.length ? "error" : "ok");
    } catch (err) {
        say(`Couldn't undo: ${errorMessage(err)}`, "error");
    }
    settle(refilter);
}

/** A tag's ✕: the tag goes from that book now; the chip stays, struck through, with Undo. */
async function removeTag(uuid: string, tag: string): Promise<void> {
    const r = rows.find((x) => x.b.uuid === uuid);
    if (!r) return;
    const parked = removedTags.get(uuid) ?? [];
    if (!parked.some((t) => sameTag(t, tag))) removedTags.set(uuid, [...parked, tag]);
    const changes = await tagEdit([r.b], { remove_tags: [tag] }, false);
    if (changes?.length) offerUndo(`Removed “${tag}” from “${r.b.title}”.`, () => undoTags(changes, false));
}

/** A removed tag's Undo. */
async function putTagBack(uuid: string, tag: string): Promise<void> {
    const r = rows.find((x) => x.b.uuid === uuid);
    if (!r) return;
    const changes = await tagEdit([r.b], { add_tags: [tag] }, false);
    if (changes) say(`Put “${tag}” back on “${r.b.title}”.`, "ok");
}

// ---------------------------------------------------------------------------
// Authors written another way ("Last, First", "A; B"): what the metadata
// editor's Split button does (tidyAuthors in authors.ts), with Undo
// ---------------------------------------------------------------------------

interface AuthorChange {
    uuid: string;
    from: string;
    to: string;
}

/** A book's authors as Split would write them, or null if they're fine as they are. */
function tidiedAuthors(b: TableBook): string | null {
    const tidy = joinAuthors(tidyAuthors(b.authors));
    return tidy && tidy !== joinAuthors(splitAuthors(b.authors)) ? tidy : null;
}

/** The Authors menu's items: tidy this book's authors, and the selected books' too. */
function authorItems(b: TableBook): MenuItem[] {
    const items: MenuItem[] = [];
    const tidy = tidiedAuthors(b);
    if (tidy) {
        const more = tidyAuthors(b.authors).length > splitAuthors(b.authors).length;
        items.push([`${more ? `Split into ${tidyAuthors(b.authors).length} authors` : "First name first"}: ${shortText(tidy)}`, () => void tidyBooksAuthors([b])]);
    }
    // Every selected book this one is among: the same for each.
    if (selected.has(b.uuid) && selected.size > 1) {
        const fixable = selectedBooks().filter((x) => tidiedAuthors(x) !== null);
        if (fixable.length) items.push([`Split / first name first: ${plural(fixable.length, "selected book")}`, () => void tidyBooksAuthors(fixable)]);
    }
    return items;
}

/** Set the books' authors (books getting the same authors share a request); null if it failed. */
async function setAuthors(changes: AuthorChange[], value: (c: AuthorChange) => string): Promise<{ changed: number; failed: string[] } | null> {
    const groups = new Map<string, string[]>();
    for (const c of changes) groups.set(value(c), [...(groups.get(value(c)) ?? []), c.uuid]);
    let changed = 0;
    const failed: string[] = [];
    try {
        for (const [authors, uuids] of groups) {
            const res = await sendEdit(uuids, { authors });
            changed += res.changed;
            failed.push(...res.failed.map((f) => f.error));
        }
    } catch (err) {
        say(`Couldn't change the authors: ${errorMessage(err)}`, "error");
        settle(false);
        return null;
    }
    settle(false);
    return { changed, failed };
}

async function tidyBooksAuthors(books: TableBook[]): Promise<void> {
    const changes = books.flatMap((b): AuthorChange[] => {
        const to = tidiedAuthors(b);
        return to ? [{ uuid: b.uuid, from: b.authors, to }] : [];
    });
    if (!changes.length) return;
    say(`Changing the authors of ${plural(changes.length, "book")}…`);
    const res = await setAuthors(changes, (c) => c.to);
    if (!res) return;
    if (res.failed.length) {
        say(`Changed ${plural(res.changed, "book")}; ${n(res.failed.length)} failed: ${res.failed.slice(0, 2).join("; ")}`, "error");
        return;
    }
    const what = changes.length === 1 ? `“${changes[0]!.from}” is now “${changes[0]!.to}”.` : `Changed the authors of ${plural(res.changed, "book")}.`;
    offerUndo(what, async () => {
        say("Undoing…");
        const back = await setAuthors(changes, (c) => c.from);
        if (back) say(back.failed.length ? `Undone for ${plural(back.changed, "book")}; ${n(back.failed.length)} failed.` : `Undone: ${plural(back.changed, "book")} as before.`, back.failed.length ? "error" : "ok");
    });
}

/** Split one of a book's tags on sep, with Undo. */
async function splitTag(uuid: string, tag: string, sep: string): Promise<void> {
    const r = rows.find((x) => x.b.uuid === uuid);
    if (!r) return;
    const parts = splitParts(tag, sep, r.b.tags.filter((t) => t !== tag));
    const edit: BulkEdit = { remove_tags: [tag] };
    if (parts.length) edit.add_tags = parts;
    const changes = await tagEdit([r.b], edit, false);
    if (changes?.length) offerUndo(`Split “${tag}” into ${plural(parts.length, "tag")}.`, () => undoTags(changes, false));
}

/** Split the selected books' tags on sep, with Undo. */
async function splitTags(sep: string): Promise<void> {
    const books = selectedBooks().filter((b) => b.tags.some((t) => t.includes(sep)));
    if (!books.length) {
        say(`No selected book has a tag with “${sep}” in it.`);
        return;
    }
    splitBtn.disabled = true;
    say(`Splitting the tags of ${plural(books.length, "book")}…`);
    const changes = await tagEdit(books, { split_tags: sep }, true);
    if (changes?.length) offerUndo(`Split the tags of ${plural(changes.length, "book")} on “${sep}”.`, () => undoTags(changes, true));
    else if (changes) say("No tags changed.");
    selectionChanged();
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
        case "series":
            return b.series ? `${b.series} [${b.series_index}]` : "";
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

/** "Dune [2]" (or "Dune #2") as a series and its number; "Dune" leaves the number alone. */
function seriesEdit(value: string): BulkEdit {
    const m = /^(.*?)\s*(?:\[\s*([^\]]*?)\s*\]|#\s*(\d+(?:\.\d+)?))$/.exec(value);
    if (!m) return { series: value };
    const index = m[2] ?? m[3] ?? "";
    return index ? { series: m[1]!, series_index: index } : { series: m[1]! };
}

/** The box open for an edit: in cell, taking up area (the cell, or one tag's place in it). */
let editing: { cell: HTMLElement; area: HTMLElement; input: HTMLInputElement; finish: (save: boolean) => Promise<void> } | null = null;

/**
 * Edit the book's cell for key, if its row is on screen; with value, just that
 * tag or language (in the "+N" panel if that hides it, or inPanel: it was
 * clicked there). A box open elsewhere is saved first.
 */
async function editAt(uuid: string, key: ColKey, value?: string, inPanel = false): Promise<void> {
    if (editing) {
        await editing.finish(true);
        if (editing) return;
    }
    const cell = rowOnScreen(uuid)?.querySelector<HTMLElement>(`.lt-editable[data-col="${key}"]`);
    if (!cell) return;
    const chipIn = (box: ParentNode | null): HTMLElement | undefined =>
        value === undefined ? undefined : [...(box?.querySelectorAll<HTMLElement>(".lt-chip:not(.lt-chip-removed)") ?? [])].find((c) => c.dataset["value"] === value);
    let ch = chipIn(cell);
    // One the "+N" hides, or one clicked in its panel (which covers the
    // row): edited in the panel that shows them all.
    if (ch && (ch.hidden || inPanel)) {
        if (!isExpanded(cell)) expand(cell);
        ch = chipIn(expanded);
    }
    if (ch && !ch.hidden && (key === "tags" || key === "languages")) startChipEdit(ch, uuid, key);
    else startEdit(cell, value);
}

/** Enter saves, Esc cancels, and leaving the box saves, as in a spreadsheet (a suggestion click comes back to it). */
function wireEditKeys(input: HTMLInputElement, finish: (save: boolean) => Promise<void>): void {
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
    input.addEventListener("blur", () => {
        setTimeout(() => {
            if (document.activeElement !== input) void finish(true);
        }, 150);
    });
}

/** Edit one tag or language in place of its chip: empty removes it. */
function startChipEdit(chipEl: HTMLElement, uuid: string, key: "tags" | "languages"): boolean {
    // In the row's cell, or in the "+N" panel.
    const cell = chipEl.closest<HTMLElement>(".lt-editable, .lt-expanded");
    const r = rows.find((x) => x.b.uuid === uuid);
    const before = chipEl.dataset["value"];
    if (!cell || !r || before === undefined || editing) return false;
    const what = key === "tags" ? "tag" : "language";
    const input = el("input", { type: "text", className: "lt-cell-input lt-chip-input", value: before, ariaLabel: `The ${what} ${before}` });
    input.title = `Edit this ${what} (empty removes it). Enter saves, Esc cancels.`;
    chipEl.replaceWith(input);
    if (key === "tags") attachSuggest(input, { category: "tag" });
    else attachSuggest(input, { category: "language", key: languageKey });
    input.parentElement?.classList.add("lt-chip-edit");
    cell.classList.add("lt-editing-chip");
    let done = false;
    const finish = async (save: boolean): Promise<void> => {
        if (done) return;
        done = true;
        editing = null;
        const value = input.value.trim();
        if (!save || value === before) {
            draw(true);
            return;
        }
        input.disabled = true;
        const edit: BulkEdit =
            key === "tags" ? { remove_tags: [before], ...(value ? { add_tags: [value] } : {}) } : { remove_languages: [before], ...(value ? { add_languages: [value] } : {}) };
        try {
            const res = await sendEdit([uuid], edit);
            const what = key === "tags" ? "Tag" : "Language";
            const text = value ? `${what} changed from ${quoted(before)} to ${quoted(value)} on “${shortText(r.b.title)}”.` : `${what} ${quoted(before)} removed from “${shortText(r.b.title)}”.`;
            savedWithUndo(res, r.b, key === "tags" ? "tags" : "languages", key === "tags" ? "Tags" : "Languages", text);
        } catch (err) {
            say(`Couldn't save: ${errorMessage(err)}`, "error");
        }
        refresh();
    };
    // Clicks elsewhere in the cell (another tag) are for the other tag.
    editing = { cell, area: input.parentElement ?? input, input, finish };
    input.focus();
    input.select();
    wireEditKeys(input, finish);
    return true;
}

/** A value in a message: quoted (cut short if long), or "nothing". */
const quoted = (v: string): string => (v.trim() ? `“${shortText(v.trim())}”` : "nothing");

/** What a cell edit changed, for the status line: Authors changed from “A” to “B” on “Title”. */
/** A column's field, in a message. */
const fieldName = (col: Column): string => (col.key === "pubdate" ? "Published date" : col.label);

function changeText(col: Column, before: string, after: string, title: string): string {
    const what = fieldName(col);
    const on = col.key === "title" ? "" : ` on “${shortText(title)}”`;
    if (!before.trim()) return `${what} set to ${quoted(after)}${on}.`;
    if (!after.trim()) return `${what} cleared${on} (was ${quoted(before)}).`;
    return `${what} changed from ${quoted(before)} to ${quoted(after)}${on}.`;
}

/**
 * After an edit to one book's field: say what changed (text), with Undo,
 * which puts the field back as it was in was (the book before the edit).
 */
function savedWithUndo(res: BulkResult, was: TableBook, field: EditField, what: string, text: string): void {
    const now = res.books[0];
    if (res.failed.length || !now) {
        say(res.failed.length ? resultText(res) : "Nothing changed.", res.failed.length ? "error" : "ok");
        return;
    }
    if (field === "tags") {
        const changes: TagChange[] = [{ uuid: was.uuid, before: was.tags, after: now.tags }];
        offerUndo(text, () => undoTags(changes, true));
        return;
    }
    offerUndo(text, async () => {
        const value = cellValue(was, field);
        say("Undoing…");
        try {
            const back = await sendEdit([was.uuid], field === "series" ? seriesEdit(value) : { [field]: value });
            say(back.failed.length ? resultText(back) : value.trim() ? `Undone: ${what} back to ${quoted(value)}.` : `Undone: ${what} empty again.`, back.failed.length ? "error" : "ok");
        } catch (err) {
            say(`Couldn't undo: ${errorMessage(err)}`, "error");
        }
        refresh();
    });
}

/** Edit a cell in place, with item (a tag or language) selected; false if it can't be edited (now). */
function startEdit(cell: HTMLElement, item?: string): boolean {
    const uuid = cell.closest<HTMLElement>(".lt-row")?.dataset["uuid"];
    const col = COLUMNS.find((c) => c.key === cell.dataset["col"]);
    const r = rows.find((x) => x.b.uuid === uuid);
    if (!uuid || !col?.edit || !r || editing) return false;
    const field = col.edit;
    const before = cellValue(r.b, field);
    const input = el("input", { type: field === "date" ? "date" : "text", className: "lt-cell-input", value: before, ariaLabel: col.label });
    // In the cell first: a suggestion list goes in a box around it.
    cell.replaceChildren(input);
    if (field === "tags") attachSuggest(input, { category: "tag", multi: true });
    if (field === "languages") attachSuggest(input, { category: "language", multi: true, key: languageKey });
    if (field === "authors") attachSuggest(input, { category: "author", multi: true, separators: "&" });
    if (field === "publisher") attachSuggest(input, { category: "publisher" });
    if (field === "series") {
        attachSuggest(input, { category: "series" });
        input.placeholder = "Series [number]";
        input.title = "The series, and the book's number in it in brackets: Dune [2]. Without a number the book keeps its own; empty: no series.";
    }
    cell.classList.add("lt-editing");
    input.focus();
    // That tag or language picked out ("a, b, c" is how cellValue joins them): typing replaces it.
    const items = field === "tags" ? r.b.tags : field === "languages" ? r.b.languages : [];
    const at = item === undefined ? -1 : items.indexOf(item);
    if (at >= 0) {
        const start = items.slice(0, at).reduce((t, v) => t + v.length + 2, 0);
        input.setSelectionRange(start, start + item!.length);
    } else input.select();

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
            const res = await sendEdit([uuid], field === "series" ? seriesEdit(value) : { [field]: value });
            const after = res.books[0] ? cellValue(res.books[0], field) : value;
            savedWithUndo(res, r.b, field, fieldName(col), changeText(col, before, after, r.b.title));
        } catch (err) {
            say(`Couldn't save: ${errorMessage(err)}`, "error");
        }
        refresh();
    };
    editing = { cell, area: cell, input, finish };
    wireEditKeys(input, finish);
    return true;
}

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
attachSuggest(field("series"), { category: "series" });
// Typing in a "set" box ticks it.
for (const name of ["tags", "languages", "authors", "series", "publisher", "date", "rating"]) {
    field(name).addEventListener("input", () => {
        field(`${name}_on`).checked = true;
        updateSummary();
    });
}
field("series_index").addEventListener("input", () => {
    field("series_on").checked = true;
    updateSummary();
});
form.addEventListener("input", updateSummary);
form.addEventListener("change", updateSummary);

/** Chips of the values the selected books have, with counts; a click marks one for removal. */
function presentChips(box: HTMLElement, values: Map<string, number>, marked: Set<string>, label: (v: string) => string): void {
    const sorted = [...values].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
    box.replaceChildren(
        ...(sorted.length
            ? sorted.map(([v, k]) => {
                  const mark = el("span", { className: "lt-chip-mark" });
                  const b = el("button", { type: "button", className: "lt-chip lt-chip-count" }, label(v), el("span", {}, String(k)), mark);
                  const show = (): void => {
                      const on = marked.has(v);
                      b.setAttribute("aria-pressed", String(on));
                      mark.textContent = on ? "Undo" : "✕";
                      b.title = on ? `Keep it on ${plural(k, "book")}` : `Remove from ${plural(k, "book")}`;
                  };
                  show();
                  b.addEventListener("click", () => {
                      if (marked.has(v)) marked.delete(v);
                      else marked.add(v);
                      show();
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
    if (field("series_on").checked) {
        edit.series = field("series").value;
        const index = field("series_index").value.trim();
        if (index && edit.series.trim()) edit.series_index = index;
    }
    if (field("publisher_on").checked) edit.publisher = field("publisher").value;
    if (field("date_on").checked) edit.date = field("date").value;
    if (field("rating_on").checked) edit.rating = field("rating").value;
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
    if (e.series !== undefined) {
        const name = e.series.trim();
        parts.push(!name ? "remove the series" : e.series_index ? `series “${name}”, number ${e.series_index}` : `series “${name}”`);
    }
    if (e.publisher !== undefined) parts.push(e.publisher.trim() ? "set the publisher" : "clear the publisher");
    if (e.date !== undefined) parts.push(e.date ? "set the date" : "clear the date");
    if (e.rating !== undefined) parts.push(e.rating ? `rate ${ratingLabel(e.rating)}` : "clear the rating");
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

{
    const spot = el("span");
    searchInput.replaceWith(spot);
    const wrap = clearable(searchInput, "Clear the search");
    wrap.classList.add("lt-search-wrap");
    spot.replaceWith(wrap);
}
searchInput.addEventListener("input", () => {
    search = searchInput.value;
    scheduleRefresh();
});
dupSelect.addEventListener("change", () => {
    dupMode = dupSelect.value;
    refresh();
});
function tagCheckChanged(): void {
    tagCheckSelect.value = tagCheck;
    tagLenWrap.hidden = tagCheck !== "long";
    tagLenInput.value = String(tagLen);
}
tagCheckSelect.addEventListener("change", () => {
    tagCheck = tagCheckSelect.value as TagCheck;
    tagCheckChanged();
    refresh();
});
tagLenInput.addEventListener("input", () => {
    const v = Math.floor(Number(tagLenInput.value));
    if (!Number.isFinite(v) || v < 1) return;
    tagLen = v;
    scheduleRefresh();
});
// Filtering by tags with ";": Split on ; at once. Otherwise: ask for the separator.
splitBtn.addEventListener("click", () => {
    if (tagCheck === "semicolon") void splitTags(SPLIT_SEP);
    else openSplitDialog();
});
const splitDialog = byId("split-dialog", HTMLDialogElement);
const splitForm = byId("split-form", HTMLFormElement);
const splitSepInput = byId("split-sep", HTMLInputElement);
const splitSummary = byId("split-summary");
const splitApply = byId("split-apply", HTMLButtonElement);
closeOnBackdropClick(splitDialog);
let lastSep = SPLIT_SEP;
/** Splitting one book's tag (its chip's menu), not the selected books' tags. */
let splitOne: { uuid: string; tag: string } | null = null;
/** What a tag is likely glued together with: the dialog's first guess for it. */
const LIKELY_SEPS = [";", " -- ", "/", "|", " - ", "&", ":", "."];

/** The tags a tag split on sep makes: trimmed, not empty, none twice or that the book has. */
function splitParts(tag: string, sep: string, has: string[]): string[] {
    const out: string[] = [];
    for (const p of tag.split(sep).map((x) => x.trim())) {
        if (p && ![...has, ...out].some((t) => sameTag(t, p))) out.push(p);
    }
    return out;
}

function splitPreview(): void {
    const sep = splitSepInput.value;
    if (splitOne) {
        const { uuid, tag } = splitOne;
        const has = rows.find((r) => r.b.uuid === uuid)?.b.tags.filter((t) => t !== tag) ?? [];
        const parts = sep ? splitParts(tag, sep, has) : [];
        splitSummary.textContent = !sep
            ? "Type the separator."
            : !tag.includes(sep)
              ? `“${tag}” has no “${sep}” in it.`
              : parts.length
                ? `Becomes ${parts.map((t) => `“${t}”`).join(", ")}.`
                : "The book has every part already: the tag just goes.";
        splitApply.disabled = !sep || !tag.includes(sep);
        return;
    }
    const books = sep ? selectedBooks().filter((b) => b.tags.some((t) => t.includes(sep))) : [];
    const tags = [...new Set(books.flatMap((b) => b.tags.filter((t) => t.includes(sep))))];
    splitSummary.textContent = !sep
        ? "Type the separator."
        : books.length
          ? `Splits ${plural(tags.length, "tag")} in ${plural(books.length, "book")}: ${tags.slice(0, 3).map((t) => `“${t}”`).join(", ")}${tags.length > 3 ? "…" : ""}`
          : `No selected book has a tag with “${sep}” in it.`;
    splitApply.disabled = books.length === 0;
}

function openSplitDialog(one: { uuid: string; tag: string } | null = null): void {
    splitOne = one;
    byId("split-title").textContent = one ? `Split “${shortText(one.tag)}”` : "Split tags";
    byId("split-hint").textContent = one
        ? "This book's tag becomes the tags it lists. Parts the book already has are left out. Undo puts it back."
        : "Each tag holding the separator becomes the tags it lists. Parts a book already has are left out. Undo puts the tags back.";
    splitSepInput.value = one ? (LIKELY_SEPS.find((x) => one.tag.includes(x)) ?? lastSep) : lastSep;
    splitPreview();
    splitDialog.showModal();
    splitSepInput.select();
}
splitSepInput.addEventListener("input", splitPreview);
byId("split-cancel").addEventListener("click", () => splitDialog.close());
splitForm.addEventListener("submit", (e) => {
    e.preventDefault();
    const sep = splitSepInput.value;
    if (!sep || splitApply.disabled) return;
    lastSep = sep;
    splitDialog.close();
    if (splitOne) void splitTag(splitOne.uuid, splitOne.tag, sep);
    else void splitTags(sep);
});
onlySelectedBox.addEventListener("change", () => {
    onlySelected = onlySelectedBox.checked;
    refresh();
});
byId("lt-clear").addEventListener("click", () => {
    for (const c of COLUMNS) {
        delete filters[c.key];
        filterInputs.get(c.key)!.value = "";
        blankBoxes.get(c.key)!.checked = false;
    }
    blankOnly.clear();
    search = searchInput.value = "";
    dupMode = dupSelect.value = "";
    tagCheck = "";
    tagLen = TAG_LEN_DEFAULT;
    tagCheckChanged();
    onlySelected = onlySelectedBox.checked = false;
    refresh();
});
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && selectedCell && !editing && !modalOpen()) {
        deselectCell();
        return;
    }
    if (e.key === "Escape" && expanded) {
        closeExpanded();
        return;
    }
    if (e.key === "Escape" && !modalOpen() && !editing && selected.size && !(e.target instanceof HTMLInputElement && e.target.value)) {
        selected.clear();
        selectionChanged();
    }
});

function saveView(): void {
    const q = new URLSearchParams();
    if (search) q.set("q", search);
    if (dupMode) q.set("dupes", dupMode);
    if (tagCheck) q.set("tagcheck", tagCheck);
    if (tagCheck === "long" && tagLen !== TAG_LEN_DEFAULT) q.set("taglen", String(tagLen));
    if (sortKey !== "title" || sortDesc) q.set("sort", `${sortDesc ? "-" : ""}${sortKey}`);
    for (const c of COLUMNS) if (filters[c.key]?.trim()) q.set(`f.${c.key}`, filters[c.key]!);
    for (const c of COLUMNS) if (blankOnly.has(c.key)) q.set(`blank.${c.key}`, "1");
    const s = q.toString();
    history.replaceState(history.state, "", s ? `?${s}` : location.pathname);
}

function loadView(): void {
    const q = new URLSearchParams(location.search);
    search = searchInput.value = q.get("q") ?? "";
    dupMode = q.get("dupes") ?? "";
    dupSelect.value = dupMode;
    const check = (q.get("tagcheck") ?? "") as TagCheck;
    tagCheck = TAG_CHECKS.includes(check) ? check : "";
    const len = Math.floor(Number(q.get("taglen") ?? TAG_LEN_DEFAULT));
    tagLen = Number.isFinite(len) && len >= 1 ? len : TAG_LEN_DEFAULT;
    tagCheckChanged();
    const sort = q.get("sort") ?? "";
    const key = sort.replace(/^-/, "") as ColKey;
    if (COLUMNS.some((c) => c.key === key)) [sortKey, sortDesc] = [key, sort.startsWith("-")];
    for (const c of COLUMNS) {
        const f = q.get(`f.${c.key}`);
        if (f) filters[c.key] = f;
        if (q.get(`blank.${c.key}`)) blankOnly.add(c.key);
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
    applyColWidths();
    loadView();
    buildHead();
    try {
        const [data] = await Promise.all([api<TableData>("GET", SOURCE.table), loadLanguageCodes()]);
        rows = data.books.map(makeRow);
        notDupSets = data.not_duplicates;
    } catch (err) {
        countEl.textContent = "";
        say(`Couldn't load ${SOURCE.name}: ${errorMessage(err)}`, "error");
        return;
    }
    loaded();
}

/** After the rows (re)load: selected books no longer in the library don't count. */
function loaded(): void {
    showUnlisted();
    void loadFailures();
    const known = new Set(rows.map((r) => r.b.uuid));
    for (const u of selected) if (!known.has(u)) selected.delete(u);
    fillDatalists();
    refresh();
    pollChecking();
}

// The queue: books edited (or just queued) are checked against the library
// again in the background; the rows are fetched again until none is waiting
// for that.
const CHECK_POLL = 3000;
let checkTimer = 0;
function pollChecking(): void {
    if (!QUEUE || checkTimer) return;
    checkTimer = window.setTimeout(() => {
        checkTimer = 0;
        if (!rows.some((r) => r.b.library === "checking")) return;
        reloadedAt = 0;
        void reloadRows().finally(() => {
            // Not fetched (something open, or the tab hidden): try again later.
            if (rows.some((r) => r.b.library === "checking")) pollChecking();
        });
    }, CHECK_POLL);
}

// Merges and edits done in other tabs (a Merge link opened in a new tab,
// say): the rows again when this tab is looked at again, unless something
// is being edited here.
let reloading = false;
let reloadedAt = 0;
/**
 * Counts the changes made on this page (marks, edits, removals). A reload
 * that was under way when one landed may have been read before it was saved:
 * applying it would undo the change on screen, so it's dropped and fetched again.
 */
let changedHere = 0;
async function reloadRows(): Promise<void> {
    // Switching tabs fires both events: one fetch.
    if (document.visibilityState !== "visible" || reloading || editing || modalOpen() || rows.length === 0) return;
    if (Date.now() - reloadedAt < 2000) return;
    reloading = true;
    reloadedAt = Date.now();
    const gen = changedHere;
    let stale = false;
    try {
        const data = await api<TableData>("GET", SOURCE.table);
        if (editing || modalOpen() || cellMenu || expanded) {
            // Redrawing would close what's open: next time.
            reloadedAt = 0;
            return;
        }
        if (gen !== changedHere) {
            stale = true;
            return;
        }
        const before = rows.length;
        rows = data.books.map(makeRow);
        notDupSets = data.not_duplicates;
        loaded();
        if (rows.length < before) say(`${plural(before - rows.length, "book")} fewer since this page loaded (${QUEUE ? "added or discarded" : "merged or removed"} elsewhere).`, "ok");
    } catch {
        /* keep what's shown */
    } finally {
        reloading = false;
        if (stale) {
            reloadedAt = 0;
            void reloadRows();
        }
    }
}
document.addEventListener("visibilitychange", () => void reloadRows());

// Books whose files metadata.db doesn't list (dashed formats): a note in
// the toolbar, to show them and to add their rows (Go: addUnlistedFormats).
const unlistedNote = byId("lt-unlisted");
const unlistedText = byId("lt-unlisted-text");
function showUnlisted(): void {
    const k = rows.reduce((t, r) => t + (r.b.formats_from_folder ? 1 : 0), 0);
    unlistedNote.hidden = k === 0;
    unlistedText.textContent = `${plural(k, "book")} with files metadata.db doesn't list`;
}
byId("lt-unlisted-show").addEventListener("click", () => {
    const input = filterInputs.get("formats")!;
    input.value = filters.formats = FOLDER_ONLY;
    refresh();
});
byId("lt-unlisted-fix").addEventListener("click", async (e) => {
    const btn = e.currentTarget as HTMLButtonElement;
    const uuids = rows.filter((r) => r.b.formats_from_folder).map((r) => r.b.uuid);
    if (!uuids.length) return;
    if (!confirm(`Add the files of ${plural(uuids.length, "book")} to metadata.db, so Calibre lists them too?\n\nEach format gets a row naming the file that's in the book's folder. Only the database is modified.`)) return;
    btn.disabled = true;
    try {
        const res = await api<FormatRepair>("POST", "/api/library/repair-formats", { uuids });
        const data = await api<TableData>("GET", SOURCE.table);
        rows = data.books.map(makeRow);
        notDupSets = data.not_duplicates;
        changedHere++;
        loaded();
        const skipped = res.skipped.length ? ` ${plural(res.skipped.length, "format")} left alone: ${res.skipped.slice(0, 2).map((s) => s.error).join("; ")}` : "";
        say(`Added ${plural(res.formats, "format")} to metadata.db for ${plural(res.books, "book")}.${skipped}`, res.skipped.length ? "error" : "ok");
    } catch (err) {
        say(`Couldn't add them: ${errorMessage(err)}`, "error");
    } finally {
        btn.disabled = false;
    }
});
window.addEventListener("focus", () => void reloadRows());

void start();
