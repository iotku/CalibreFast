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
 *     visitor, the featured ones (pages/discover/featured.json).
 *   - Recommended for you (signed in): from /api/me/home, next in series
 *     first, each with what it follows or is like.
 *   - More in <tag>: a few tag rows; the signed-in user's strongest tags
 *     first, then picked at random from the index in the page (weighted by
 *     size; tags an admin included weighted up), each filled from
 *     /api/tag-strip as it nears the screen and walked a strip at a time
 *     with its own Shuffle and Previous, like the Shuffle row.
 *   - Recently added: the start of pages/page1.json.
 *   - The introduction's sign-in line, for signed-out visitors, and Hide.
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
import { setupDragScroll } from "./drag-scroll.js";

const STRIP = 12; // books in a row
const CURSOR_KEY = "cf-shuffle"; // ShuffleState
const INTRO_KEY = "cf-intro-hidden";

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
    fillRow("row-latest", (await pageFile("/pages/", 1)).slice(0, STRIP));
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
    showShelves(h.shelves ?? [], true);
    showRecommended(h.recommended ?? []);
    affinityTags = (h.tags ?? []).map((t) => t.toLowerCase());
    // Something to go back to: the introduction steps aside.
    byId("intro").classList.toggle("compact", h.reading.length > 0 || !!store.get(INTRO_KEY));
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
    byId("row-wanted").hidden = true;
    byId("row-recommended").hidden = true;
    byId("row-reading-device").hidden = false;
    fillRow("row-reading", books);
    const shelves = await featured();
    if (!serverSignedIn) showShelves(shelves, false);
    // The introduction stays open for a guest (only Hide folds it): its
    // sign-in line is how this reading would follow them to other devices.
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
            // Want to read has a row of its own; a favorite is only the mark.
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

let reordering: Reorderable | null = null;

/**
 * A pinned tag's books as first shown on this page. The server starts a
 * tag's row at a random place on each answer, so the page differs on every
 * load; the answers that come after (a mark changed, back to the tab) keep
 * the books the row already has instead of swapping them under the user.
 */
const pinnedTagBooks = new Map<string, Book[]>();

/**
 * The shelf rows. own: the signed-in user's, with Remove and Reorder.
 * Empty shelves are left out.
 */
function showShelves(shelves: HomeShelf[], own: boolean): void {
    if (reordering) return; // being moved: the list stays as the user has it
    const list = byId("pin-list");
    list.replaceChildren();
    pinnedTags = new Set(shelves.filter((s) => s.kind === "tag").map((s) => s.name.toLowerCase()));
    shelves.forEach((s, i) => {
        const tag = s.kind === "tag";
        if (tag) {
            const key = s.name.toLowerCase();
            if (!pinnedTagBooks.has(key)) pinnedTagBooks.set(key, s.books);
            s = { ...s, books: pinnedTagBooks.get(key)! };
        }
        if (!s.books.length) return;
        const section = document.createElement("section");
        section.className = "row pin-row";
        // "s:7" a shelf, "t:Space Opera" a tag (the order sent back says which).
        section.dataset["id"] = tag ? `t:${s.name}` : `s:${s.id}`;
        section.dataset["name"] = s.name;
        section.setAttribute("aria-labelledby", `pin-${i}-title`);
        const name = escapeHtml(s.name);
        section.innerHTML = `
            <header class="row-head">
                <h2 id="pin-${i}-title"><a class="row-title-link" href="${escapeHtml(s.href)}">${name}</a>${tag ? '<span class="row-sub"> tag</span>' : ""}</h2>
                <div class="row-tools">
                    <a href="${escapeHtml(s.href)}" class="row-all">See all</a>
                    ${
                        own
                            ? `<details class="row-menu">
                        <summary class="row-btn" aria-label="Options for ${name}">
                            <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="19" cy="12" r="2"/></svg>
                        </summary>
                        <div class="row-menu-list">
                            <button type="button" data-act="unpin">Remove from home</button>
                            <button type="button" data-act="reorder">Reorder rows</button>
                        </div>
                    </details>`
                            : ""
                    }
                </div>
            </header>
            <div class="row-books"></div>`;
        list.append(section);
        fillRow(section, s.books, { all: true });
    });
    pinControls(own);
    updateTagRows(own);
}

/** The user's strongest tags (lower case): their rows come first. */
let affinityTags: string[] = [];

/** Recommended for you, each card saying why. */
function showRecommended(recs: RecBook[]): void {
    const section = byId("row-recommended");
    fillRow(section, recs.map((r) => r.book), { hint: true });
    recs.forEach((r, i) => {
        const card = section.querySelectorAll<HTMLElement>(".row-books > .book")[i];
        const info = card?.querySelector(".book-info");
        if (!info || !r.because) return;
        const why = document.createElement("div");
        why.className = "book-because";
        why.textContent = r.kind === "series" ? `Next after ${r.because}` : `Like ${r.because}`;
        info.append(why);
    });
}

/** Pinned tags (lower case): their "More in" rows step aside. */
let pinnedTags = new Set<string>();

/**
 * Reordering needs two rows or more: with fewer, neither the button under
 * the rows nor the menus' Reorder shelves is offered.
 */
function pinControls(own: boolean): void {
    const list = byId("pin-list");
    const n = list.children.length;
    byId("pin-rows").hidden = n === 0;
    byId("pin-foot").hidden = !own || n < 2;
    for (const item of list.querySelectorAll<HTMLElement>('.row-menu [data-act="reorder"]')) item.hidden = n < 2;
}

function setReordering(on: boolean): void {
    const btn = byId("pins-edit");
    const list = byId("pin-list");
    if (on === !!reordering) return;
    if (on) {
        for (const m of list.querySelectorAll<HTMLDetailsElement>(".row-menu")) m.open = false;
        reordering = makeReorderable({
            list,
            items: ".pin-row",
            label: (el) => el.dataset["name"] ?? "",
            slot: (el) => el.querySelector<HTMLElement>(".row-head")!,
            onChange: (ids) =>
                void api("PUT", "/api/me/pins", {
                    rows: ids.map((id) => (id.startsWith("t:") ? { tag: id.slice(2) } : { shelf: Number(id.slice(2)) })),
                }).catch(() => {}),
        });
    } else {
        reordering?.stop();
        reordering = null;
        void refreshHome();
    }
    btn.setAttribute("aria-pressed", String(on));
    btn.querySelector("span")!.textContent = on ? "Done" : "Reorder rows";
    pumpSoon();
}

function setupShelves(): void {
    byId("pins-edit").addEventListener("click", () => setReordering(!reordering));
    const list = byId("pin-list");
    list.addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".row-menu [data-act]") : null;
        const row = btn?.closest<HTMLElement>(".pin-row");
        if (!btn || !row) return;
        btn.closest("details")!.open = false;
        if (btn.dataset["act"] === "reorder") {
            setReordering(true);
            byId("pins-edit").scrollIntoView({ block: "nearest" });
            return;
        }
        // Remove from home: at once here; the server's answer follows.
        row.remove();
        pinControls(true);
        const id = row.dataset["id"] ?? "";
        const path = id.startsWith("t:") ? `/api/me/tag-pins/${encodeURIComponent(id.slice(2))}` : `/api/me/pins/${id.slice(2)}`;
        void api("DELETE", path)
            .catch(() => {})
            .then(() => refreshHome());
    });
    // A menu closes when the click is elsewhere.
    document.addEventListener("click", (e) => {
        for (const m of list.querySelectorAll<HTMLDetailsElement>(".row-menu[open]")) {
            if (!(e.target instanceof Node && m.contains(e.target))) m.open = false;
        }
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

/** The strip of the tag's books from start (null: a random place). */
function tagStrip(name: string, start: number | null): Promise<TagStrip | null> {
    const key = `${name.toLowerCase()}\n${start}`;
    let p = start === null ? undefined : tagStrips.get(key);
    if (!p) {
        const at = start === null ? "" : `&start=${start}`;
        p = fetch(`/api/tag-strip?tag=${encodeURIComponent(name)}&n=${STRIP}${at}`, { credentials: "same-origin" })
            .then((r) => (r.ok ? (r.json() as Promise<TagStrip>) : null))
            .catch(() => null);
        if (start !== null) {
            tagStrips.set(key, p);
            void p.then((s) => s ?? tagStrips.delete(key)); // a failure is tried again
        }
    }
    return p;
}

/** A tag row's Shuffle and Previous. */
interface TagWalker {
    row: TagRow;
    cur: TagCursor;
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
        const s = await tagStrip(w.row.name, start);
        if (!s) return false;
        // The server leaves out what the user finished; this also leaves
        // out what was finished on this device.
        const books = s.books.filter((b) => !marks.finished.has(b.uuid));
        if (!books.length && section.querySelector(".row-books .book")) return false;
        if (push) w.cur.starts = [...w.cur.starts, s.start].slice(-SHUFFLE_HISTORY);
        w.cur.next = s.next;
        saveTagCursor(w.row.name.toLowerCase(), w.cur);
        if (books.length) fillRow(section, books, { all: true });
        return books.length > 0;
    } finally {
        w.busy = false;
        updateTagButtons(section, w);
    }
}

function setupTagRows(): void {
    const box = byId("tag-rows");
    let index: TagRow[] = [];
    try {
        index = JSON.parse(box.dataset["tags"] ?? "[]") as TagRow[];
    } catch {
        /* none */
    }
    if (!index.length) return;
    const cursors = loadTagCursors();
    // Every tag in a random order (bigger ones sooner). Each row takes the
    // next one as it nears the screen, by when the user's pinned tags are
    // usually known: a pinned tag has its own row, so it's skipped.
    const queue = pickTagRows(index, index.length);
    const fill = async (section: HTMLElement, i: number): Promise<void> => {
        // The next tag that isn't pinned and has books the user hasn't finished.
        for (;;) {
            const free = (t: TagRow): boolean => !pinnedTags.has(t.name.toLowerCase());
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
            const w: TagWalker = { row, cur: cursors.get(row.name.toLowerCase()) ?? { starts: [], next: null }, busy: false };
            const name = escapeHtml(row.name);
            const href = escapeHtml(row.href);
            section.dataset["name"] = row.name;
            section.querySelector("header")!.innerHTML = `
            <h2 id="tag-row-${i}-title">More in <a class="row-title-link" href="${href}">${name}</a></h2>
            <div class="row-tools">
                <button type="button" class="row-btn" data-act="tag-back" aria-label="Previous books in ${name}" disabled>
                    <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M9 14L4 9l5-5"/><path d="M4 9h11a5 5 0 0 1 0 10h-3"/></svg>
                    <span>Previous</span>
                </button>
                <button type="button" class="row-btn" data-act="tag-next" aria-label="More books in ${name}">
                    <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M16 3h5v5M4 20L21 3M21 16v5h-5M15 15l6 6M4 4l5 5"/></svg>
                    <span>Shuffle</span>
                </button>
                <a href="${href}" class="row-all">See all</a>
                <details class="row-menu tag-menu" hidden>
                    <summary class="row-btn" aria-label="Options for ${name}">
                        <svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor" aria-hidden="true"><circle cx="5" cy="12" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="19" cy="12" r="2"/></svg>
                    </summary>
                    <div class="row-menu-list">
                        <button type="button" data-act="pin-tag">Pin to home</button>
                    </div>
                </details>
            </div>`;
            // A tag that fits in one strip shows all its books already.
            for (const b of section.querySelectorAll<HTMLElement>('[data-act^="tag-"]')) b.hidden = row.count <= STRIP;
            tagWalkers.set(section, w);
            if (await showTagStrip(section, w, w.cur.next, true)) break;
        }
        updateTagRows(serverSignedIn ?? signedIn() === true);
        box.hidden = !box.querySelector(".row:not([hidden])");
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
        box.append(section);
        near.observe(section);
    }
    updateTagRows(signedIn() === true);
    box.addEventListener("click", (e) => {
        const step = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="tag-next"], [data-act="tag-back"]') : null;
        const stepRow = step?.closest<HTMLElement>(".tag-row");
        const w = stepRow && tagWalkers.get(stepRow);
        if (step && stepRow && w && !w.busy) {
            if (step.dataset["act"] === "tag-next") {
                void showTagStrip(stepRow, w, w.cur.next, true).then(() => {
                    // Still on the button: get the strip after this one ready too.
                    if (step.matches(":hover, :focus-visible")) preloadTagStrip(w);
                });
            } else if (w.cur.starts.length >= 2) {
                w.cur.starts = w.cur.starts.slice(0, -1);
                void showTagStrip(stepRow, w, w.cur.starts[w.cur.starts.length - 1]!, false);
            }
            return;
        }
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>('[data-act="pin-tag"]') : null;
        const section = btn?.closest<HTMLElement>(".tag-row");
        const name = section?.dataset["name"];
        if (!btn || !section || !name) return;
        btn.closest("details")!.open = false;
        // Pinned: it moves up among the pinned rows (from the server's answer).
        pinnedTags.add(name.toLowerCase());
        updateTagRows(true);
        void api("PUT", `/api/me/tag-pins/${encodeURIComponent(name)}`)
            .catch(() => {})
            .then(() => refreshHome());
    });
    // About to shuffle: fetch the next strip's covers now, so they show at once.
    const aboutTo = (e: Event): void => {
        const btn = e.target instanceof Element ? e.target.closest('[data-act="tag-next"]') : null;
        const section = btn?.closest<HTMLElement>(".tag-row");
        const w = section && tagWalkers.get(section);
        if (w) preloadTagStrip(w);
    };
    box.addEventListener("pointerover", aboutTo);
    box.addEventListener("focusin", aboutTo);
    // A menu closes when the click is elsewhere.
    document.addEventListener("click", (e) => {
        for (const m of box.querySelectorAll<HTMLDetailsElement>(".row-menu[open]")) {
            if (!(e.target instanceof Node && m.contains(e.target))) m.open = false;
        }
    });
}

/** The covers of a tag row's next strip, into the browser's cache. */
function preloadTagStrip(w: TagWalker): void {
    if (w.busy || w.cur.next === null) return;
    void tagStrip(w.row.name, w.cur.next).then((s) => {
        for (const b of s?.books ?? []) preloadCover(coverUrl(b));
    });
}

/**
 * "More in" rows: Pin to home for a signed-in user; a tag already pinned
 * has its own row up with the pinned ones, so its "More in" row steps aside.
 */
function updateTagRows(own: boolean): void {
    const box = maybeId("tag-rows");
    if (!box) return;
    for (const section of box.querySelectorAll<HTMLElement>(".tag-row")) {
        const menu = section.querySelector<HTMLElement>(".tag-menu");
        if (menu) menu.hidden = !own;
        const pinned = pinnedTags.has((section.dataset["name"] ?? "").toLowerCase());
        section.classList.toggle("pinned-away", pinned);
    }
}

/* ------------------------------------------------------------------ */
/* Introduction                                                        */
/* ------------------------------------------------------------------ */

/**
 * Who's signed in, as the top bar last saw it (nav.ts keeps its answer in
 * cf-me and says when it changes): true, false, or null while unknown.
 */
function signedIn(): boolean | null {
    if (marks.signedIn) return true;
    try {
        const me = store.get("cf-me");
        return me === null ? null : !!(JSON.parse(me) as { username?: string | null }).username;
    } catch {
        return null;
    }
}

/** The sign-in line: for signed-out visitors only; hidden until that's known. */
function showAccountLine(): void {
    const line = maybeId("intro-account");
    if (line) line.hidden = signedIn() !== false;
}

function setupIntro(): void {
    const intro = byId("intro");
    if (store.get(INTRO_KEY)) intro.classList.add("compact");
    maybeId("intro-dismiss")?.addEventListener("click", () => {
        intro.classList.add("compact");
        store.set(INTRO_KEY, "1");
    });
    showAccountLine();
    document.addEventListener("cf:me", showAccountLine);
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
setupIntro();
setupShelves();
const homeIn = setupHome();
setupTagRows(); // after the cached home answer: pinned tags aren't picked
openFromHash();
const empty = maybeId("discover-empty");
if (empty) empty.hidden = totalBooks > 0;
void latestRow();
// Shuffle skips finished books: known now (cached marks, or this device's
// for a signed-out visitor), or on their way (first visit on this browser).
void (signedIn() !== false && !marks.signedIn ? homeIn : Promise.resolve()).then(shuffleRow);
