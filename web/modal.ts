import { forgetControls, prefetchControls } from "./user.js"; // favorite / status / shelves controls in the book popup
import { el, icon, lineIcon } from "./dom.js";
import { parseServerHtml } from "./trusted.js";
import "./htmx-security.js"; // htmx swaps under Trusted Types
/* ------------------------------------------------------------------ */
/* Modal (hash-driven)                                                 */
/* ------------------------------------------------------------------ */

const MODAL_ID = "book-modal";
const BODY_ID = "modal-body";
export const HASH_PREFIX = "#book/";
const CLOSE_SELECTOR = "#modal-close, [data-modal-close]";

// Rendered from {{ .Title }} in index.html, so it follows a service rename.
const BASE_TITLE = document.title;
const TITLE_SEP = " – ";

let modalInitialized = false;
let openedViaNav = false; // true if there's an entry to go back to (click or back/forward)
let shownUuid: string | null = null; // book in the open modal
let shownCard: HTMLElement | null = null; // its card (.book) on the page, for the books either side

function modalEl(): HTMLElement | null {
    return document.getElementById(MODAL_ID);
}
function bodyEl(): HTMLElement | null {
    return document.getElementById(BODY_ID);
}
function isModalOpen(): boolean {
    return modalEl()?.classList.contains("open") ?? false;
}
/** Show a message in place of the book. */
function setBodyText(text: string): void {
    bodyEl()?.replaceChildren(text);
}

/** htmx 4's request events carry the request's context. */
type HtmxCtxEvent = CustomEvent<{
    ctx?: { target?: Element | null; status?: string; request?: { action?: string }; response?: { status: number } };
}>;

/* ------------------------------------------------------------------ */
/* Tab title                                                           */
/* ------------------------------------------------------------------ */

/**
 * The book fragment sets its own <title>, which htmx applies. Whatever sets
 * it, prefix it with the base title while the modal is open.
 */
function prefixTitle(): void {
    if (!isModalOpen()) return;
    const t = document.title;
    if (!t || t.startsWith(BASE_TITLE)) return; // already prefixed (or base)
    document.title = BASE_TITLE + TITLE_SEP + t;
}

const titleEl = document.querySelector("title");
if (titleEl) {
    new MutationObserver(prefixTitle).observe(titleEl, {
        childList: true,
        characterData: true,
        subtree: true,
    });
}

/* ------------------------------------------------------------------ */

function hashUuid(): string | null {
    if (!location.hash.startsWith(HASH_PREFIX)) return null;
    const raw = location.hash.slice(HASH_PREFIX.length);
    if (!raw) return null;
    try {
        return decodeURIComponent(raw);
    } catch {
        return raw;
    }
}

/* ------------------------------------------------------------------ */
/* Busy: grid covers pause while the modal loads                       */
/* ------------------------------------------------------------------ */

/** Three bouncing dots. */
function throbber(): HTMLElement {
    const dot = () => el("span", { className: "throbber-dot" });
    const t = el("span", { className: "throbber" }, dot(), dot(), dot());
    t.setAttribute("aria-hidden", "true");
    return t;
}

/** The popup's throbber while a book loads. */
function loadingBox(): HTMLElement {
    const box = el("div", { className: "modal-loading" }, throbber());
    box.setAttribute("role", "status");
    box.setAttribute("aria-label", "Loading");
    return box;
}
/** A book that arrives sooner shows without a loading state in between. */
const LOADING_DELAY_MS = 100;
let loadingUrl: string | null = null; // the book request whose answer the modal waits for
let loadingTimer: ReturnType<typeof setTimeout> | undefined;
/** The longest the grid waits for the modal; it then loads covers anyway. */
const BUSY_MAX_MS = 6000;
let busy = false;
let busyTimer: ReturnType<typeof setTimeout> | undefined;

/**
 * Tell the grid (app-htmx.ts) to stop loading covers while the modal loads,
 * so on slow connections the book and its cover get all the bandwidth.
 */
function setBusy(b: boolean): void {
    clearTimeout(busyTimer);
    if (b) busyTimer = setTimeout(() => setBusy(false), BUSY_MAX_MS); // a cover that never settles mustn't hold the grid
    if (b === busy) return;
    busy = b;
    document.dispatchEvent(new CustomEvent<boolean>("cf:modal-busy", { detail: b }));
}

/**
 * Busy ends once the book has arrived and its cover images (thumbnail, full
 * cover) have loaded or failed. Other images (e.g. in a description) don't
 * count: one that never loads would hold the grid's covers back.
 */
function checkIdle(): void {
    const body = bodyEl();
    if (!busy || !body || loadingUrl !== null) return;
    const pending = [...body.querySelectorAll<HTMLImageElement>(".cover-stack img")].filter((img) => !img.complete);
    if (pending.length === 0) {
        setBusy(false);
        prefetchNeighbours();
        return;
    }
    for (const img of pending) {
        img.addEventListener("load", checkIdle, { once: true });
        img.addEventListener("error", checkIdle, { once: true });
    }
}

/** Shown over a book while the next one loads (a switch from one to the next). */
let switchingEl: HTMLElement | null = null;

/** Whether the modal shows a book (not a throbber or an error), to keep while the next loads. */
const showsBook = (): boolean => !!bodyEl()?.querySelector(":scope > .book-page");

/**
 * Open the modal for a book that's loading. A new modal stays invisible
 * (.pending) until the book arrives or LOADING_DELAY_MS passes, when the
 * throbber shows. A switch from another book keeps that book showing,
 * dimmed after LOADING_DELAY_MS: the cover and the arrows beside it stay
 * where they are, so a run of taps on an arrow keeps hitting it.
 */
function showModal(): void {
    setBusy(true);
    clearTimeout(loadingTimer);
    const keep = isModalOpen() && showsBook();
    loadingTimer = setTimeout(() => {
        modalEl()?.classList.remove("pending");
        if (loadingUrl === null) return;
        if (keep) setSwitching(true);
        else bodyEl()?.replaceChildren(loadingBox());
    }, LOADING_DELAY_MS);
    bodyEl()?.setAttribute("aria-busy", "true");
    if (isModalOpen()) return;
    bodyEl()?.replaceChildren();
    // Hiding the scrollbar would widen the page: keep its space if there was one.
    const root = document.documentElement;
    root.classList.toggle("keep-gutter", root.scrollHeight > root.clientHeight);
    document.body.classList.add("no-scroll");
    modalEl()?.classList.add("open", "pending");
}

function setSwitching(on: boolean): void {
    modalEl()?.classList.toggle("switching", on);
    if (switchingEl) switchingEl.hidden = !on;
}

/** The book (or its error) is in the modal: show it, and get the books either side ready. */
function doneLoading(): void {
    if (loadingUrl === null) return;
    loadingUrl = null;
    clearTimeout(loadingTimer);
    modalEl()?.classList.remove("pending");
    setSwitching(false);
    bodyEl()?.removeAttribute("aria-busy");
    updateNavButtons();
    checkIdle();
}

/**
 * The modal's body changed. A book that just came in gets the arrows and
 * starts at the top here, before it's drawn (observer callbacks run before
 * the next frame), so it never shows a frame without them or scrolled.
 */
let freshSwap = false;
function bodyChanged(): void {
    if (freshSwap) {
        freshSwap = false;
        const content = modalEl()?.querySelector(".modal-content");
        if (content) content.scrollTop = 0;
        setSwitching(false);
        updateNavButtons();
    }
    checkIdle();
}

function hideModal(): void {
    document.body.classList.remove("no-scroll");
    document.documentElement.classList.remove("keep-gutter");
    modalEl()?.classList.remove("open", "pending");
    setSwitching(false);
    watchGrid(null);
    queuedStep = null;
    const body = bodyEl();
    if (body) {
        body.replaceChildren();
        body.removeAttribute("aria-busy");
    }
    document.title = BASE_TITLE; // every close path ends here
    shownUuid = null;
    shownCard = null;
    loadingUrl = null;
    clearTimeout(loadingTimer);
    setBusy(false);
    // The URL no longer has #book/: the grid may write its page now (app-htmx.ts).
    document.dispatchEvent(new Event("cf:modal-closed"));
}

const bookUrl = (uuid: string): string => `/book/get/${encodeURIComponent(uuid)}`;

/** Path and query of a request URL, as htmx may have resolved it. */
function samePath(a: string, b: string): boolean {
    const p = (u: string) => {
        const url = new URL(u, location.href);
        return url.pathname + url.search;
    };
    return p(a) === p(b);
}

export function loadBook(uuid: string): void {
    queuedStep = null; // another book asked for (back/forward, a link): drop a waiting step
    const card = cardFor(uuid);
    shownUuid = uuid;
    shownCard = card;
    if (typeof htmx === "undefined") {
        showModal();
        setBodyText("htmx not loaded.");
        return;
    }
    const url = bookUrl(uuid);
    loadingUrl = url;
    showModal();
    watchGrid(card);
    updateNavButtons(); // the arrows follow the book asked for, not the one still showing
    prefetchControls(uuid); // in parallel with the book, not after it
    const hit = takePrefetch(uuid);
    // Otherwise the book's card (if on the page) as the source: htmx then
    // uses the card's hx-preload response, even one still in flight.
    htmx.ajax("GET", url, {
        source: hit ? bodyEl() ?? undefined : card?.querySelector(".book-link") ?? undefined,
        target: "#" + BODY_ID,
        swap: "innerHTML",
        ...(hit ? { fetch: () => hit } : {}),
    }).catch(() => {
        if (loadingUrl === url) {
            setBodyText("Failed to load book.");
            doneLoading();
        }
    });
}

/* ------------------------------------------------------------------ */
/* The books either side: arrow keys, and their prefetch               */
/* ------------------------------------------------------------------ */

/** How long a prefetched book is used: like the controls' (user.ts). */
const BOOK_PREFETCH_TTL_MS = 30_000;
const prefetched = new Map<string, { res: Promise<Response>; expires: number }>();

/** The book's card: the one it was opened from, else the first on the page. */
function cardFor(uuid: string): HTMLElement | null {
    if (shownCard?.isConnected && shownCard.dataset["uuid"] === uuid) return shownCard;
    return document.querySelector<HTMLElement>(`.book[data-uuid="${CSS.escape(uuid)}"]`);
}

/**
 * The card next to the shown one (dir 1) or before it (-1), in its grid or
 * row: a book, or a skeleton whose page hasn't arrived yet. Null at the end.
 */
function adjacent(dir: 1 | -1): HTMLElement | null {
    const card = shownUuid ? cardFor(shownUuid) : null;
    for (let el = card ? sibling(card, dir) : null; el; el = sibling(el, dir)) {
        if (el.classList.contains("book")) return el; // not e.g. the grid's spacers
    }
    return null;
}

/** The book next to the shown one, if it's there yet (not a skeleton). */
function neighbour(dir: 1 | -1): HTMLElement | null {
    const el = adjacent(dir);
    return el?.dataset["uuid"] ? el : null;
}

/** A step asked for while that way was still a skeleton: taken once it's a book. */
let queuedStep: 1 | -1 | null = null;

const sibling = (el: Element, dir: 1 | -1): HTMLElement | null =>
    (dir > 0 ? el.nextElementSibling : el.previousElementSibling) as HTMLElement | null;

/**
 * The grid around the shown card fills in skeletons as their pages arrive
 * (app-htmx.ts): an arrow hidden because the next card was a skeleton
 * shows once it's a book.
 */
let gridObserver: MutationObserver | null = null;
let watchedGrid: Element | null = null;
function watchGrid(card: HTMLElement | null): void {
    const grid = card?.parentElement ?? null;
    if (grid === watchedGrid) return;
    gridObserver?.disconnect();
    watchedGrid = grid;
    if (!grid) return;
    gridObserver ??= new MutationObserver(() => {
        if (!isModalOpen()) return;
        if (queuedStep && neighbour(queuedStep)) {
            step(queuedStep);
            return;
        }
        updateNavButtons();
        if (loadingUrl === null && !busy) prefetchNeighbours();
    });
    gridObserver.observe(grid, { childList: true });
}

/** Fetch a book's popup ahead of time, with its cover images. */
function prefetchBook(uuid: string): void {
    const now = Date.now();
    for (const [k, v] of prefetched) if (v.expires <= now) prefetched.delete(k);
    if (prefetched.has(uuid)) return;
    const res = fetch(bookUrl(uuid), {
        headers: { "HX-Request": "true", "HX-Request-Type": "partial" },
        credentials: "same-origin",
    }).then((r) => {
        if (!r.ok) throw new Error(`${r.status}`);
        return r;
    });
    prefetched.set(uuid, { res, expires: now + BOOK_PREFETCH_TTL_MS });
    res.then((r) => r.clone().text())
        .then(preloadCovers)
        .catch(() => prefetched.delete(uuid));
}

/** The prefetched answer for a book, if fresh (each use gets its own copy). */
function takePrefetch(uuid: string): Promise<Response> | null {
    const hit = prefetched.get(uuid);
    if (!hit || hit.expires <= Date.now()) return null;
    return hit.res.then((r) => r.clone());
}

/** Have the browser fetch the popup's cover images (thumbnail and full cover). */
function preloadCovers(html: string): void {
    const doc = parseServerHtml(html); // inert: nothing loads or runs
    for (const img of doc.querySelectorAll<HTMLImageElement>(".cover-stack img")) {
        const src = img.getAttribute("src");
        if (!src) continue;
        const pre = new Image();
        pre.fetchPriority = "low";
        pre.src = src;
    }
}

/** Once the shown book has settled, get the ones either side ready. */
function prefetchNeighbours(): void {
    for (const dir of [1, -1] as const) {
        const uuid = neighbour(dir)?.dataset["uuid"];
        if (!uuid) continue;
        prefetchBook(uuid);
        prefetchControls(uuid);
    }
}

/* On-screen arrows and swipes: the same steps as the arrow keys. */

/** An on-screen arrow, hidden until there's a book that way. */
function navButton(dir: "prev" | "next"): HTMLButtonElement {
    const btn = el("button", { type: "button", className: `modal-nav modal-nav-${dir}`, hidden: true });
    btn.dataset["step"] = dir === "next" ? "1" : "-1";
    btn.setAttribute("aria-label", dir === "next" ? "Next book" : "Previous book");
    btn.append(icon(lineIcon(22, 2.2), dir === "next" ? "m9 6 6 6-6 6" : "m15 6-6 6 6 6"));
    return btn;
}

/** The throbber shown over the popup while it switches books. */
function switchingThrobber(): HTMLElement {
    const box = el("div", { className: "modal-switching", hidden: true }, throbber());
    box.setAttribute("aria-hidden", "true");
    return box;
}

let navButtons: HTMLButtonElement[] = [];

/**
 * Narrow screens (the popup fills the screen) have the arrows beside the
 * cover, in the book's own markup; the next book's markup replaces them,
 * so they're put back each time. Elsewhere they're in the margins beside
 * the popup. A short landscape screen lays the book out in columns, with
 * no room beside the cover.
 */
const narrow = matchMedia("(max-width: 768px)");
const shortLandscape = matchMedia("(max-height: 520px) and (orientation: landscape)");

/** Put the arrows where they go, and show each only when there's a book that way. */
function updateNavButtons(): void {
    const byCover = narrow.matches && !shortLandscape.matches;
    modalEl()?.classList.toggle("nav-by-cover", byCover);
    const cover = byCover ? bodyEl()?.querySelector(".book-page-cover") : null;
    const home = cover ?? modalEl();
    for (const btn of navButtons) {
        if (home && btn.parentElement !== home) home.append(btn);
        // A skeleton that way: the arrow stays (a tap waits for the book),
        // so a quick run of taps never finds it gone.
        const next = adjacent(btn.dataset["step"] === "1" ? 1 : -1);
        btn.hidden = !next;
        btn.classList.toggle("waiting", !!next && !next.dataset["uuid"]);
    }
}

/** A swipe: at least this far sideways, mostly sideways, and quick. */
const SWIPE_MIN_PX = 60;
const SWIPE_MAX_MS = 600;

/** Whether the touch began on something that scrolls sideways itself (a wide table in a description). */
function scrollsSideways(el: Element | null, stop: Element): boolean {
    for (; el && el !== stop; el = el.parentElement) {
        if (el.scrollWidth > el.clientWidth && /auto|scroll/.test(getComputedStyle(el).overflowX)) return true;
    }
    return false;
}

function setupSwipe(content: HTMLElement): void {
    let start: { x: number; y: number; t: number } | null = null;
    content.addEventListener(
        "touchstart",
        (e) => {
            const t = e.touches[0];
            const target = e.target instanceof Element ? e.target : null;
            start =
                e.touches.length === 1 && t && !scrollsSideways(target, content) && !target?.closest("input, textarea, select")
                    ? { x: t.clientX, y: t.clientY, t: e.timeStamp }
                    : null; // a pinch, or something with its own sideways scroll
        },
        { passive: true },
    );
    content.addEventListener(
        "touchend",
        (e) => {
            const t = e.changedTouches[0];
            const s = start;
            start = null;
            if (!s || !t || e.touches.length) return;
            const dx = t.clientX - s.x;
            const dy = t.clientY - s.y;
            if (Math.abs(dx) < SWIPE_MIN_PX || Math.abs(dx) < 2 * Math.abs(dy) || e.timeStamp - s.t > SWIPE_MAX_MS) return;
            if (!document.getSelection()?.isCollapsed) return; // selecting text
            step(dx < 0 ? 1 : -1); // swipe left: the next book
        },
        { passive: true },
    );
    content.addEventListener("touchcancel", () => (start = null), { passive: true });
}

/** Show the next or previous book, in place of this one in history. */
function step(dir: 1 | -1): void {
    queuedStep = null;
    const card = neighbour(dir);
    const uuid = card?.dataset["uuid"];
    if (!card || !uuid) {
        const pending = adjacent(dir);
        if (pending) {
            // Its page is on the way (the grid fetches around the shown card).
            queuedStep = dir;
            revealCard(pending);
            if (showsBook()) setSwitching(true);
        }
        return;
    }
    revealCard(card);
    // No hashchange or popstate: those mean back/forward to the listeners above.
    history.replaceState(history.state, "", location.pathname + location.search + HASH_PREFIX + encodeURIComponent(uuid));
    shownCard = card;
    loadBook(uuid);
}

/** Scroll the page (and a card row) so the card is on screen: there when the modal closes. */
function revealCard(card: HTMLElement): void {
    card.scrollIntoView({ block: "nearest", inline: "nearest", behavior: "instant" });
    const top = card.getBoundingClientRect().top;
    const bar = document.querySelector<HTMLElement>(".top-bar")?.offsetHeight ?? 0;
    if (top < bar) window.scrollBy({ top: top - bar - 8, behavior: "instant" });
}

/**
 * Sync modal state to the URL hash. Runs on popstate and hashchange, which
 * often both fire for one navigation: a book already shown isn't reloaded.
 */
function syncFromHash(): void {
    const uuid = hashUuid();
    if (uuid) {
        if (uuid !== shownUuid || !isModalOpen()) loadBook(uuid);
    } else if (isModalOpen()) {
        hideModal();
        openedViaNav = false;
    }
}

/**
 * Close the modal. Goes back if the click pushed an entry,
 * otherwise strips the hash in place (deep-link case).
 */
export function closeModal(): void {
    if (!isModalOpen()) return;
    if (openedViaNav) {
        history.back(); // popstate -> syncFromHash -> hideModal
    } else {
        history.replaceState(null, "", location.pathname + location.search);
        hideModal();
    }
}

/**
 * One-time modal setup. Safe to call more than once.
 * Expects #book-modal (overlay containing content) and #modal-body.
 * Close buttons: .modal-close or [data-modal-close].
 */
export function setupModal(): void {
    if (modalInitialized || !modalEl()) return;
    modalInitialized = true;

    // Mark that the next hash change came from a card click
    document.addEventListener(
        "click",
        (e) => {
            if (e.target instanceof Element && e.target.closest(".book-link")) openedViaNav = true;
        },
        true,
    );

    // The modal follows the URL. Back/forward between entries whose ?page
    // differs (the grid moved after the modal closed) isn't a hash change, so
    // popstate covers it. htmx history is off on these pages (htmx-config
    // meta): it would re-fetch the page and replace the body.
    window.addEventListener("hashchange", syncFromHash);
    window.addEventListener("popstate", () => {
        const uuid = hashUuid();
        if (uuid) {
            openedViaNav = true; // reached by back/forward: there's an entry behind it
            // Back to a book: its status and progress may have moved since
            // the controls were prefetched (read in between, another tab).
            if (uuid !== shownUuid || !isModalOpen()) forgetControls(uuid);
        }
        syncFromHash();
    });

    const body = bodyEl();
    if (body) new MutationObserver(bodyChanged).observe(body, { childList: true, subtree: true });

    // Close: X button or backdrop click. Previous/next: the arrows.
    const modal = modalEl();
    if (modal) {
        navButtons = [navButton("prev"), navButton("next")];
        switchingEl = switchingThrobber();
        modal.append(...navButtons, switchingEl);
    }
    for (const mq of [narrow, shortLandscape]) mq.addEventListener("change", updateNavButtons);
    modal?.addEventListener("click", (e) => {
        const nav = e.target instanceof Element ? e.target.closest<HTMLElement>(".modal-nav") : null;
        if (nav) {
            step(nav.dataset["step"] === "1" ? 1 : -1);
            return;
        }
        if (e.target === modal || (e.target instanceof Element && e.target.closest(CLOSE_SELECTOR))) {
            e.preventDefault();
            closeModal();
        }
    });
    const content = modal?.querySelector<HTMLElement>(".modal-content");
    if (content) setupSwipe(content);

    // Close: Escape. Left and right: the books either side.
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") closeModal();
        if ((e.key !== "ArrowLeft" && e.key !== "ArrowRight") || !isModalOpen()) return;
        if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return;
        if (e.target instanceof Element && e.target.closest("input, textarea, select, [contenteditable]")) return;
        e.preventDefault();
        step(e.key === "ArrowRight" ? 1 : -1);
    });

    // Only the book last asked for goes in: an earlier one still in flight
    // (a quick run of arrow presses) is dropped.
    document.addEventListener("htmx:before:swap", (e) => {
        const ctx = (e as HtmxCtxEvent).detail?.ctx;
        if (!ctx || ctx.target !== bodyEl()) return;
        if (loadingUrl === null || !samePath(ctx.request?.action ?? "", loadingUrl)) e.preventDefault();
        else freshSwap = true;
    });
    document.addEventListener("htmx:finally:request", (e) => {
        const ctx = (e as HtmxCtxEvent).detail?.ctx;
        if (!ctx || ctx.target !== bodyEl() || loadingUrl === null) return;
        if (!samePath(ctx.request?.action ?? "", loadingUrl)) return;
        if (ctx.status !== "swapped" || (ctx.response?.status ?? 0) >= 400) setBodyText("Failed to load book.");
        doneLoading();
    });

    // Deep link on initial load
    if (hashUuid()) {
        openedViaNav = false;
        syncFromHash();
    }
}

/** Set up the modal and htmx preloading for a container of book cards. */
export function setupBookClickHandlers(container: Element): void {
    setupModal();
    if (typeof htmx !== "undefined") htmx.process(container); // Allow hx-preload
}
