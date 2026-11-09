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
let openedViaNav = false; // true if the current modal entry was pushed by a click
// htmx restores history on popstate (state.htmx) by re-fetching the current
// URL. The modal is a pure overlay driven by the hash, so htmx history is
// unused: swallow popstate before htmx sees it. Registered at module load,
// in the capture phase, so it runs ahead of htmx's own handler.
window.addEventListener("popstate", (e) => {
    e.stopImmediatePropagation();
}, true);
function modalEl() {
    return document.getElementById(MODAL_ID);
}
function bodyEl() {
    return document.getElementById(BODY_ID);
}
function isModalOpen() {
    return modalEl()?.classList.contains("open") ?? false;
}
function setBody(html) {
    const body = bodyEl();
    if (body)
        body.innerHTML = html;
}
/* ------------------------------------------------------------------ */
/* Tab title                                                           */
/* ------------------------------------------------------------------ */
/**
 * The book fragment sets its own <title>, which htmx applies. Whatever sets
 * it, prefix it with the base title while the modal is open.
 */
function prefixTitle() {
    if (!isModalOpen())
        return;
    const t = document.title;
    if (!t || t.startsWith(BASE_TITLE))
        return; // already prefixed (or base)
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
function hashUuid() {
    if (!location.hash.startsWith(HASH_PREFIX))
        return null;
    const raw = location.hash.slice(HASH_PREFIX.length);
    if (!raw)
        return null;
    try {
        return decodeURIComponent(raw);
    }
    catch {
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
function setBusy(b) {
    if (b === busy)
        return;
    busy = b;
    document.dispatchEvent(new CustomEvent("cf:modal-busy", { detail: b }));
}
/** Busy ends once the book has arrived and every image in it has loaded (or failed). */
function checkIdle() {
    const body = bodyEl();
    if (!busy || !body || body.querySelector(".modal-loading"))
        return;
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
function showModal() {
    setBody(LOADING_HTML);
    setBusy(true);
    document.body.classList.add("no-scroll");
    modalEl()?.classList.add("open");
}
function hideModal() {
    document.body.classList.remove("no-scroll");
    modalEl()?.classList.remove("open");
    const body = bodyEl();
    if (body)
        body.innerHTML = "";
    document.title = BASE_TITLE; // every close path ends here
    setBusy(false);
}
export function loadBook(uuid) {
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
 * Sync modal state to the URL hash.
 */
function syncFromHash() {
    const uuid = hashUuid();
    if (uuid) {
        loadBook(uuid);
    }
    else if (isModalOpen()) {
        hideModal();
        openedViaNav = false;
    }
}
/**
 * Close the modal. Goes back if the click pushed an entry,
 * otherwise strips the hash in place (deep-link case).
 */
export function closeModal() {
    if (!isModalOpen())
        return;
    if (openedViaNav) {
        history.back(); // hashchange -> syncFromHash -> hideModal
    }
    else {
        history.replaceState(null, "", location.pathname + location.search);
        hideModal();
    }
}
/**
 * One-time modal setup. Safe to call more than once.
 * Expects #book-modal (overlay containing content) and #modal-body.
 * Close buttons: .modal-close or [data-modal-close].
 */
export function setupModal() {
    if (modalInitialized || !modalEl())
        return;
    modalInitialized = true;
    // Mark that the next hash change came from a card click
    document.addEventListener("click", (e) => {
        if (e.target instanceof Element && e.target.closest(".book-link"))
            openedViaNav = true;
    }, true);
    window.addEventListener("hashchange", syncFromHash);
    const body = bodyEl();
    if (body)
        new MutationObserver(checkIdle).observe(body, { childList: true, subtree: true });
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
        if (e.key === "Escape")
            closeModal();
    });
    // Failed fetch
    for (const type of ["htmx:responseError", "htmx:sendError"]) {
        document.body.addEventListener(type, (e) => {
            if (e.detail?.target?.id === BODY_ID)
                setBody("Failed to load book.");
        });
    }
    // Deep link on initial load
    if (hashUuid()) {
        openedViaNav = false;
        syncFromHash();
    }
}
/** Set up the modal and htmx preloading for a container of book cards. */
export function setupBookClickHandlers(container) {
    setupModal();
    if (typeof htmx !== "undefined")
        htmx.process(container); // Allow hx-preload
}
