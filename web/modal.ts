import "./user.js"; // favorite / status / shelves controls in the book popup
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

function modalEl(): HTMLElement | null {
    return document.getElementById(MODAL_ID);
}
function bodyEl(): HTMLElement | null {
    return document.getElementById(BODY_ID);
}
function isModalOpen(): boolean {
    return modalEl()?.classList.contains("open") ?? false;
}
function setBody(html: string): void {
    const body = bodyEl();
    if (body) body.innerHTML = html;
}

/** htmx error events carry the element the request was for. */
type HtmxErrorEvent = CustomEvent<{ target?: Element | null }>;

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

const LOADING_HTML = '<p class="modal-loading">Loading...</p>';
let busy = false;

/**
 * Tell the grid (app-htmx.ts) to stop loading covers while the modal loads,
 * so on slow connections the book and its cover get all the bandwidth.
 */
function setBusy(b: boolean): void {
    if (b === busy) return;
    busy = b;
    document.dispatchEvent(new CustomEvent<boolean>("cf:modal-busy", { detail: b }));
}

/** Busy ends once the book has arrived and every image in it has loaded (or failed). */
function checkIdle(): void {
    const body = bodyEl();
    if (!busy || !body || body.querySelector(".modal-loading")) return;
    const pending = [...body.querySelectorAll("img")].filter((img) => !img.complete);
    if (pending.length === 0) {
        setBusy(false);
        return;
    }
    for (const img of pending) {
        img.addEventListener("load", checkIdle, { once: true });
        img.addEventListener("error", checkIdle, { once: true });
    }
}

function showModal(): void {
    setBody(LOADING_HTML);
    setBusy(true);
    // Hiding the scrollbar would widen the page: keep its space if there was one.
    const root = document.documentElement;
    root.classList.toggle("keep-gutter", root.scrollHeight > root.clientHeight);
    document.body.classList.add("no-scroll");
    modalEl()?.classList.add("open");
}

function hideModal(): void {
    document.body.classList.remove("no-scroll");
    document.documentElement.classList.remove("keep-gutter");
    modalEl()?.classList.remove("open");
    const body = bodyEl();
    if (body) body.innerHTML = "";
    document.title = BASE_TITLE; // every close path ends here
    shownUuid = null;
    setBusy(false);
    // The URL no longer has #book/: the grid may write its page now (app-htmx.ts).
    document.dispatchEvent(new Event("cf:modal-closed"));
}

export function loadBook(uuid: string): void {
    shownUuid = uuid;
    showModal();
    if (typeof htmx === "undefined") {
        setBody("htmx not loaded.");
        return;
    }
    htmx.ajax("GET", `/book/get/${encodeURIComponent(uuid)}`, {
        target: "#" + BODY_ID,
        swap: "innerHTML",
    });
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
        if (hashUuid()) openedViaNav = true; // reached by back/forward: there's an entry behind it
        syncFromHash();
    });

    const body = bodyEl();
    if (body) new MutationObserver(checkIdle).observe(body, { childList: true, subtree: true });

    // Close: X button or backdrop click
    const modal = modalEl();
    modal?.addEventListener("click", (e) => {
        if (e.target === modal || (e.target instanceof Element && e.target.closest(CLOSE_SELECTOR))) {
            e.preventDefault();
            closeModal();
        }
    });

    // Close: Escape
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") closeModal();
    });

    // Failed fetch
    for (const type of ["htmx:responseError", "htmx:sendError"]) {
        document.body.addEventListener(type, (e) => {
            if ((e as HtmxErrorEvent).detail?.target?.id === BODY_ID) setBody("Failed to load book.");
        });
    }

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
