/**
 * The aggregate pages (/authors, /tags, …, templates/aggregate.html).
 *
 * - The search box finds every entry, single-book ones too, from
 *   /api/<plural>. Matching is fuzzy (see fuzzy.ts): names that start with
 *   the query come first, then those containing it, then typo and initials
 *   matches; ties go to the entry with more books. An empty box (Escape,
 *   or its ✕: clearable.ts) shows the page as it was.
 * - Discover shows a random few (the server picks the first ones); "Show
 *   more" draws another few from the same list the search box uses.
 * - The cloud can be ordered as served (by book count; series complete
 *   ones first), A–Z (with a letter bar to jump by), or shuffled. The
 *   choice is remembered. The letter bar marks the section in view; on a
 *   touch screen, dragging along it picks a letter (shown in a bubble above
 *   the finger) and lifting the finger jumps there.
 * - Hovering an entry (or focusing it from the keyboard) previews a few of
 *   its books' covers.
 * - "Your favorites" (signed in) lists the user's favorites of this kind,
 *   from the list favorites.ts keeps, so the page stays the same for all:
 *   authors as a carousel, as on /me/favorites, other kinds as chips.
 */

import { clearable } from "./clearable.js";
import { byId, el, random } from "./dom.js";
import { booksLabel, favCard, setupFavRows } from "./fav-carousel.js";
import { keptFavorites, loadFavorites } from "./favorites.js";
import { bestScore, fold, type Hay, prepare, prepareQuery } from "./fuzzy.js";
import type { AggFavorite, AggregateEntry, Book } from "./types.js";
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

/** Series the server drew coverage for (✓ or a ring): their badge, by name. */
const coverageBadges = new Map<string, { badge: Element; complete: boolean }>();
for (const a of document.querySelectorAll<HTMLAnchorElement>("a[data-coverage]")) {
    const badge = a.querySelector(".agg-coverage");
    if (!badge || !a.pathname.startsWith(prefix)) continue;
    try {
        coverageBadges.set(decodeURIComponent(a.pathname.slice(prefix.length)), { badge, complete: "complete" in a.dataset });
    } catch {
        // a malformed path: no badge in search results
    }
}

/** One entry, as the "agg-entry" template draws it. */
function link(item: AggregateEntry, className: string): HTMLAnchorElement {
    const a = el("a", { className, href: prefix + encodeURIComponent(item.name) });
    a.dataset["count"] = String(item.count);
    if (item.members?.length) a.classList.add("agg-group");
    a.append(item.display || item.name);
    if (item.display && item.display !== item.name) a.append(" ", el("span", { className: "agg-sub" }, item.name));
    const coverage = coverageBadges.get(item.name);
    if (coverage) {
        a.dataset["coverage"] = "";
        if (coverage.complete) a.dataset["complete"] = "";
        a.append(coverage.badge.cloneNode(true));
    }
    a.append(el("span", { className: "agg-count" }, String(item.count)));
    return a;
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

/**
 * The best max entries matching q (best match first, then by book count),
 * and how many match in all. Only the shown ones are sorted: a short query
 * can match most of a big library.
 */
function search(all: Entry[], q: string, max: number): { shown: AggregateEntry[]; total: number } {
    const query = prepareQuery(q);
    const scores = new Float64Array(all.length);
    let total = 0;
    for (let i = 0; i < all.length; i++) {
        const e = all[i] as Entry;
        const s = bestScore(query, e.hay);
        if (s) {
            scores[i] = s;
            total++;
        }
    }
    const better = (i: number, j: number): number =>
        (scores[j] as number) - (scores[i] as number) || (all[j] as Entry).item.count - (all[i] as Entry).item.count;
    let picked: number[] = [];
    for (let i = 0; i < all.length; i++) {
        if (!scores[i]) continue;
        picked.push(i);
        // Keep at most 2×max, trimming to the best max when full: linear overall.
        if (picked.length >= 2 * max) picked = picked.sort(better).slice(0, max);
    }
    picked.sort(better);
    return { shown: picked.slice(0, max).map((i) => (all[i] as Entry).item), total };
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
    const { shown, total } = search(all, q, MAX_RESULTS);
    results.replaceChildren(...shown.map((item) => link(item, `agg-item agg-t${tierOf(item.count)}`)));
    if (total > shown.length) {
        results.append(el("p", { className: "agg-more" }, `${total - shown.length} more: keep typing to narrow it down.`));
    } else if (total === 0) {
        results.append(el("p", { className: "status-message" }, entries ? "Nothing found." : "Couldn't load the list."));
    }
    showResults(true);
}

// Searching a big list takes a while on a slow phone: wait for a pause in typing rather than
// searching for every letter, and never run two at once.
const TYPING_PAUSE = 120;
let updateTimer = 0;
let running: Promise<void> | null = null;
let again = false;

function queueUpdate(): void {
    clearTimeout(updateTimer);
    if (!input.value.trim()) {
        void update(); // clearing the box: at once
        return;
    }
    updateTimer = window.setTimeout(runUpdate, entries ? TYPING_PAUSE : 0);
}

function runUpdate(): void {
    if (running) {
        again = true;
        return;
    }
    running = update().finally(() => {
        running = null;
        if (again) {
            again = false;
            runUpdate();
        }
    });
}

// Its ✕ empties it with an "input" event: the page comes back at once.
clearable(input, "Clear the search");
input.addEventListener("input", queueUpdate);
input.addEventListener("focus", () => void load(), { once: true });
input.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && input.value) {
        input.value = "";
        queueUpdate();
    }
});
// The browser may restore the box's text on back/forward.
if (input.value) runUpdate();

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
        const j = Math.floor(random() * copy.length);
        out.push(copy[j] as T);
        copy[j] = copy[copy.length - 1] as T;
        copy.pop();
    }
    return out;
}

if (reshuffle && discoverList) {
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

// Folded on first use: most visits never sort A–Z.
let keys: Map<HTMLAnchorElement, string> | null = null;

function letterOf(key: string): string {
    const c = key.charAt(0);
    return /\p{L}/u.test(c) ? c.toUpperCase() : "#";
}

function setOrder(order: Order): void {
    let sorted: HTMLAnchorElement[];
    if (order === "az") {
        keys ??= new Map(items.map((a) => [a, sortKey(a)]));
        const collator = new Intl.Collator(undefined, { numeric: true });
        sorted = items.slice().sort((a, b) => collator.compare(keys?.get(a) ?? "", keys?.get(b) ?? ""));
    } else if (order === "shuffle") {
        sorted = sample(items, items.length, () => false);
    } else {
        sorted = items; // as served: most books first (series: complete ones first, sortByCoverage)
    }

    const nodes: Node[] = [];
    const jump: HTMLElement[] = [];
    let last = "";
    for (const a of sorted) {
        if (order === "az") {
            const letter = letterOf(keys?.get(a) ?? "");
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
    queueSpy();
    for (const b of orderBar.querySelectorAll<HTMLButtonElement>("button")) {
        b.setAttribute("aria-pressed", String(b.dataset["order"] === order));
    }
    try {
        localStorage.setItem(ORDER_KEY, order);
    } catch {
        // private window or blocked storage: the order just isn't remembered
    }
}

// ---------------------------------------------------------------------------
// The letter bar: marks the section in view, and on touch is a scrubber
// ---------------------------------------------------------------------------

// Headings and their links are built in the same order: index i of one is index i of the other.
const heads = (): HTMLElement[] => [...grid.querySelectorAll<HTMLElement>(".agg-letter")];
const jumps = (): HTMLAnchorElement[] => [...letters.querySelectorAll<HTMLAnchorElement>("a")];

/** Where the bar's bottom edge sits once it's stuck under the top bar (it may wrap to two rows). */
function barBottom(): number {
    return (parseFloat(getComputedStyle(letters).top) || 0) + letters.offsetHeight;
}

function markCurrent(i: number): void {
    jumps().forEach((a, j) => {
        if (i === j) a.setAttribute("aria-current", "true");
        else a.removeAttribute("aria-current");
    });
}

/** Puts section i's heading just below the stuck bar. */
function jumpTo(i: number): void {
    const h = heads()[i];
    if (h) window.scrollTo({ top: window.scrollY + h.getBoundingClientRect().top - barBottom() - 4 });
}

let scrub: { pointer: number; index: number } | null = null;
let scrubEnded = -Infinity;
let spyQueued = false;

/** Marks the last section whose heading has passed under the bar (the first before any has). */
function spy(): void {
    spyQueued = false;
    if (letters.hidden || browse.hidden || scrub) return;
    const line = barBottom() + 8; // jumpTo leaves a heading 4px below the bar: that counts as passed
    let current = 0;
    for (const [i, h] of heads().entries()) {
        if (h.getBoundingClientRect().top > line) break;
        current = i;
    }
    markCurrent(current);
}

function queueSpy(): void {
    if (spyQueued) return;
    spyQueued = true;
    requestAnimationFrame(spy);
}

window.addEventListener("scroll", queueSpy, { passive: true });
window.addEventListener("resize", queueSpy, { passive: true });

letters.addEventListener("click", (e) => {
    e.preventDefault(); // jumpTo instead of the #fragment, which doesn't know how tall the bar is
    if (performance.now() - scrubEnded < 500) return; // the click a scrub's lift may leave behind
    const a = (e.target as Element).closest("a");
    if (a) jumpTo(jumps().indexOf(a));
});

const bubble = el("div", { className: "agg-letter-bubble", hidden: true });
bubble.setAttribute("aria-hidden", "true");
document.body.append(bubble);

/** The link nearest a point: the finger may wander off the bar, or between its rows. */
function nearest(x: number, y: number): number {
    let best = -1;
    let bestD = Infinity;
    jumps().forEach((a, i) => {
        const r = a.getBoundingClientRect();
        const dx = Math.max(r.left - x, 0, x - r.right);
        const dy = Math.max(r.top - y, 0, y - r.bottom);
        const d = dx * dx + dy * dy;
        if (d < bestD) {
            bestD = d;
            best = i;
        }
    });
    return best;
}

function scrubTo(e: PointerEvent): void {
    if (!scrub) return;
    const i = nearest(e.clientX, e.clientY);
    const a = jumps()[i];
    if (!a) return;
    scrub.index = i;
    markCurrent(i);
    bubble.textContent = a.textContent;
    bubble.hidden = false;
    // Centred 40px above the finger, kept on screen: near the top it can't be above, so it
    // stops at the edge (still clear of a finger on the bar's first row).
    const w = bubble.offsetWidth;
    const h = bubble.offsetHeight;
    bubble.style.left = `${Math.max(4, Math.min(e.clientX - w / 2, window.innerWidth - w - 4))}px`;
    bubble.style.top = `${Math.max(4, e.clientY - h - 40)}px`;
}

function endScrub(e: PointerEvent, jump: boolean): void {
    if (scrub?.pointer !== e.pointerId) return;
    const i = scrub.index;
    scrub = null;
    bubble.hidden = true;
    scrubEnded = performance.now();
    if (jump) jumpTo(i);
    queueSpy();
}

// touch-action: none on the bar (style.css) keeps the page from scrolling under the drag.
letters.addEventListener("pointerdown", (e) => {
    if (e.pointerType !== "touch" || scrub) return;
    scrub = { pointer: e.pointerId, index: -1 };
    letters.setPointerCapture(e.pointerId);
    scrubTo(e);
});
letters.addEventListener("pointermove", (e) => {
    if (scrub?.pointer === e.pointerId) scrubTo(e);
});
letters.addEventListener("pointerup", (e) => endScrub(e, true));
letters.addEventListener("pointercancel", (e) => endScrub(e, false));

// The server draws the bar shown with a dozen or more (templates/aggregate.html),
// so it's there on first paint; fewer read at a glance in any order.
if (!orderBar.hidden) {
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

// A library page: books for offline reading that this device is missing (book-sync.ts).
/*
 * "Your favorites": the user's favorites of this page's kind. Authors are a
 * carousel (.fav-aggs-row, fav-carousel.ts), which aggregate.html's inline
 * script has already drawn from the kept list while the page parsed
 * (data-kept on the section), or reserved with a throbber when there's no
 * kept list yet; other kinds are chips. The list is then asked for again
 * if the user's favorites, or the page's generation (counts, pictures),
 * changed since it was kept, and drawn again only if it differs.
 */
const favSection = document.getElementById("agg-favorites");
const favRow = favSection?.querySelector<HTMLElement>(".fav-aggs-row") ?? null;
const favKind = page.dataset["kind"] ?? "";
/** What the section shows (favKey), so an unchanged list isn't drawn again (its pictures would flicker); null: nothing yet. */
let favShown: string | null = null;

function favKey(mine: AggFavorite[]): string {
    return JSON.stringify(mine.map((f) => [f.name, f.label ?? "", f.href, f.count, f.image_url ?? "", f.initial ?? ""]));
}

function paintFavorites(list: AggFavorite[] | null): void {
    if (!favSection) return;
    const mine = (list ?? []).filter((f) => f.kind === favKind);
    const key = favKey(mine);
    if (key === favShown) return;
    favShown = key;
    if (favRow) {
        favRow.replaceChildren(...mine.map((f) => favCard(f, booksLabel(f.count))));
    } else {
        const prefix = page.dataset["prefix"] ?? "";
        favSection.querySelector(".agg-chips")?.replaceChildren(
            ...mine.map((f) =>
                el(
                    "a",
                    { className: "agg-chip", href: prefix + encodeURIComponent(f.name) },
                    f.label || f.name,
                    el("span", { className: "agg-count" }, String(f.count)),
                ),
            ),
        );
    }
    favSection.hidden = mine.length === 0;
}

if (favSection) {
    setupFavRows(favSection);
    const kept = keptFavorites();
    if (favSection.dataset["kept"] !== undefined) favShown = favKey((kept ?? []).filter((f) => f.kind === favKind));
    else if (kept) paintFavorites(kept);
    const gen = Number(new URL(src, location.href).searchParams.get("v") ?? "");
    void loadFavorites(src && Number.isFinite(gen) ? gen : undefined)
        .then(paintFavorites)
        .catch(() => {
            // Offline: the kept list stands; a throbber waiting for none goes.
            if (favShown === null) paintFavorites([]);
        });
    document.addEventListener("cf:favorites", () => paintFavorites(keptFavorites()));
}

// No startBookSync() here: the category lists show no books, so the
// download sync and the offline page's lists (/api/me/saved-books,
// /me/reading) wait for a page that does.
