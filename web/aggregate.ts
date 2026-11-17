/**
 * The aggregate pages (/authors, /tags, …, templates/aggregate.html).
 *
 * - The search box finds every entry, single-book ones too, from
 *   /api/<plural>. Matching is fuzzy (see fuzzy.ts): names that start with
 *   the query come first, then those containing it, then typo and initials
 *   matches; ties go to the entry with more books. An empty box shows the
 *   page as it was.
 * - Discover shows a random few (the server picks the first ones); "Show
 *   others" draws another few from the same list the search box uses.
 * - The cloud can be ordered by book count (as served), A–Z (with a letter
 *   bar to jump by), or shuffled. The choice is remembered.
 * - Hovering an entry (or focusing it from the keyboard) previews a few of
 *   its books' covers.
 */

import { byId, el } from "./dom.js";
import { bestScore, fold, type Hay, prepare, prepareQuery } from "./fuzzy.js";
import type { AggregateEntry, Book } from "./types.js";
import { colorFromBook, coverUrl } from "./utils.js";

const MAX_RESULTS = 300;
const DISCOVER_COUNT = 12;
const ORDER_KEY = "agg-order";

interface Entry {
    item: AggregateEntry;
    hay: Hay[]; // name, display name and members
}

const page = byId("aggregate-page");
const input = byId("agg-search", HTMLInputElement);
const results = byId("agg-results");
const browse = byId("agg-browse");
const grid = byId("aggregate-grid");
const src = page.dataset["src"] ?? "";
const prefix = page.dataset["prefix"] ?? "/";
/** Minimum counts of tiers 2–5 (Go: aggregateTiers). */
const tiers = (page.dataset["tiers"] ?? "").split(",").map(Number).filter((n) => n > 0);

let entries: Entry[] | null = null;
let loading: Promise<Entry[]> | null = null;

function load(): Promise<Entry[]> {
    loading ??= fetch(src, { credentials: "same-origin" })
        .then((res): Promise<AggregateEntry[]> => {
            if (!res.ok) throw new Error(`HTTP ${res.status}`);
            return res.json() as Promise<AggregateEntry[]>;
        })
        .then((items) => {
            entries = items.map((item) => ({
                item,
                hay: [item.name, item.display ?? "", ...(item.members ?? [])].filter(Boolean).map(prepare),
            }));
            return entries;
        })
        .catch((err: unknown): Entry[] => {
            console.error("aggregate search:", err);
            loading = null; // try again next time
            return [];
        });
    return loading;
}

function tierOf(count: number): number {
    return 1 + tiers.filter((t) => count >= t).length;
}

function plural(n: number): string {
    return `${n.toLocaleString("en-US")} ${n === 1 ? "book" : "books"}`;
}

/** One entry, as the "agg-entry" template draws it. */
function link(item: AggregateEntry, className: string): HTMLAnchorElement {
    const a = el("a", { className, href: prefix + encodeURIComponent(item.name) });
    a.dataset["count"] = String(item.count);
    a.title = item.members?.length ? `Group: ${item.members.join(", ")} · ${plural(item.count)}` : plural(item.count);
    if (item.members?.length) a.classList.add("agg-group");
    a.append(item.display || item.name);
    if (item.display && item.display !== item.name) a.append(" ", el("span", { className: "agg-sub" }, item.name));
    a.append(el("span", { className: "agg-count" }, String(item.count)));
    return a;
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

/** Entries matching q, best match first, then by book count. */
function search(all: Entry[], q: string): AggregateEntry[] {
    const query = prepareQuery(q);
    const found: { item: AggregateEntry; score: number }[] = [];
    for (const e of all) {
        const score = bestScore(query, e.hay);
        if (score) found.push({ item: e.item, score });
    }
    found.sort((a, b) => b.score - a.score || b.item.count - a.item.count);
    return found.map((f) => f.item);
}

function showResults(searching: boolean): void {
    results.hidden = !searching;
    browse.hidden = searching;
}

async function update(): Promise<void> {
    const q = input.value.trim();
    if (!q) {
        showResults(false);
        results.replaceChildren();
        return;
    }
    const all = entries ?? (await load());
    if (input.value.trim() !== q) return; // typed on meanwhile
    const found = search(all, q);
    const shown = found.slice(0, MAX_RESULTS);
    results.replaceChildren(...shown.map((item) => link(item, `agg-item agg-t${tierOf(item.count)}`)));
    if (found.length > shown.length) {
        results.append(el("p", { className: "agg-more" }, `${found.length - shown.length} more: keep typing to narrow it down.`));
    } else if (found.length === 0) {
        results.append(el("p", { className: "status-message" }, entries ? "Nothing found." : "Couldn't load the list."));
    }
    showResults(true);
}

input.addEventListener("input", () => void update());
input.addEventListener("focus", () => void load(), { once: true });
input.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && input.value) {
        input.value = "";
        void update();
    }
});
// The browser may restore the box's text on back/forward.
if (input.value) void update();

// ---------------------------------------------------------------------------
// Discover
// ---------------------------------------------------------------------------

const reshuffle = document.getElementById("agg-reshuffle");
const discoverList = document.getElementById("agg-discover-list");

/** n distinct random picks from all, none of them in skip. */
function sample<T>(all: T[], n: number, skip: (t: T) => boolean): T[] {
    const pool = all.filter((t) => !skip(t));
    const source = pool.length >= n ? pool : all; // a small list: repeats are fine
    const out: T[] = [];
    const copy = source.slice();
    for (let i = 0; i < n && copy.length; i++) {
        const j = Math.floor(Math.random() * copy.length);
        out.push(copy[j] as T);
        copy[j] = copy[copy.length - 1] as T;
        copy.pop();
    }
    return out;
}

if (reshuffle && discoverList) {
    reshuffle.hidden = false;
    reshuffle.addEventListener("click", () => {
        void load().then((all) => {
            if (!all.length) return;
            const showing = new Set([...discoverList.querySelectorAll("a")].map((a) => a.getAttribute("href")));
            const picks = sample(all, DISCOVER_COUNT, (e) => showing.has(prefix + encodeURIComponent(e.item.name)));
            discoverList.replaceChildren(...picks.map((e) => link(e.item, "agg-chip")));
        });
    });
}

// ---------------------------------------------------------------------------
// Ordering the cloud
// ---------------------------------------------------------------------------

type Order = "count" | "az" | "shuffle";

const orderBar = byId("agg-order");
const letters = byId("agg-letters");
const items = [...grid.querySelectorAll<HTMLAnchorElement>("a.agg-item")];

/** The A–Z key: an author's "Surname, First" when the server sent one, else the name shown. */
function sortKey(a: HTMLAnchorElement): string {
    return fold(a.dataset["sort"] ?? a.firstChild?.textContent ?? "").trim();
}

const keys = new Map(items.map((a) => [a, sortKey(a)]));

function letterOf(key: string): string {
    const c = key.charAt(0);
    return /\p{L}/u.test(c) ? c.toUpperCase() : "#";
}

function setOrder(order: Order): void {
    let sorted: HTMLAnchorElement[];
    if (order === "az") {
        const collator = new Intl.Collator(undefined, { numeric: true });
        sorted = items.slice().sort((a, b) => collator.compare(keys.get(a) ?? "", keys.get(b) ?? ""));
    } else if (order === "shuffle") {
        sorted = sample(items, items.length, () => false);
    } else {
        sorted = items; // as served: most books first
    }

    const nodes: Node[] = [];
    const jump: HTMLElement[] = [];
    let last = "";
    for (const a of sorted) {
        if (order === "az") {
            const letter = letterOf(keys.get(a) ?? "");
            if (letter !== last) {
                last = letter;
                const id = `agg-letter-${letter === "#" ? "num" : letter}`;
                nodes.push(el("span", { className: "agg-letter", id }, letter));
                jump.push(el("a", { href: `#${id}` }, letter));
            }
        }
        nodes.push(a);
    }
    grid.replaceChildren(...nodes);
    letters.replaceChildren(...jump);
    letters.hidden = order !== "az" || jump.length < 2;
    for (const b of orderBar.querySelectorAll<HTMLButtonElement>("button")) {
        b.setAttribute("aria-pressed", String(b.dataset["order"] === order));
    }
    try {
        localStorage.setItem(ORDER_KEY, order);
    } catch {
        // private window or blocked storage: the order just isn't remembered
    }
}

// A dozen or more: fewer read at a glance in any order.
if (items.length >= 12) {
    orderBar.hidden = false;
    orderBar.addEventListener("click", (e) => {
        const b = (e.target as Element).closest<HTMLButtonElement>("button[data-order]");
        if (b) setOrder(b.dataset["order"] as Order);
    });
    let saved: string | null = null;
    try {
        saved = localStorage.getItem(ORDER_KEY);
    } catch {
        // as above
    }
    if (saved === "az" || saved === "shuffle") setOrder(saved);
}

// ---------------------------------------------------------------------------
// Preview: a few covers of the entry under the pointer
// ---------------------------------------------------------------------------

const PREVIEW_COVERS = 5;
const HOVER_DELAY = 350;

const preview = byId("agg-preview");
const previews = new Map<string, Promise<{ books: Book[] } | null>>();
let previewFor: HTMLAnchorElement | null = null;
let hoverTimer = 0;

function fetchPreview(href: string): Promise<{ books: Book[] } | null> {
    let p = previews.get(href);
    if (!p) {
        p = fetch(`${href}?api=1&page=1`, { credentials: "same-origin" })
            .then((res) => (res.ok ? (res.json() as Promise<Book[]>) : null))
            .then((books) => (books ? { books: books.slice(0, PREVIEW_COVERS) } : null))
            .catch(() => null);
        previews.set(href, p);
    }
    return p;
}

function cover(book: Book): HTMLElement {
    const tile = el("span", { className: "agg-preview-cover", title: book.title });
    tile.style.background = colorFromBook(book);
    const fallback = (): void => {
        tile.replaceChildren(el("span", { className: "agg-preview-title" }, book.title));
    };
    const src = book.placeholder ? book.placeholder.cover : coverUrl(book);
    if (!src) {
        fallback();
        return tile;
    }
    const img = el("img", { src, alt: book.title, loading: "eager", decoding: "async" });
    img.addEventListener("error", fallback, { once: true });
    tile.append(img);
    return tile;
}

function place(a: HTMLAnchorElement): void {
    const r = a.getBoundingClientRect();
    const w = preview.offsetWidth;
    const h = preview.offsetHeight;
    const left = Math.max(8, Math.min(r.left + r.width / 2 - w / 2, window.innerWidth - w - 8));
    const below = r.bottom + 8;
    const top = below + h > window.innerHeight - 8 && r.top - h - 8 > 0 ? r.top - h - 8 : below;
    preview.style.left = `${left}px`;
    preview.style.top = `${top}px`;
}

async function showPreview(a: HTMLAnchorElement): Promise<void> {
    previewFor = a;
    const href = a.getAttribute("href") ?? "";
    const data = await fetchPreview(href);
    if (previewFor !== a || !data?.books.length) return;
    const count = Number(a.dataset["count"] ?? data.books.length);
    const name = a.firstChild?.textContent ?? "";
    preview.replaceChildren(
        el("div", { className: "agg-preview-head" }, el("strong", {}, name), ` · ${plural(count)}`),
        el("div", { className: "agg-preview-covers" }, ...data.books.map(cover)),
    );
    if (count > data.books.length) {
        preview.append(el("div", { className: "agg-preview-more" }, `and ${(count - data.books.length).toLocaleString("en-US")} more`));
    }
    preview.hidden = false;
    place(a);
}

function hidePreview(): void {
    clearTimeout(hoverTimer);
    previewFor = null;
    preview.hidden = true;
}

const ENTRY = "a.agg-item, a.agg-chip";

if (matchMedia("(hover: hover) and (pointer: fine)").matches) {
    page.addEventListener("pointerover", (e) => {
        const a = (e.target as Element).closest<HTMLAnchorElement>(ENTRY);
        if (!a || a === previewFor) return;
        hidePreview();
        previewFor = a;
        hoverTimer = window.setTimeout(() => void showPreview(a), HOVER_DELAY);
    });
    page.addEventListener("pointerout", (e) => {
        const a = (e.target as Element).closest(ENTRY);
        if (a && !a.contains(e.relatedTarget as Node | null)) hidePreview();
    });
}
// From the keyboard: preview what has focus.
page.addEventListener("focusin", (e) => {
    const a = (e.target as Element).closest<HTMLAnchorElement>(ENTRY);
    if (a?.matches(":focus-visible")) void showPreview(a);
});
page.addEventListener("focusout", hidePreview);
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") hidePreview();
});
window.addEventListener("scroll", hidePreview, { passive: true });
