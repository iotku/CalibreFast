/**
 * The Discover page ("/", templates/discover.html; docs/notes/Discover.md).
 *
 * The page is the same for everyone; this fills in its rows from the static
 * page files (cached for good under ?v=) and what's personal:
 *   - Shuffle: the next strip of the server's random order
 *     (pages/shuffle/), walked a strip per load (or per click of Shuffle)
 *     so the same books don't come back until the whole library has gone
 *     by; Previous goes back through the strips shown. Books the user
 *     finished are skipped. There's deliberately no link to /shuffle.
 *   - Shelves: the signed-in user's pinned and featured shelves (from
 *     /api/me/home), which they can unpin and reorder; for a signed-out
 *     visitor, the featured ones (pages/discover/featured.json). A pinned
 *     tag's row starts somewhere new on each load and walks its tag with
 *     Shuffle and Previous, like the "More in" rows.
 *   - Recommended for you (signed in): from /api/me/home, next in series
 *     first, each with what it follows or is like. It stays the same until
 *     the user asks for more (Refresh: the next page of their list, from
 *     /api/me/recommended/next); Not interested on a card hides that book
 *     from their recommendations for good, with Undo in its place.
 *   - More in <tag>: a few tag rows; the signed-in user's strongest tags
 *     first, then picked at random from the index in the page (weighted by
 *     size; tags an admin included weighted up), each filled from
 *     /api/tag-strip as it nears the screen and walked a strip at a time
 *     with its own Shuffle and Previous, like the Shuffle row.
 *   - Recently added: the start of pages/page1.json.
 *   - The greeting: the user's name and a quote of the day; for signed-out
 *     visitors, the sign-in line.
 *
 * The rows are one list, in the order the user arranged it with Edit (in
 * the top bar): drag or Move up / Move down, and every section but
 * Continue reading can be turned off. Kept with the user's account
 * (/api/me/home-layout), or in this browser for a signed-out visitor. Each
 * row's icon goes to its page (or pins a "More in" tag); its ✕ takes a
 * pinned row off the page, or a tag out of the "More in" rows for good.
 *
 * Covers use the same URLs (coverUrl) and cards (renderBookCard) as the
 * grid, so a cover cached there is cached here. They load on-screen first,
 * a few at a time (maxCovers).
 */

import { api, byId, maybeId } from "./dom.js";
import type { Book } from "./types.js";
import { coverUrl, escapeHtml, renderBookCard, setupCoverStates } from "./utils.js";
import { makeReorderable, type Reorderable } from "./reorder.js";
import { setupBookClickHandlers, loadBook, HASH_PREFIX } from "./modal.js";
import {
    agrees,
    applyChange,
    cachedMarks,
    coverAction,
    markCard,
    marksFromAnswer,
    saveMarks,
    type BookChange,
    type BookState,
    type Marks,
    type StoredMarks,
} from "./reading.js";
import { forgetLocalReading, localReading, LOCAL_FINISHED } from "./local-reading.js";
import { maxCovers } from "./cover-limits.js";
import { setupDragScroll, setupPageDragScroll } from "./drag-scroll.js";

const STRIP = 12; // books in a row
const PIN_STRIP = 24; // books in a pinned tag's row (Go: shelfRowBooks)
const CURSOR_KEY = "cf-shuffle"; // ShuffleState
const LAYOUT_KEY = "cf-home-layout"; // a signed-out visitor's layout and hidden tags

const root = byId("discover");
const cfg = root.dataset;
const version = cfg["version"] ?? "";
const pageSize = Math.max(1, parseInt(cfg["pageSize"] ?? "", 10) || 60);
const totalBooks = parseInt(cfg["totalBooks"] ?? "", 10) || 0;
const shuffleTotal = parseInt(cfg["shuffleTotal"] ?? "", 10) || 0;
const featuredVersion = cfg["featuredVersion"] ?? "";

let marks: Marks = cachedMarks();

const store = {
    get(key: string): string | null {
        try {
            return localStorage.getItem(key);
        } catch {
            return null;
        }
    },
    set(key: string, value: string): void {
        try {
            localStorage.setItem(key, value);
        } catch {
            /* storage blocked: this visit only */
        }
    },
};

/** A row's "See all" button. */
function seeAll(href: string): string {
    return `<a href="${escapeHtml(href)}" class="row-btn row-all">
        <span>See all</span>
        <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 6l6 6-6 6"/></svg>
    </a>`;
}

const SHUFFLE_ICON = `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 3h5v5M4 20L21 3M21 16v5h-5M15 15l6 6M4 4l5 5"/></svg>`;
const svg = (paths: string, fill = "none"): string =>
    `<svg viewBox="0 0 24 24" width="20" height="20" fill="${fill}" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${paths}</svg>`;
const PIN_PATH = '<path d="M12 17v5"/><path d="M9 10.8V4h6v6.8a2 2 0 0 0 1.1 1.8l1.8.9a2 2 0 0 1 1.1 1.8V17H5v-1.7a2 2 0 0 1 1.1-1.8l1.8-.9A2 2 0 0 0 9 10.8z"/><path d="M8 3h8"/>';
const PIN_ICON = svg(PIN_PATH);
const PINNED_ICON = svg(PIN_PATH, "currentColor");
const TAG_ICON = svg('<path d="M20.6 13.4l-7.2 7.2a2 2 0 0 1-2.8 0L2 12V2h10l8.6 8.6a2 2 0 0 1 0 2.8z"/><circle cx="7" cy="7" r="1.5"/>');
const SHELVES_ICON = svg('<path d="M5 4v16M10 4v16M14.5 5l4 15M3 20h18"/>');
const X_ICON = `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>`;
const BACK_ICON = `<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 14L4 9l5-5"/><path d="M4 9h11a5 5 0 0 1 0 10h-3"/></svg>`;

/** A tag row's Previous and Shuffle (hidden when the tag fits in one strip). */
function tagStepButtons(name: string, hidden: boolean): string {
    const h = hidden ? " hidden" : "";
    return `<button type="button" class="row-btn" data-act="tag-back" aria-label="Previous books in ${name}" disabled${h}>
                    ${BACK_ICON}
                    <span>Previous</span>
                </button>
                <button type="button" class="row-btn" data-act="tag-next" aria-label="More books in ${name}"${h}>
                    ${SHUFFLE_ICON}
                    <span>Shuffle</span>
                </button>`;
}

/* ------------------------------------------------------------------ */
/* Page files                                                          */
/* ------------------------------------------------------------------ */

const pageFiles = new Map<string, Promise<Book[]>>();

/** A page file's books ([] when it can't be had). */
function pageFile(dir: string, n: number): Promise<Book[]> {
    return booksFile(`${dir}page${n}.json?v=${encodeURIComponent(version)}`);
}

/** A file of books, fetched once ([] when it can't be had). */
function booksFile(url: string): Promise<Book[]> {
    let p = pageFiles.get(url);
    if (!p) {
        p = fetch(url, { credentials: "same-origin" })
            .then((r) => (r.ok ? (r.json() as Promise<Book[] | null>) : null))
            .then((b) => b ?? [])
            .catch(() => []);
        pageFiles.set(url, p);
    }
    return p;
}

/* ------------------------------------------------------------------ */
/* Rows                                                                */
/* ------------------------------------------------------------------ */

const loadedCovers = new Set<string>(); // cover URLs the browser has: shown at once

/**
 * Fill a row. With hint, an empty row stays, showing how to fill it (the
 * layout is the same for everyone); without, it's hidden. all shows its
 * "See all" link.
 */
function fillRow(row: string | HTMLElement, books: Book[], { hint = false, all = false } = {}): void {
    const section = typeof row === "string" ? byId(row) : row;
    const list = section.querySelector<HTMLElement>(".row-books");
    if (!list) return;
    list.innerHTML = books.map((b) => renderBookCard(b, loadedCovers.has(coverUrl(b)))).join("");
    for (const card of list.querySelectorAll<HTMLElement>(".book")) markCard(card, marks);
    setupBookClickHandlers(list);
    const hintEl = section.querySelector<HTMLElement>(".row-hint");
    if (hintEl) hintEl.hidden = !(hint && books.length === 0);
    list.hidden = books.length === 0;
    const allEl = section.querySelector<HTMLElement>(".row-all");
    if (allEl) allEl.hidden = !all;
    section.hidden = books.length === 0 && !hint;
    pumpCovers();
}

async function latestRow(): Promise<void> {
    if (!totalBooks) return;
    fillRow("row-latest", (await pageFile("/pages/", 1)).slice(0, STRIP), { all: true });
}

/*
 * Shuffle state, kept in localStorage (cf-shuffle) so it carries across
 * loads: where each strip shown started (the last is the one on screen),
 * and where the next one starts. Strips are consecutive stretches of the
 * server's random order, so going forward never repeats until the whole
 * order has gone by; Previous steps back through the strips shown.
 */
interface ShuffleState {
    version: string;
    starts: number[]; // strips shown, oldest first; the last is on screen
    next: number; // where the next strip starts
}
const SHUFFLE_HISTORY = 20; // strips Previous can go back through

function loadShuffle(): ShuffleState {
    try {
        const v = JSON.parse(store.get(CURSOR_KEY) ?? "null") as Partial<ShuffleState> | null;
        const ok = (n: unknown): n is number => Number.isInteger(n) && (n as number) >= 0 && (n as number) < shuffleTotal;
        if (v?.version === version && ok(v.next) && Array.isArray(v.starts) && v.starts.every(ok)) {
            return { version, starts: v.starts.slice(-SHUFFLE_HISTORY), next: v.next };
        }
    } catch {
        /* unreadable: start somewhere new */
    }
    // A restart made a new order (or first visit): a random place, so
    // visitors don't all start on the same strip.
    return { version, starts: [], next: Math.floor(Math.random() * shuffleTotal) };
}

let shuffle: ShuffleState = { version, starts: [], next: 0 };

/**
 * STRIP books of the random order from start, skipping finished ones and
 * wrapping at the end; and where the strip after it starts.
 */
async function stripFrom(start: number): Promise<{ books: Book[]; end: number }> {
    const books: Book[] = [];
    let i = start;
    for (let scanned = 0; books.length < STRIP && scanned < shuffleTotal; scanned++) {
        const page = Math.floor(i / pageSize) + 1;
        const book = (await pageFile("/pages/shuffle/", page))[i - (page - 1) * pageSize];
        if (book && !marks.finished.has(book.uuid)) books.push(book);
        i = (i + 1) % shuffleTotal;
    }
    // Went all the way round (a library smaller than a strip): start the
    // next one further on anyway, so it isn't the same books in the same order.
    if (i === start) i = (start + Math.max(1, Math.floor(books.length / 2))) % shuffleTotal;
    return { books, end: i };
}

let shuffling = false; // a strip is being fetched: ignore more clicks

/** Show the strip at start: a new one (push) or one already in the history. */
async function showStrip(start: number, push: boolean): Promise<void> {
    shuffling = true;
    try {
        const { books, end } = await stripFrom(start);
        if (push) shuffle.starts = [...shuffle.starts, start].slice(-SHUFFLE_HISTORY);
        shuffle.next = end;
        store.set(CURSOR_KEY, JSON.stringify(shuffle));
        fillRow("row-shuffle", books);
    } finally {
        shuffling = false;
        updateShuffleButtons();
    }
}

const PRELOAD_STRIPS = 2;

/** The covers of the next PRELOAD_STRIPS strips, into the browser's cache. */
async function preloadAhead(): Promise<void> {
    if (!shuffleTotal || shuffling) return;
    let at = shuffle.next;
    for (let k = 0; k < PRELOAD_STRIPS; k++) {
        const { books, end } = await stripFrom(at);
        for (const b of books) preloadCover(coverUrl(b));
        at = end;
    }
}

function updateShuffleButtons(): void {
    const back = maybeId("shuffle-back", HTMLButtonElement);
    if (back) back.disabled = shuffling || shuffle.starts.length < 2;
    const next = maybeId("shuffle-next", HTMLButtonElement);
    if (next) next.disabled = shuffling;
}

/** On load: the strip after the last one shown (or a new place). */
async function shuffleRow(): Promise<void> {
    if (!shuffleTotal) return;
    shuffle = loadShuffle();
    const nextBtn = maybeId("shuffle-next");
    nextBtn?.addEventListener("click", () => {
        if (!shuffling) void showStrip(shuffle.next, true).then(() => {
            // Still on the button: get the strips after this one ready too.
            if (nextBtn.matches(":hover, :focus-visible")) void preloadAhead();
        });
    });
    // About to shuffle: fetch the covers of the next two strips now, so they
    // show at once when they come.
    nextBtn?.addEventListener("pointerenter", () => void preloadAhead());
    nextBtn?.addEventListener("focus", () => void preloadAhead());
    maybeId("shuffle-back")?.addEventListener("click", () => {
        if (shuffling || shuffle.starts.length < 2) return;
        shuffle.starts = shuffle.starts.slice(0, -1);
        void showStrip(shuffle.starts[shuffle.starts.length - 1]!, false);
    });
    await showStrip(shuffle.next, true);
}

/* ------------------------------------------------------------------ */
/* Covers                                                              */
/* ------------------------------------------------------------------ */

const coversInflight = new Set<HTMLImageElement>();
let coversPaused = false;

/** px from what's visible: 0 = on screen (within its row's scrolled view, too). */
function coverDistance(img: HTMLImageElement): number {
    const r = img.getBoundingClientRect();
    const header = document.querySelector<HTMLElement>(".top-bar")?.offsetHeight ?? 0;
    const row = img.closest<HTMLElement>(".row-books")?.getBoundingClientRect();
    const left = Math.max(0, row?.left ?? 0);
    const right = Math.min(window.innerWidth, row?.right ?? window.innerWidth);
    const dy = r.top > window.innerHeight ? r.top - window.innerHeight : r.bottom < header ? header - r.bottom : 0;
    const dx = r.left > right ? r.left - right : r.right < left ? left - r.right : 0;
    return dy + dx;
}

/** On-screen covers first, a few at a time; then the nearest. */
function pumpCovers(): void {
    for (const img of coversInflight) if (!img.isConnected) coversInflight.delete(img);
    if (coversPaused) return;
    const queue = [...root.querySelectorAll<HTMLImageElement>(".book-cover[data-src]")]
        .map((img) => ({ img, dist: coverDistance(img) }))
        .sort((a, b) => a.dist - b.dist);
    const onScreenWaiting = queue.some((c) => c.dist === 0);
    const cap = maxCovers();
    for (const { img, dist } of queue) {
        if (coversInflight.size >= cap) break;
        if (dist > 0 && onScreenWaiting) break; // the screen first
        const src = img.dataset["src"] ?? "";
        delete img.dataset["src"];
        coversInflight.add(img);
        img.src = src;
    }
}

function onCoverSettled(e: Event): void {
    const img = e.target;
    if (!(img instanceof HTMLImageElement) || !img.classList.contains("book-cover")) return;
    if (e.type === "load") loadedCovers.add(img.getAttribute("src") ?? "");
    if (coversInflight.delete(img)) pumpCovers();
    drainPreloads();
}

/*
 * Preloading: covers fetched before their cards exist (the next Shuffle
 * strips), with the same URLs, so the cards find them in the browser's
 * cache. They only use the download slots the page's own covers leave
 * free, at low priority.
 */
const preloadQueue: string[] = [];
const preloadSeen = new Set<string>();
let preloading = 0;

function preloadCover(url: string): void {
    if (preloadSeen.has(url) || loadedCovers.has(url)) return;
    preloadSeen.add(url);
    preloadQueue.push(url);
    drainPreloads();
}

function drainPreloads(): void {
    while (preloadQueue.length && !coversPaused && preloading + coversInflight.size < maxCovers()) {
        const url = preloadQueue.shift()!;
        preloading++;
        const img = new Image();
        img.setAttribute("fetchpriority", "low");
        const done = (ok: boolean): void => {
            preloading--;
            if (ok) loadedCovers.add(url);
            drainPreloads();
        };
        img.onload = () => done(true);
        img.onerror = () => done(false); // no cover: the card shows its title, as it would anyway
        img.src = url;
    }
}
document.addEventListener("load", onCoverSettled, true);
document.addEventListener("error", onCoverSettled, true);

let pumpQueued = false;
function pumpSoon(): void {
    if (pumpQueued) return;
    pumpQueued = true;
    requestAnimationFrame(() => {
        pumpQueued = false;
        pumpCovers();
    });
}
// The window and each row scroll.
document.addEventListener("scroll", pumpSoon, { capture: true, passive: true });
window.addEventListener("resize", pumpSoon);

// The book popup is loading: covers wait until it's done.
document.addEventListener("cf:modal-busy", (e) => {
    coversPaused = (e as CustomEvent<boolean>).detail;
    if (!coversPaused) pumpCovers();
});

/* ------------------------------------------------------------------ */
/* What's the user's: /api/me/home, or this device's reading           */
/* ------------------------------------------------------------------ */

const HOME_KEY = "cf-home"; // the last /api/me/home answer, painted at once next time

interface HomeAnswer {
    marks: StoredMarks;
    reading: Book[];
    reading_count: number;
    wanted: Book[];
    wanted_count: number;
    shelves?: HomeShelf[];
    recommended?: RecBook[];
    tags?: string[];
    layout?: Layout;
    hidden_tags?: string[];
}

/** A recommendation (Go: recBook). */
interface RecBook {
    book: Book;
    kind: "series" | "tags";
    because?: string;
}

/** A shelf row (Go: homeShelf). */
interface HomeShelf {
    kind?: "shelf" | "tag"; // a tag the user pinned: no id
    id?: number;
    name: string;
    href: string;
    global?: boolean;
    featured?: boolean;
    count: number;
    books: Book[];
    start?: number; // a tag's: where its strip starts, and the next one
    next?: number;
}

function repaintMarks(only?: string): void {
    for (const card of root.querySelectorAll<HTMLElement>(".book")) {
        if (!only || card.dataset["uuid"] === only) markCard(card, marks);
    }
}

/** Marks changed in place: keep them in the cached answer too. */
function keepMarks(): void {
    const stored = saveMarks(marks);
    const h = cachedHome();
    if (!stored || !h || h.marks.user !== stored.user) return;
    h.marks = stored;
    store.set(HOME_KEY, JSON.stringify(h));
}

/** The cached answer, if it's whoever the top bar says is signed in. */
function cachedHome(): HomeAnswer | null {
    try {
        const h = JSON.parse(store.get(HOME_KEY) ?? "null") as HomeAnswer | null;
        const me = JSON.parse(store.get("cf-me") ?? "null") as { username?: string | null } | null;
        if (h && me?.username && h.marks?.user === me.username) return h;
    } catch {
        /* unreadable */
    }
    return null;
}

function showHome(h: HomeAnswer): void {
    byId("row-reading-device").hidden = true;
    fillRow("row-reading", h.reading, { hint: true, all: h.reading_count > 0 });
    fillRow("row-wanted", h.wanted, { hint: true, all: h.wanted_count > 0 });
    for (const id of ["row-wanted", "row-recommended"]) byId(id).classList.remove("row-na");
    if (!editing) {
        layout = cleanLayout(h.layout);
        hiddenTags = cleanTags(h.hidden_tags);
    }
    showShelves(h.shelves ?? [], true);
    showRecommended(h.recommended ?? []);
    affinityTags = (h.tags ?? []).map((t) => t.toLowerCase());
    showHiddenTags();
    recheckTagRows();
}

/** A signed-out visitor's marks: what was read on this device. */
function guestMarks(): Marks {
    const reading = new Map<string, number | null>();
    const resume = new Map<string, string>();
    const finished = new Set<string>();
    for (const [uuid, e] of Object.entries(localReading())) {
        if (e.percent >= LOCAL_FINISHED) finished.add(uuid);
        else {
            reading.set(uuid, e.percent);
            resume.set(uuid, (e.format === "pdf" ? "/read-pdf?uuid=" : "/read?uuid=") + encodeURIComponent(uuid));
        }
    }
    return { user: null, signedIn: false, reading, resume, wanted: new Set(), finished, favorites: new Set() };
}

/** Continue reading, for a signed-out visitor: this device's books, newest first. */
async function showGuest(): Promise<void> {
    const index = localReading();
    const uuids = Object.entries(index)
        .filter(([, e]) => e.percent < LOCAL_FINISHED)
        .sort(([, a], [, b]) => b.updatedAt - a.updatedAt)
        .slice(0, STRIP * 2)
        .map(([u]) => u);
    let books: Book[] = [];
    if (uuids.length) {
        try {
            const res = await fetch(`/api/books?uuids=${uuids.map(encodeURIComponent).join(",")}`, { credentials: "same-origin" });
            if (res.ok) {
                books = ((await res.json()) as Book[] | null) ?? [];
                // Gone from the library: drop them here too.
                const have = new Set(books.map((b) => b.uuid));
                forgetLocalReading(uuids.filter((u) => !have.has(u)));
            }
        } catch {
            /* offline: nothing to show */
        }
    }
    // Started from a stale "signed out" (the top bar's cache), and the server
    // has since said otherwise: its rows stand.
    if (serverSignedIn) return;
    // Rows only an account has: not shown, nor offered when editing.
    for (const id of ["row-wanted", "row-recommended"]) {
        byId(id).hidden = true;
        byId(id).classList.add("row-na");
    }
    if (!editing) ({ layout, hiddenTags } = guestLayout());
    byId("row-reading-device").hidden = false;
    fillRow("row-reading", books);
    const shelves = await featured();
    if (!serverSignedIn) showShelves(shelves, false);
    showHiddenTags();
    recheckTagRows();
}

let homeFetchedAt = 0;
/** What the server last said: signed in (true), out (false), or not yet asked. */
let serverSignedIn: boolean | null = null;

/** Fetch what's the user's (or note they're signed out) and show it. */
async function refreshHome(): Promise<void> {
    homeFetchedAt = Date.now();
    let res: Response;
    try {
        res = await fetch("/api/me/home", { credentials: "same-origin" });
    } catch {
        return; // offline: keep what's shown
    }
    if (res.status === 401) {
        serverSignedIn = false;
        store.set(HOME_KEY, "null");
        marksFromAnswer(null);
        marks = guestMarks();
        await showGuest();
    } else if (res.ok) {
        serverSignedIn = true;
        const h = (await res.json()) as HomeAnswer;
        store.set(HOME_KEY, JSON.stringify(h));
        marks = marksFromAnswer(h.marks);
        showHome(h);
    } else {
        return;
    }
    repaintMarks();
    showAccountLine();
}

/** Paints from what's known at once; resolves once the server has answered. */
function setupHome(): Promise<void> {
    const cached = cachedHome();
    if (cached) showHome(cached);
    else if (signedIn() === false) {
        marks = guestMarks();
        void showGuest();
    }
    const fresh = refreshHome();
    window.addEventListener("pageshow", (e) => {
        if (e.persisted) void refreshHome();
    });
    document.addEventListener("visibilitychange", () => {
        // Back from the reader in another tab: progress may have moved.
        if (document.visibilityState === "visible" && Date.now() - homeFetchedAt > 15_000) void refreshHome();
    });
    // Saved in the book popup (user.ts). A status change moves books between
    // the rows, which only the server can fill; a favorite is just a mark.
    document.addEventListener("cf:status", (e) => {
        const change = (e as CustomEvent<BookChange>).detail;
        if (change.status !== undefined || !applyChange(marks, change)) {
            void refreshHome();
            return;
        }
        keepMarks();
        repaintMarks(change.uuid);
    });
    // The popup's controls, just loaded: what the server says now.
    document.addEventListener("cf:book-state", (e) => {
        if (!agrees(marks, (e as CustomEvent<BookState>).detail)) void refreshHome();
    });
    // The buttons over a card's cover; "continue reading" is a plain link.
    document.addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".cover-actions button") : null;
        const uuid = btn?.closest<HTMLElement>(".book")?.dataset["uuid"];
        if (!btn || !uuid) return;
        e.preventDefault();
        const action = btn.dataset["action"] ?? "";
        void coverAction(action, uuid, marks, () => repaintMarks(uuid)).then(() => {
            // Saved has a row of its own; a favorite is only the mark.
            if (action === "want") void refreshHome();
            else keepMarks();
        });
    });
    return fresh;
}

/* ------------------------------------------------------------------ */
/* Shelves                                                             */
/* ------------------------------------------------------------------ */

let featuredFile: Promise<HomeShelf[]> | null = null;

/** The featured shelves, for a signed-out visitor ([] when there are none). */
function featured(): Promise<HomeShelf[]> {
    if (!featuredVersion) return Promise.resolve([]);
    featuredFile ??= fetch(`/pages/discover/featured.json?v=${encodeURIComponent(featuredVersion)}`, { credentials: "same-origin" })
        .then((r) => (r.ok ? (r.json() as Promise<HomeShelf[] | null>) : null))
        .then((s) => s ?? [])
        .catch(() => []);
    return featuredFile;
}

/**
 * A pinned tag's row as this page has it: its walker and the books it
 * shows. The server starts a tag's row at a random place on each answer,
 * so the page differs on every load; the answers that come after (a mark
 * changed, back to the tab) keep the books the row already has instead of
 * swapping them under the user, and where Shuffle got to.
 */
const pinnedTagWalkers = new Map<string, TagWalker>();

/** The pinned rows' ids ("s:7", "t:Space Opera"), in the server's order. */
let pinOrder: string[] = [];

/**
 * The shelf rows, among the others where the layout puts them. own: the
 * signed-in user's, with ✕ (and a pinned tag's pin) to take them off.
 * Empty shelves are left out.
 */
function showShelves(shelves: HomeShelf[], own: boolean): void {
    if (editing) return; // being arranged: the list stays as the user has it
    const list = byId("home-rows");
    for (const old of list.querySelectorAll(":scope > .pin-row")) old.remove();
    pinnedTags = new Set(shelves.filter((s) => s.kind === "tag").map((s) => s.name.toLowerCase()));
    pinOrder = [];
    const slot = byId("row-recommended"); // for now: applyLayout puts them in place
    shelves.forEach((s, i) => {
        const tag = s.kind === "tag";
        let walker: TagWalker | undefined;
        if (tag) {
            const key = s.name.toLowerCase();
            walker = pinnedTagWalkers.get(key);
            if (!walker) {
                walker = {
                    row: { n: 0, name: s.name, href: s.href, count: s.count },
                    size: PIN_STRIP,
                    cur: { starts: [s.start ?? 0], next: s.next ?? null },
                    books: s.books,
                    busy: false,
                };
                pinnedTagWalkers.set(key, walker);
            }
            walker.row.count = s.count;
            s = { ...s, books: walker.books };
        }
        if (!s.books.length) return;
        const section = document.createElement("section");
        section.className = "row home-row pin-row";
        // "s:7" a shelf, "t:Space Opera" a tag (the order sent back says which).
        const id = tag ? `t:${s.name}` : `s:${s.id}`;
        section.dataset["id"] = id;
        section.dataset["name"] = s.name;
        section.setAttribute("aria-labelledby", `pin-${i}-title`);
        pinOrder.push(id);
        const name = escapeHtml(s.name);
        const icon = !tag
            ? `<a class="row-icon" href="/shelves" title="Shelves" aria-label="All shelves">${SHELVES_ICON}</a>`
            : own
              ? `<button type="button" class="row-icon" data-act="unpin" aria-pressed="true" title="Pinned: unpin" aria-label="Unpin ${name} from home">${PINNED_ICON}</button>`
              : `<a class="row-icon" href="/tags" title="Tags" aria-label="All tags">${TAG_ICON}</a>`;
        section.innerHTML = `
            <header class="row-head">
                <h2 id="pin-${i}-title">${icon}<a class="row-title-link" href="${escapeHtml(s.href)}">${name}</a>${tag ? '<span class="row-sub"> tag</span>' : ""}</h2>
                <div class="row-tools">
                    ${tag ? tagStepButtons(name, s.count <= PIN_STRIP) : ""}
                    ${seeAll(s.href)}
                    ${own ? `<button type="button" class="row-btn row-x" data-act="unpin" title="Remove from home" aria-label="Remove ${name} from home">${X_ICON}</button>` : ""}
                </div>
            </header>
            <div class="row-books"></div>`;
        slot.before(section);
        fillRow(section, s.books, { all: true });
        if (walker) {
            tagWalkers.set(section, walker);
            updateTagButtons(section, walker);
        }
    });
    applyLayout();
    updateTagRows(own);
}

/* ------------------------------------------------------------------ */
/* Layout: the rows' order, sections turned off, hidden tags           */
/* ------------------------------------------------------------------ */

/** Go: homeLayout. order: the rows' ids; hidden: sections turned off. */
interface Layout {
    order: string[];
    hidden: string[];
}

/** The rows' order by default; "pins" stands for the pinned rows. */
const DEFAULT_ORDER = ["k:reading", "k:wanted", "pins", "k:recommended", "k:shuffle", "k:latest", "k:tags"];
/** Sections that can be turned off (Go: homeSections). */
const TOGGLEABLE = ["greeting", "wanted", "recommended", "shuffle", "latest", "tags"];

let layout: Layout = { order: [], hidden: [] };
/** Tags the user wants no "More in" rows for. */
let hiddenTags: string[] = [];

const strings = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

function cleanLayout(v: unknown): Layout {
    const l = (v ?? {}) as Partial<Layout>;
    return { order: strings(l.order), hidden: strings(l.hidden).filter((k) => TOGGLEABLE.includes(k)) };
}

function cleanTags(v: unknown): string[] {
    return strings(v);
}

/** A signed-out visitor's, from this browser. */
function guestLayout(): { layout: Layout; hiddenTags: string[] } {
    try {
        const v = JSON.parse(store.get(LAYOUT_KEY) ?? "null") as (Layout & { hiddenTags?: unknown }) | null;
        return { layout: cleanLayout(v), hiddenTags: cleanTags(v?.hiddenTags) };
    } catch {
        return { layout: cleanLayout(null), hiddenTags: [] };
    }
}

/** Whether the page is a signed-in user's (theirs to keep on the server). */
function isOwn(): boolean {
    return serverSignedIn ?? signedIn() === true;
}

/** Keep the layout and hidden tags: on the server, or in this browser. */
function saveLayout(what: "layout" | "tags" = "layout"): void {
    if (!isOwn()) {
        store.set(LAYOUT_KEY, JSON.stringify({ ...layout, hiddenTags }));
        return;
    }
    if (what === "layout") void api("PUT", "/api/me/home-layout", layout).catch(() => {});
    const h = cachedHome();
    if (!h) return;
    h.layout = layout;
    h.hidden_tags = hiddenTags;
    store.set(HOME_KEY, JSON.stringify(h));
}

/**
 * The saved order merged with the rows there are now: ids no longer there
 * dropped; a section missing goes after the one before it by default, a
 * pinned row missing (pinned since) beside its neighbours among the pins.
 */
function mergeOrder(saved: string[], fixed: string[], pins: string[]): string[] {
    const lc = (id: string): string => id.toLowerCase();
    const have = new Map([...fixed, ...pins].map((id) => [lc(id), id]));
    const out: string[] = [];
    for (const id of saved) {
        const real = have.get(lc(id));
        if (real && !out.includes(real)) out.push(real);
    }
    const at = (id: string): number => out.indexOf(id);
    const lastPin = (): number => out.reduce((last, id, i) => (pins.includes(id) ? i : last), -1);
    /** Just after whatever comes before key by default and is placed. */
    const slot = (key: string): number => {
        for (let j = DEFAULT_ORDER.indexOf(key) - 1; j >= 0; j--) {
            const d = DEFAULT_ORDER[j]!;
            const k = d === "pins" ? lastPin() : at(d);
            if (k >= 0) return k + 1;
        }
        return 0;
    };
    for (const id of fixed) if (at(id) < 0) out.splice(slot(id), 0, id);
    pins.forEach((id, i) => {
        if (at(id) >= 0) return;
        let k = -1;
        for (let j = i - 1; j >= 0 && k < 0; j--) if (at(pins[j]!) >= 0) k = at(pins[j]!) + 1;
        for (let j = i + 1; j < pins.length && k < 0; j++) k = at(pins[j]!);
        out.splice(k >= 0 ? k : slot("pins"), 0, id);
    });
    return out;
}

/** The rows in the layout's order, and the sections turned off. */
function applyLayout(): void {
    if (editing) return;
    const list = byId("home-rows");
    const rows = new Map([...list.querySelectorAll<HTMLElement>(":scope > .home-row")].map((el) => [el.dataset["id"] ?? "", el]));
    const order = mergeOrder(
        layout.order,
        DEFAULT_ORDER.filter((id) => rows.has(id)),
        pinOrder.filter((id) => rows.has(id)),
    );
    // Only what moves is moved: a row put back in the page loses where it
    // was scrolled to.
    let prev: HTMLElement | null = null;
    for (const id of order) {
        const el = rows.get(id)!;
        if (prev ? prev.nextElementSibling !== el : list.firstElementChild !== el) {
            if (prev) prev.after(el);
            else list.prepend(el);
        }
        prev = el;
    }
    for (const el of [byId("greeting"), ...rows.values()]) {
        const key = el.dataset["key"];
        el.classList.toggle("row-off", !!key && layout.hidden.includes(key));
    }
    pumpSoon();
}

/** The hidden tags, under the "More in" rows while editing, each to bring back. */
function showHiddenTags(): void {
    const box = byId("hidden-tags");
    box.hidden = hiddenTags.length === 0;
    box.innerHTML =
        "<span>Not shown:</span> " +
        hiddenTags
            .map(
                (t) =>
                    `<button type="button" class="row-btn" data-act="unhide-tag" data-name="${escapeHtml(t)}" aria-label="Show ${escapeHtml(t)} again">` +
                    `${escapeHtml(t)}${X_ICON}</button>`, // no <span>: narrow screens hide a button's
            )
            .join(" ");
}

function setTagHidden(name: string, hide: boolean): void {
    const lc = name.toLowerCase();
    hiddenTags = hiddenTags.filter((t) => t.toLowerCase() !== lc);
    if (hide) hiddenTags.push(name);
    if (isOwn()) {
        void api(hide ? "PUT" : "DELETE", `/api/me/hidden-tags/${encodeURIComponent(name)}`).catch(() => {});
    }
    saveLayout("tags");
    showHiddenTags();
}

let editing: Reorderable | null = null;

/** A section's Show switch, while editing. */
function addToggle(el: HTMLElement): void {
    const key = el.dataset["key"];
    const head = el.querySelector<HTMLElement>(":scope > .row-head");
    if (!key || !head || !TOGGLEABLE.includes(key) || el.classList.contains("row-na")) return;
    const label = document.createElement("label");
    label.className = "row-toggle";
    label.innerHTML = `<input type="checkbox" role="switch" /><span>Show</span>`;
    const box = label.querySelector("input")!;
    box.checked = !layout.hidden.includes(key);
    box.setAttribute("aria-label", `Show ${el.dataset["name"] ?? key}`);
    box.addEventListener("change", () => {
        layout.hidden = layout.hidden.filter((k) => k !== key);
        if (!box.checked) layout.hidden.push(key);
        el.classList.toggle("row-off", !box.checked);
        saveLayout();
    });
    head.append(label);
}

/**
 * Edit (the top bar's button): every row shrinks to its title, with
 * handles to move it and a switch to turn it off; Done puts the page back.
 */
function setEditing(on: boolean): void {
    if (on === !!editing) return;
    const btn = byId("home-edit");
    const list = byId("home-rows");
    if (on) {
        // Rows a signed-out visitor can't have: last, out of the way.
        for (const na of list.querySelectorAll(":scope > .row-na")) list.append(na);
        editing = makeReorderable({
            list,
            items: ".home-row:not(.row-na)",
            label: (el) => el.dataset["name"] ?? "",
            slot: (el) => el.querySelector<HTMLElement>(":scope > .row-head")!,
            onChange: () => {
                layout.order = [...list.querySelectorAll<HTMLElement>(":scope > .home-row")].map((el) => el.dataset["id"] ?? "");
                saveLayout();
                if (!isOwn()) return;
                // The pins' own order too, as before (featured shelves keep their place).
                pinOrder = layout.order.filter((id) => /^[st]:/.test(id));
                void api("PUT", "/api/me/pins", {
                    rows: pinOrder.map((id) => (id.startsWith("t:") ? { tag: id.slice(2) } : { shelf: Number(id.slice(2)) })),
                }).catch(() => {});
            },
        });
        for (const el of [byId("greeting"), ...list.querySelectorAll<HTMLElement>(":scope > .home-row")]) addToggle(el);
    } else {
        editing?.stop();
        editing = null;
        for (const t of document.querySelectorAll(".row-toggle")) t.remove();
        applyLayout();
        void refreshHome();
    }
    document.body.classList.toggle("home-editing", on);
    btn.setAttribute("aria-pressed", String(on));
    const label = on ? "Done editing" : "Edit home page";
    btn.setAttribute("aria-label", label);
    btn.title = label;
    pumpSoon();
}

/** The user's strongest tags (lower case): their rows come first. */
let affinityTags: string[] = [];

const HIDE_ICON = `<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"/></svg>`;

/** Recommended for you, each card saying why, with Not interested. */
function showRecommended(recs: RecBook[]): void {
    const section = byId("row-recommended");
    fillRow(section, recs.map((r) => r.book), { hint: true });
    byId("rec-next").hidden = recs.length === 0;
    recs.forEach((r, i) => {
        const card = section.querySelectorAll<HTMLElement>(".row-books > .book")[i];
        if (!card) return;
        const hide = document.createElement("button");
        hide.type = "button";
        hide.className = "cover-action rec-hide";
        hide.dataset["act"] = "rec-hide";
        hide.title = "Not interested";
        hide.setAttribute("aria-label", `Not interested in ${r.book.title || "this book"}`);
        hide.innerHTML = HIDE_ICON;
        card.append(hide);
        const info = card.querySelector(".book-info");
        if (!info || !r.because) return;
        const why = document.createElement("div");
        why.className = "book-because";
        why.textContent = r.kind === "series" ? `Next after ${r.because}` : `Like ${r.because}`;
        info.append(why);
    });
}

/** The cached answer's Recommended row, changed in place. */
function keepRecommended(change: (recs: RecBook[]) => RecBook[]): void {
    const h = cachedHome();
    if (!h) return;
    h.recommended = change(h.recommended ?? []);
    store.set(HOME_KEY, JSON.stringify(h));
}

function setupRecommended(): void {
    const section = byId("row-recommended");
    const next = byId("rec-next", HTMLButtonElement);
    next.addEventListener("click", () => {
        if (next.disabled) return;
        next.disabled = true;
        void api<{ recommended: RecBook[] }>("POST", "/api/me/recommended/next")
            .then((a) => {
                showRecommended(a.recommended ?? []);
                keepRecommended(() => a.recommended ?? []);
                const list = section.querySelector<HTMLElement>(".row-books");
                if (list) list.scrollLeft = 0;
            })
            .catch(() => {})
            .finally(() => (next.disabled = false));
    });
    section.addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="rec-hide"], [data-act="rec-undo"]') : null;
        const card = btn?.closest<HTMLElement>(".book");
        const uuid = card?.dataset["uuid"];
        if (!btn || !card || !uuid) return;
        e.preventDefault();
        const hide = btn.dataset["act"] === "rec-hide";
        // At once here: the card says it's hidden, with Undo; the row fills
        // the gap the next time it's loaded.
        const set = (on: boolean): void => {
            card.classList.toggle("rec-hidden", on);
            card.querySelector(".rec-gone")?.remove();
            if (!on) return;
            const gone = document.createElement("div");
            gone.className = "rec-gone";
            gone.innerHTML = `<p>Not interested</p><button type="button" class="row-btn" data-act="rec-undo">Undo</button>`;
            card.append(gone);
            gone.querySelector("button")!.focus();
        };
        set(hide);
        void api("PUT", `/api/me/books/${encodeURIComponent(uuid)}/not-interested`, { not_interested: hide })
            .then(() => {
                if (hide) keepRecommended((recs) => recs.filter((r) => r.book.uuid !== uuid));
            })
            .catch(() => set(!hide));
    });
}

/** Pinned tags (lower case): their "More in" rows step aside. */
let pinnedTags = new Set<string>();

function setupShelves(): void {
    byId("home-edit").addEventListener("click", () => setEditing(!editing));
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && editing && !document.querySelector(".modal-overlay.active")) setEditing(false);
    });
    const list = byId("home-rows");
    list.addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="unpin"]') : null;
        const row = btn?.closest<HTMLElement>(".pin-row");
        if (!btn || !row) return;
        // Off the page: at once here; the server's answer follows.
        row.remove();
        const id = row.dataset["id"] ?? "";
        pinOrder = pinOrder.filter((p) => p !== id);
        if (id.startsWith("t:")) {
            pinnedTags.delete(id.slice(2).toLowerCase());
            updateTagRows(true);
        }
        const path = id.startsWith("t:") ? `/api/me/tag-pins/${encodeURIComponent(id.slice(2))}` : `/api/me/pins/${id.slice(2)}`;
        void api("DELETE", path)
            .catch(() => {})
            .then(() => refreshHome());
    });
    byId("hidden-tags").addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="unhide-tag"]') : null;
        const name = btn?.dataset["name"];
        if (name) setTagHidden(name, false);
    });
}

/* ------------------------------------------------------------------ */
/* Tag rows                                                            */
/* ------------------------------------------------------------------ */

/** An entry of the tag-row index (Go: tagRow). */
interface TagRow {
    n: number;
    name: string;
    href: string;
    count: number;
    include?: boolean;
}

const TAG_ROWS = 3;

/** k rows, at random, bigger tags (and included ones) more likely. */
function pickTagRows(index: TagRow[], k: number): TagRow[] {
    const top = Math.max(1, ...index.map((t) => t.count));
    const pool = index.map((t) => ({ t, w: t.include ? top : Math.max(1, t.count) }));
    const out: TagRow[] = [];
    while (out.length < k && pool.length) {
        let r = Math.random() * pool.reduce((s, p) => s + p.w, 0);
        let i = 0;
        while (i < pool.length - 1 && (r -= pool[i]!.w) > 0) i++;
        out.push(pool.splice(i, 1)[0]!.t);
    }
    return out;
}

/*
 * Each tag row walks its tag's books a strip at a time (/api/tag-strip: the
 * tag's books in the server's random order), as the Shuffle row walks the
 * library: Shuffle shows the next strip, Previous goes back, and where a
 * tag got to is kept in localStorage (cf-tag-shuffle) so the next load
 * shows the strip after it. A tag seen for the first time starts at a
 * random place.
 */
interface TagCursor {
    starts: number[]; // strips shown, oldest first; the last is on screen
    next: number | null; // where the next strip starts; null: anywhere
}
const TAG_CURSOR_KEY = "cf-tag-shuffle";
const TAG_CURSORS_MAX = 60; // tags remembered, most recently walked kept

function loadTagCursors(): Map<string, TagCursor> {
    const out = new Map<string, TagCursor>();
    try {
        const v = JSON.parse(store.get(TAG_CURSOR_KEY) ?? "null") as { version?: string; tags?: [string, TagCursor][] } | null;
        // A restart made a new order: the places mean nothing in it.
        if (v?.version !== version || !Array.isArray(v.tags)) return out;
        const ok = (n: unknown): n is number => Number.isInteger(n) && (n as number) >= 0;
        for (const [k, c] of v.tags) {
            if (typeof k === "string" && Array.isArray(c?.starts) && c.starts.every(ok) && (c.next === null || ok(c.next))) {
                out.set(k, { starts: c.starts.slice(-SHUFFLE_HISTORY), next: c.next });
            }
        }
    } catch {
        /* unreadable: start somewhere new */
    }
    return out;
}

function saveTagCursor(key: string, cur: TagCursor): void {
    const all = loadTagCursors();
    all.delete(key); // to the end: the most recent
    all.set(key, cur);
    const tags = [...all].slice(-TAG_CURSORS_MAX);
    store.set(TAG_CURSOR_KEY, JSON.stringify({ version, tags }));
}

/** A strip of a tag's books (Go: tagStripAnswer). */
interface TagStrip {
    books: Book[];
    start: number;
    next: number;
    count: number;
}

const tagStrips = new Map<string, Promise<TagStrip | null>>();

/** n of the tag's books from start (null: a random place). */
function tagStrip(name: string, start: number | null, n: number): Promise<TagStrip | null> {
    const key = `${name.toLowerCase()}\n${start}\n${n}`;
    let p = start === null ? undefined : tagStrips.get(key);
    if (!p) {
        const at = start === null ? "" : `&start=${start}`;
        p = fetch(`/api/tag-strip?tag=${encodeURIComponent(name)}&n=${n}${at}`, { credentials: "same-origin" })
            .then((r) => (r.ok ? (r.json() as Promise<TagStrip>) : null))
            .catch(() => null);
        if (start !== null) {
            tagStrips.set(key, p);
            void p.then((s) => s ?? tagStrips.delete(key)); // a failure is tried again
        }
    }
    return p;
}

/** A tag row's Shuffle and Previous ("More in", or a pinned tag's). */
interface TagWalker {
    row: TagRow;
    size: number; // books in a strip
    cur: TagCursor;
    books: Book[]; // on screen
    busy: boolean;
}

const tagWalkers = new WeakMap<HTMLElement, TagWalker>();

function updateTagButtons(section: HTMLElement, w: TagWalker): void {
    const back = section.querySelector<HTMLButtonElement>('[data-act="tag-back"]');
    if (back) back.disabled = w.busy || w.cur.starts.length < 2;
    const next = section.querySelector<HTMLButtonElement>('[data-act="tag-next"]');
    if (next) next.disabled = w.busy;
}

/**
 * Show the walker's strip at start (null: where it got to, or anywhere): a
 * new one (push) or one already in its history. False when it had nothing
 * to show (every book finished, or no answer).
 */
async function showTagStrip(section: HTMLElement, w: TagWalker, start: number | null, push: boolean): Promise<boolean> {
    w.busy = true;
    updateTagButtons(section, w);
    try {
        const s = await tagStrip(w.row.name, start, w.size);
        if (!s) return false;
        // The server leaves out what the user finished; this also leaves
        // out what was finished on this device.
        const books = s.books.filter((b) => !marks.finished.has(b.uuid));
        if (!books.length && section.querySelector(".row-books .book")) return false;
        if (push) w.cur.starts = [...w.cur.starts, s.start].slice(-SHUFFLE_HISTORY);
        w.cur.next = s.next;
        saveTagCursor(w.row.name.toLowerCase(), w.cur);
        if (books.length) {
            w.books = books;
            fillRow(section, books, { all: true });
        }
        return books.length > 0;
    } finally {
        w.busy = false;
        updateTagButtons(section, w);
    }
}

/** Rows showing a tag hidden or pinned since they were filled take another. */
let recheckTagRows: () => void = () => {};

function setupTagRows(): void {
    const box = byId("tag-rows");
    const rows = byId("tag-list");
    let index: TagRow[] = [];
    try {
        index = JSON.parse(box.dataset["tags"] ?? "[]") as TagRow[];
    } catch {
        /* none */
    }
    if (!index.length) {
        box.classList.add("row-na"); // nothing to turn on
        return;
    }
    const cursors = loadTagCursors();
    // Every tag in a random order (bigger ones sooner). Each row takes the
    // next one as it nears the screen, by when the user's pinned tags are
    // usually known: a pinned tag has its own row, so it's skipped.
    const queue = pickTagRows(index, index.length);
    const fill = async (section: HTMLElement, i: number): Promise<void> => {
        // The next tag that isn't pinned and has books the user hasn't finished.
        for (;;) {
            const free = (t: TagRow): boolean => !pinnedTags.has(t.name.toLowerCase()) && !isHiddenTag(t.name);
            // The user's own tags first (when they have a row), then the rest.
            let k = -1;
            for (const a of affinityTags) {
                k = queue.findIndex((t) => free(t) && t.name.toLowerCase() === a);
                if (k >= 0) break;
            }
            if (k < 0) k = queue.findIndex(free);
            if (k < 0) {
                section.hidden = true; // none left
                return;
            }
            const row = queue.splice(k, 1)[0]!;
            const cur = cursors.get(row.name.toLowerCase()) ?? { starts: [], next: null };
            const w: TagWalker = { row, size: STRIP, cur, books: [], busy: false };
            const name = escapeHtml(row.name);
            const href = escapeHtml(row.href);
            section.dataset["name"] = row.name;
            section.hidden = false;
            // The pin for a signed-in user (updateTagRows), else the tag's icon.
            section.querySelector("header")!.innerHTML = `
            <h2 id="tag-row-${i}-title"><button type="button" class="row-icon tag-pin" data-act="pin-tag" title="Pin to home" aria-label="Pin ${name} to home" hidden>${PIN_ICON}</button><a class="row-icon tag-link" href="/tags" title="Tags" aria-label="All tags">${TAG_ICON}</a>More in <a class="row-title-link" href="${href}">${name}</a></h2>
            <div class="row-tools">
                ${tagStepButtons(name, row.count <= STRIP)}
                ${seeAll(row.href)}
                <button type="button" class="row-btn row-x" data-act="hide-tag" title="Don't show this tag here" aria-label="Don't show ${name} on the home page">${X_ICON}</button>
            </div>`;
            tagWalkers.set(section, w);
            if (await showTagStrip(section, w, w.cur.next, true)) break;
        }
        updateTagRows(isOwn());
        box.hidden = !rows.querySelector(".row:not([hidden])");
    };
    const near = new IntersectionObserver(
        (entries) => {
            for (const e of entries) {
                if (!e.isIntersecting) continue;
                near.unobserve(e.target);
                const el = e.target as HTMLElement;
                void fill(el, Number(el.dataset["i"]));
            }
        },
        { rootMargin: "800px 0px" },
    );
    box.hidden = false;
    for (let i = 0; i < Math.min(TAG_ROWS, index.length); i++) {
        const section = document.createElement("section");
        section.className = "row tag-row";
        section.dataset["i"] = String(i);
        section.setAttribute("aria-labelledby", `tag-row-${i}-title`);
        section.innerHTML = `<header class="row-head"></header><div class="row-books"></div>`;
        rows.append(section);
        near.observe(section);
    }
    updateTagRows(signedIn() === true);
    recheckTagRows = () => {
        for (const section of rows.querySelectorAll<HTMLElement>(".tag-row")) {
            const name = section.dataset["name"];
            if (name && isHiddenTag(name)) void fill(section, Number(section.dataset["i"]));
        }
    };
    rows.addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="pin-tag"], [data-act="hide-tag"]') : null;
        const section = btn?.closest<HTMLElement>(".tag-row");
        const name = section?.dataset["name"];
        if (!btn || !section || !name) return;
        if (btn.dataset["act"] === "hide-tag") {
            // Not on this page again: the row takes the next tag.
            setTagHidden(name, true);
            void fill(section, Number(section.dataset["i"]));
            return;
        }
        // Pinned: it moves up among the pinned rows (from the server's answer).
        pinnedTags.add(name.toLowerCase());
        updateTagRows(true);
        void api("PUT", `/api/me/tag-pins/${encodeURIComponent(name)}`)
            .catch(() => {})
            .then(() => refreshHome());
    });
}

function isHiddenTag(name: string): boolean {
    const lc = name.toLowerCase();
    return hiddenTags.some((t) => t.toLowerCase() === lc);
}

/** Shuffle and Previous of the tag rows ("More in" and pinned ones). */
function setupTagSteps(): void {
    root.addEventListener("click", (e) => {
        const step = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="tag-next"], [data-act="tag-back"]') : null;
        const section = step?.closest<HTMLElement>(".row");
        const w = section && tagWalkers.get(section);
        if (!step || !section || !w || w.busy) return;
        if (step.dataset["act"] === "tag-next") {
            void showTagStrip(section, w, w.cur.next, true).then(() => {
                // Still on the button: get the strip after this one ready too.
                if (step.matches(":hover, :focus-visible")) preloadTagStrip(w);
            });
        } else if (w.cur.starts.length >= 2) {
            w.cur.starts = w.cur.starts.slice(0, -1);
            void showTagStrip(section, w, w.cur.starts[w.cur.starts.length - 1]!, false);
        }
    });
    // About to shuffle: fetch the next strip's covers now, so they show at once.
    const aboutTo = (e: Event): void => {
        const btn = e.target instanceof Element ? e.target.closest('[data-act="tag-next"]') : null;
        const section = btn?.closest<HTMLElement>(".row");
        const w = section && tagWalkers.get(section);
        if (w) preloadTagStrip(w);
    };
    root.addEventListener("pointerover", aboutTo);
    root.addEventListener("focusin", aboutTo);
}

/** The covers of a tag row's next strip, into the browser's cache. */
function preloadTagStrip(w: TagWalker): void {
    if (w.busy || w.cur.next === null) return;
    void tagStrip(w.row.name, w.cur.next, w.size).then((s) => {
        for (const b of s?.books ?? []) preloadCover(coverUrl(b));
    });
}

/**
 * "More in" rows: the pin for a signed-in user (the tag's icon for anyone
 * else); a tag already pinned has its own row among the others, so its
 * "More in" row steps aside.
 */
function updateTagRows(own: boolean): void {
    const box = maybeId("tag-list");
    if (!box) return;
    for (const section of box.querySelectorAll<HTMLElement>(".tag-row")) {
        const pin = section.querySelector<HTMLElement>(".tag-pin");
        if (pin) pin.hidden = !own;
        const link = section.querySelector<HTMLElement>(".tag-link");
        if (link) link.hidden = own;
        const pinned = pinnedTags.has((section.dataset["name"] ?? "").toLowerCase());
        section.classList.toggle("pinned-away", pinned);
    }
}

/* ------------------------------------------------------------------ */
/* Greeting                                                            */
/* ------------------------------------------------------------------ */

/**
 * Who's signed in, as the top bar last saw it (nav.ts keeps its answer in
 * cf-me and says when it changes): true, false, or null while unknown.
 */
function signedIn(): boolean | null {
    if (marks.signedIn) return true;
    const me = topBarMe();
    return me === undefined ? null : !!me.username;
}

/** nav.ts's cf-me (undefined when it hasn't said). */
function topBarMe(): { username?: string | null } | undefined {
    try {
        const me = store.get("cf-me");
        return me === null ? undefined : (JSON.parse(me) as { username?: string | null });
    } catch {
        return undefined;
    }
}

/** The sign-in line: for signed-out visitors only; hidden until that's known. */
function showAccountLine(): void {
    const line = maybeId("intro-account");
    if (line) line.hidden = signedIn() !== false;
}

/** Quotes about reading, one a day (all long out of copyright). */
const QUOTES: [string, string][] = [
    ["I declare after all there is no enjoyment like reading!", "Jane Austen"],
    ["There is no frigate like a book to take us lands away.", "Emily Dickinson"],
    ["Some books are to be tasted, others to be swallowed, and some few to be chewed and digested.", "Francis Bacon"],
    ["Reading is to the mind what exercise is to the body.", "Richard Steele"],
    ["If you have a garden and a library, you have everything you need.", "Cicero"],
    ["Books are the quietest and most constant of friends.", "Charles W. Eliot"],
    ["The reading of all good books is like a conversation with the finest minds of past centuries.", "René Descartes"],
    ["A good book is the precious life-blood of a master spirit.", "John Milton"],
    ["Many a man has dated a new era in his life from the reading of a book.", "Henry David Thoreau"],
    ["Books are the treasured wealth of the world and the fit inheritance of generations and nations.", "Henry David Thoreau"],
    ["I cannot live without books.", "Thomas Jefferson"],
    ["When I get a little money I buy books; and if any is left I buy food and clothes.", "Erasmus"],
    ["No entertainment is so cheap as reading, nor any pleasure so lasting.", "Mary Wortley Montagu"],
    ["A house without books is like a room without windows.", "Horace Mann"],
    ["In books lies the soul of the whole past time.", "Thomas Carlyle"],
    ["Reading maketh a full man; conference a ready man; and writing an exact man.", "Francis Bacon"],
    ["'Tis the good reader that makes the good book.", "Ralph Waldo Emerson"],
    ["One must be an inventor to read well.", "Ralph Waldo Emerson"],
    ["Literature is my Utopia.", "Helen Keller"],
    ["Wear the old coat and buy the new book.", "Austin Phelps"],
    ["The best of a book is not the thought which it contains, but the thought which it suggests.", "Oliver Wendell Holmes Sr."],
    ["Except a living man, there is nothing more wonderful than a book.", "Charles Kingsley"],
    ["Books are the bees which carry the quickening pollen from one to another mind.", "James Russell Lowell"],
    ["It is a great thing to start life with a small number of really good books which are your very own.", "Arthur Conan Doyle"],
    ["A book must be the axe for the frozen sea within us.", "Franz Kafka"],
    ["Books, the children of the brain.", "Jonathan Swift"],
];

function showGreeting(): void {
    const name = topBarMe()?.username ?? marks.user;
    byId("greeting-hello").textContent = name ? `Welcome, ${name}!` : "Welcome!";
    showAccountLine();
}

function setupGreeting(): void {
    // The same quote all day (the visitor's day), the next one tomorrow.
    const day = Math.floor((Date.now() - new Date().getTimezoneOffset() * 60_000) / 86_400_000);
    const [text, who] = QUOTES[day % QUOTES.length]!;
    const fig = byId("greeting-quote");
    fig.querySelector("p")!.textContent = `“${text}”`;
    fig.querySelector("figcaption")!.textContent = `— ${who}`;
    fig.hidden = false;
    showGreeting();
    document.addEventListener("cf:me", showGreeting);
}

/* ------------------------------------------------------------------ */
/* Start                                                               */
/* ------------------------------------------------------------------ */

function openFromHash(): void {
    // As the grid does: the hash-less URL is the base entry, the popup one on top.
    const hash = window.location.hash;
    if (!hash.startsWith(HASH_PREFIX)) return;
    history.replaceState(history.state, "", window.location.pathname + window.location.search);
    const url = new URL(window.location.href);
    url.hash = hash;
    history.pushState({ ...history.state, modal: true }, "", url);
    loadBook(hash.substring(HASH_PREFIX.length));
}

setupCoverStates(root);
setupDragScroll(root, ".row-books");
setupPageDragScroll(root); // after the rows: on a row, the direction decides which
setupGreeting();
setupShelves();
setupTagSteps();
setupRecommended();
if (!cachedHome() && signedIn() !== true) ({ layout, hiddenTags } = guestLayout());
applyLayout(); // the sections in place before they fill
const homeIn = setupHome();
setupTagRows(); // after the cached home answer: pinned tags aren't picked
openFromHash();
const empty = maybeId("discover-empty");
if (empty) empty.hidden = totalBooks > 0;
void latestRow();
// Shuffle skips finished books: known now (cached marks, or this device's
// for a signed-out visitor), or on their way (first visit on this browser).
void (signedIn() !== false && !marks.signedIn ? homeIn : Promise.resolve()).then(shuffleRow);
