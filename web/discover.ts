/**
 * The Discover page ("/", templates/discover.html; docs/notes/Discover.md).
 *
 * The page is the same for everyone; this fills in its rows from the static
 * page files (cached for good under ?v=) and what's personal:
 *   - Shuffle: the next strip of the server's random order
 *     (pages/shuffle/), walked a strip per load so the same books don't
 *     come back until the whole library has gone by; books the user
 *     finished are skipped.
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
const CURSOR_KEY = "cf-shuffle"; // {version, at}: where the next Shuffle strip starts
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

/** Where the next Shuffle strip starts: saved, or a random place in a new order. */
function shuffleCursor(): number {
    try {
        const c = JSON.parse(store.get(CURSOR_KEY) ?? "null") as { version?: string; at?: number } | null;
        if (c?.version === version && Number.isInteger(c.at) && c.at! >= 0 && c.at! < shuffleTotal) return c.at!;
    } catch {
        /* unreadable: start somewhere new */
    }
    // A restart made a new order (or first visit): a random place, so
    // visitors don't all start on the same strip.
    return Math.floor(Math.random() * shuffleTotal);
}

/**
 * The next STRIP books of the random order from the cursor, skipping
 * finished ones, wrapping at the end; the cursor moves past them. Books only
 * repeat once the whole order has gone by (a library smaller than two
 * strips can't avoid some).
 */
async function shuffleRow(): Promise<void> {
    if (!shuffleTotal) return;
    const start = shuffleCursor();
    const out: Book[] = [];
    let i = start;
    for (let scanned = 0; out.length < STRIP && scanned < shuffleTotal; scanned++) {
        const page = Math.floor(i / pageSize) + 1;
        const book = (await pageFile("/pages/shuffle/", page))[i - (page - 1) * pageSize];
        if (book && !marks.finished.has(book.uuid)) out.push(book);
        i = (i + 1) % shuffleTotal;
    }
    // Went all the way round (a small library): start the next one further
    // on anyway, so it isn't the same strip in the same order.
    if (i === start) i = (start + Math.max(1, Math.floor(out.length / 2))) % shuffleTotal;
    store.set(CURSOR_KEY, JSON.stringify({ version, at: i }));
    fillRow("row-shuffle", out);
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
