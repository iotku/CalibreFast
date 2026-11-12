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
 *   - Recently added: the start of pages/page1.json.
 *   - The introduction's sign-in line, for signed-out visitors, and Hide.
 *
 * Covers use the same URLs (coverUrl) and cards (renderBookCard) as the
 * grid, so a cover cached there is cached here. They load on-screen first,
 * a few at a time (maxCovers).
 */

import { byId, maybeId } from "./dom.js";
import type { Book } from "./types.js";
import { coverUrl, renderBookCard, setupCoverStates } from "./utils.js";
import { setupBookClickHandlers, loadBook, HASH_PREFIX } from "./modal.js";
import { cachedMarks, coverAction, fetchMarks, markCard, type Marks } from "./reading.js";
import { forgetControls } from "./user.js";
import { maxCovers } from "./cover-limits.js";

const STRIP = 12; // books in a row
const CURSOR_KEY = "cf-shuffle"; // ShuffleState
const INTRO_KEY = "cf-intro-hidden";

const root = byId("discover");
const cfg = root.dataset;
const version = cfg["version"] ?? "";
const pageSize = Math.max(1, parseInt(cfg["pageSize"] ?? "", 10) || 60);
const totalBooks = parseInt(cfg["totalBooks"] ?? "", 10) || 0;
const shuffleTotal = parseInt(cfg["shuffleTotal"] ?? "", 10) || 0;

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
    const url = `${dir}page${n}.json?v=${encodeURIComponent(version)}`;
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

function fillRow(id: string, books: Book[]): void {
    const section = byId(id);
    const list = section.querySelector<HTMLElement>(".row-books");
    if (!list) return;
    list.innerHTML = books.map((b) => renderBookCard(b, loadedCovers.has(coverUrl(b)))).join("");
    for (const card of list.querySelectorAll<HTMLElement>(".book")) markCard(card, marks);
    setupBookClickHandlers(list);
    section.hidden = books.length === 0;
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
    maybeId("shuffle-next")?.addEventListener("click", () => {
        if (!shuffling) void showStrip(shuffle.next, true);
    });
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
/* Reading marks (reading.ts)                                          */
/* ------------------------------------------------------------------ */

function repaintMarks(only?: string): void {
    for (const card of root.querySelectorAll<HTMLElement>(".book")) {
        if (!only || card.dataset["uuid"] === only) markCard(card, marks);
    }
}

async function refreshMarks(): Promise<void> {
    const fresh = await fetchMarks();
    if (!fresh) return;
    marks = fresh;
    repaintMarks();
    showAccountLine();
}

/** Fetches the marks now; resolves once they're in. */
function setupMarks(): Promise<void> {
    const fresh = refreshMarks();
    document.addEventListener("cf:status", () => void refreshMarks());
    window.addEventListener("pageshow", (e) => {
        if (e.persisted) void refreshMarks();
    });
    // The buttons over a card's cover; "continue reading" is a plain link.
    document.addEventListener("click", (e) => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".cover-actions button") : null;
        const uuid = btn?.closest<HTMLElement>(".book")?.dataset["uuid"];
        if (!btn || !uuid) return;
        e.preventDefault();
        void coverAction(btn.dataset["action"] ?? "", uuid, marks, () => repaintMarks(uuid)).then(() => {
            forgetControls(uuid);
            void refreshMarks();
        });
    });
    return fresh;
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
const marksIn = setupMarks();
setupIntro();
openFromHash();
const empty = maybeId("discover-empty");
if (empty) empty.hidden = totalBooks > 0;
void latestRow();
// Shuffle skips finished books: a signed-in user's are in the cached marks,
// or (first visit on this browser) on their way.
void (signedIn() && !marks.signedIn ? marksIn : Promise.resolve()).then(shuffleRow);
