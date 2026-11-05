/* ------------------------------------------------------------------ */
/* Modal (hash-driven)                                                 */
/* ------------------------------------------------------------------ */

const MODAL_ID = "book-modal";
const BODY_ID = "modal-body";
export const HASH_PREFIX = "#book/";
const CLOSE_SELECTOR = ".modal-close, [data-modal-close]";

let modalInitialized = false;
let openedViaNav = false; // true if the current modal entry was pushed by a click

// htmx restores history on popstate (state.htmx) by re-fetching the current
// URL. The modal is a pure overlay driven by the hash, so htmx history is
// unused: swallow popstate before htmx sees it. Registered at module load,
// in the capture phase, so it runs ahead of htmx's own handler.
window.addEventListener(
    "popstate",
    (e) => {
        e.stopImmediatePropagation();
    },
    true,
);

function modalEl() {
    return document.getElementById(MODAL_ID);
}
function bodyEl() {
    return document.getElementById(BODY_ID);
}
function isModalOpen() {
    return modalEl()?.classList.contains("open");
}

function hashUuid() {
    if (!location.hash.startsWith(HASH_PREFIX)) return null;
    const raw = location.hash.slice(HASH_PREFIX.length);
    if (!raw) return null;
    try {
        return decodeURIComponent(raw);
    } catch {
        return raw;
    }
}

function showModal() {
    bodyEl().innerHTML = "Loading...";
    document.body.classList.add("no-scroll");
    modalEl().classList.add("open");
}

function hideModal() {
    document.body.classList.remove("no-scroll");
    modalEl()?.classList.remove("open");
    const body = bodyEl();
    if (body) body.innerHTML = "";
}

function loadBook(uuid) {
    showModal();
    if (typeof htmx === "undefined") {
        bodyEl().innerHTML = "htmx not loaded.";
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
    } else if (isModalOpen()) {
        hideModal();
        openedViaNav = false;
    }
}

/**
 * Close the modal. Goes back if the click pushed an entry,
 * otherwise strips the hash in place (deep-link case).
 */
export function closeModal() {
    if (!isModalOpen()) return;
    if (openedViaNav) {
        history.back(); // hashchange -> syncFromHash -> hideModal
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
export function setupModal() {
    if (modalInitialized || !modalEl()) return;
    modalInitialized = true;

    // Mark that the next hash change came from a card click
    document.addEventListener(
        "click",
        (e) => {
            if (e.target.closest?.(".book-link")) openedViaNav = true;
        },
        true,
    );

    window.addEventListener("hashchange", syncFromHash);

    // Close: X button or backdrop click
    modalEl().addEventListener("click", (e) => {
        if (e.target === modalEl() || e.target.closest(CLOSE_SELECTOR)) {
            e.preventDefault();
            closeModal();
        }
    });

    // Close: Escape
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") closeModal();
    });

    // Failed fetch
    document.body.addEventListener("htmx:responseError", (e) => {
        if (e.detail.target?.id === BODY_ID)
            bodyEl().innerHTML = "Failed to load book.";
    });
    document.body.addEventListener("htmx:sendError", (e) => {
        if (e.detail.target?.id === BODY_ID)
            bodyEl().innerHTML = "Failed to load book.";
    });

    // Deep link on initial load
    if (hashUuid()) {
        openedViaNav = false;
        syncFromHash();
    }
}

/**
 * Setup book card click handlers for modal opening
 * @param {HTMLElement} container - Container element with book cards
 */
export function setupBookClickHandlers(container) {
    setupModal();
    if (typeof htmx !== "undefined") htmx.process(container); // Allow hx-preload
}
