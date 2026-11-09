import { byId, maybeId } from "./dom.js";
import { createReadingSync, renderBookmarks } from "./reading-sync.js";
import { setupSidebar } from "./reader-sidebar.js";
const params = new URLSearchParams(window.location.search);
const uuidParam = params.get("uuid");
if (!uuidParam) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}
const uuid = uuidParam;
const $ = (id) => byId(id);
const progressLabel = $("progress-label");
const sidebar = setupSidebar();
const syncLabel = maybeId("sync-status");
const setSync = (text) => {
    if (syncLabel)
        syncLabel.textContent = text;
};
/**
 * The whole .epub is downloaded once and opened from memory: chapters,
 * images, styles and fonts never touch the network again, so the rest of the
 * book reads offline. (epub.js renders each chapter in a sandboxed iframe
 * without scripts.)
 */
async function downloadBook() {
    const res = await fetch(`/download/${encodeURIComponent(uuid)}/epub`, { credentials: "same-origin" });
    if (!res.ok || !res.body)
        throw new Error(`HTTP ${res.status}`);
    const total = Number(res.headers.get("content-length")) || 0;
    const chunks = [];
    let got = 0;
    const reader = res.body.getReader();
    for (;;) {
        const { done, value } = await reader.read();
        if (done)
            break;
        chunks.push(value);
        got += value.length;
        progressLabel.textContent = total ? `Loading ${Math.round((got / total) * 100)}%` : `Loading ${(got / 1048576).toFixed(1)} MB`;
    }
    const buf = new Uint8Array(got);
    let at = 0;
    for (const c of chunks) {
        buf.set(c, at);
        at += c.length;
    }
    progressLabel.textContent = "—";
    return buf.buffer;
}
let data;
try {
    data = await downloadBook();
}
catch (err) {
    progressLabel.textContent = "Failed to load";
    $("reader-container").textContent = `Couldn't load this book (${err instanceof Error ? err.message : String(err)}).`;
    throw err;
}
const book = ePub(data);
const rendition = book.renderTo("reader-container", {
    width: "100%",
    height: "100%",
    flow: "paginated",
});
/**
 * Links inside the book (registered before the first chapter renders). We handle clicks ourselves (capture phase, before
 * epub.js): in-book links become a history entry plus a jump; external links
 * open in a new tab from this page (the chapter iframe is sandboxed and
 * can't open windows).
 */
rendition.on("rendered", (section, view) => {
    // A press in the page (its own document) closes the sidebar.
    view.document.addEventListener("pointerdown", () => sidebar.close());
    view.document.addEventListener("click", (e) => {
        // The target lives in the iframe's window: `instanceof Element` (ours) is false.
        const t = e.target;
        const a = typeof t?.closest === "function" ? t.closest("a[href]") : null;
        const href = a?.getAttribute("href");
        if (!href)
            return;
        e.preventDefault();
        e.stopImmediatePropagation();
        const url = new URL(href, `epub:${section.url}`);
        if (url.protocol !== "epub:") {
            if (/^(https?|mailto):$/.test(url.protocol))
                window.open(url.href, "_blank", "noopener,noreferrer");
            return;
        }
        // Same-book target: find its spine item (by path), keep the #anchor.
        const path = decodeURIComponent(url.pathname);
        for (let i = 0; i < (book.spine.length ?? 0); i++) {
            const item = book.spine.get(i);
            if (item && decodeURIComponent(item.url) === path) {
                pushJump();
                void rendition.display(item.href + url.hash);
                return;
            }
        }
    }, true);
});
// ---- font size -------------------------------------------------------------
let fontSize = parseInt(localStorage.getItem("reader-fontsize") || "100", 10) || 100;
function applyFontSize() {
    rendition.themes.fontSize(`${fontSize}%`);
    localStorage.setItem("reader-fontsize", String(fontSize));
}
// ---- position: resume (local or synced), save as the reader moves ----------
const sync = await createReadingSync(uuid, "epub", { legacyKey: `reader-cfi-${uuid}`, onStatus: setSync });
const saved = await sync.loadProgress();
let startCfi = saved?.position || null;
applyFontSize();
let locationsReady = false;
let currentCfi = null;
let currentPercent = 0;
// The first "relocated" is just the book opening (epub.js may normalise the
// saved CFI to the page start). Only moving away from it is saved — and that
// is what marks the book as being read.
let openingCfi = null;
function percentFor(loc) {
    if (loc.atEnd)
        return 1;
    if (!locationsReady)
        return 0;
    return book.locations.percentageFromCfi(loc.start.cfi) || 0;
}
/**
 * "p. N / M": the print edition's page numbers when the book has a page
 * list, otherwise synthetic pages (~1600 characters, roughly a printed page;
 * the same on every device and font size). The tooltip adds the percentage
 * and the screen within the chapter.
 */
const CHARS_PER_PAGE = 1600;
let lastLoc = null;
function pageText(cfi) {
    const pl = book.pageList;
    const printed = pl?.pages.length ? pl.pageFromCfi(cfi) : -1;
    if (printed > 0)
        return `p. ${printed} / ${pl?.lastPage}`;
    if (!locationsReady)
        return "—";
    return `p. ${book.locations.locationFromCfi(cfi) + 1} / ${book.locations.length()}`;
}
function updateLabel() {
    if (!currentCfi)
        return;
    progressLabel.textContent = pageText(currentCfi);
    const d = lastLoc?.start.displayed;
    progressLabel.title =
        (locationsReady ? `${Math.round(currentPercent * 100)}%` : "") + (d ? ` · screen ${d.page} of ${d.total} in this chapter` : "");
}
rendition.on("relocated", (loc) => {
    if (!loc?.start?.cfi)
        return;
    currentCfi = loc.start.cfi;
    lastLoc = loc;
    currentPercent = percentFor(loc);
    updateLabel();
    if (openingCfi === null) {
        openingCfi = currentCfi;
        return;
    }
    if (currentCfi === openingCfi)
        return;
    openingCfi = ""; // moved: from now on every change is saved
    sync.saveProgress(currentCfi, currentPercent);
});
// Handler is attached first so the opening position is seen as such.
try {
    await rendition.display(startCfi || undefined);
}
catch {
    startCfi = null; // stale/invalid CFI (e.g. the book file changed)
    openingCfi = null;
    await rendition.display();
}
book.ready
    .then(() => book.locations.generate(CHARS_PER_PAGE))
    .then(() => {
    locationsReady = true;
    if (currentCfi)
        currentPercent = book.locations.percentageFromCfi(currentCfi) || 0;
    updateLabel();
});
const state = () => history.state ?? {};
// Keep the current entry pointing at where the reader is now.
rendition.on("relocated", (loc) => {
    if (loc?.start?.cfi)
        history.replaceState({ depth: state().depth ?? 0, cfi: loc.start.cfi }, ""); // only our keys (not htmx's)
});
/** Call before an in-book jump: the current spot stays behind as a Back entry. */
function pushJump() {
    history.pushState({ depth: (state().depth ?? 0) + 1 }, "");
}
window.addEventListener("popstate", () => {
    const cfi = state().cfi;
    if (cfi)
        void rendition.display(cfi);
});
// The top-bar Back leaves the reader, skipping the in-book jumps.
document.querySelector(".btn-back")?.addEventListener("click", () => history.go(-((state().depth ?? 0) + 1)));
// ---- contents --------------------------------------------------------------
/** Every contents entry, in order, with its link. */
const tocEntries = [];
function renderToc(items, el) {
    for (const item of items) {
        const li = document.createElement("li");
        const a = document.createElement("a");
        const label = item.label.trim();
        a.textContent = label;
        a.href = "#";
        a.addEventListener("click", (e) => {
            e.preventDefault();
            pushJump();
            void rendition.display(item.href);
            sidebar.close();
        });
        tocEntries.push({ href: item.href, label, link: a });
        li.appendChild(a);
        if (item.subitems?.length) {
            const sub = document.createElement("ul");
            renderToc(item.subitems, sub);
            li.appendChild(sub);
        }
        el.appendChild(li);
    }
}
/** The contents entry for the chapter being read (the first one in its file). */
function currentTocEntry() {
    try {
        const href = (currentCfi ? book.spine.get(currentCfi) : null)?.href || "";
        if (!href)
            return undefined;
        return tocEntries.find((t) => {
            const file = t.href.split("#")[0];
            return !!file && href.endsWith(file);
        });
    }
    catch {
        return undefined;
    }
}
/** Highlight the current chapter in the contents. */
function markCurrentChapter() {
    const current = currentTocEntry()?.link;
    for (const { link } of tocEntries) {
        if (link === current)
            link.setAttribute("aria-current", "true");
        else
            link.removeAttribute("aria-current");
    }
}
book.ready.then(() => {
    renderToc(book.navigation.toc, $("toc-list"));
    markCurrentChapter();
});
rendition.on("relocated", markCurrentChapter);
// ---- bookmarks -------------------------------------------------------------
function chapterLabel() {
    return currentTocEntry()?.label ?? "";
}
const bookmarkHandlers = {
    onGo: (b) => {
        void rendition.display(b.position);
        sidebar.close();
    },
    onDelete: (b) => {
        sync.removeBookmark(b.id);
        showBookmarks(sync.bookmarks());
    },
};
const showBookmarks = (items) => renderBookmarks($("bookmark-list"), items, bookmarkHandlers);
sync.loadBookmarks().then(showBookmarks);
$("bookmark-btn")?.addEventListener("click", () => {
    if (!currentCfi)
        return;
    const page = pageText(currentCfi);
    const pct = page !== "—" ? ` — ${page.split(" / ")[0]}` : "";
    sync.addBookmark(currentCfi, `${chapterLabel() || "Bookmark"}${pct}`);
    showBookmarks(sync.bookmarks());
    setSync("Bookmark added");
});
// ---- navigation ------------------------------------------------------------
function safePrev() {
    rendition.prev().catch((e) => {
        if (!e?.message?.includes("No Section Found"))
            console.error(e);
    });
}
function safeNext() {
    rendition.next().catch((e) => {
        if (!e?.message?.includes("No Section Found"))
            console.error(e);
    });
}
$("prev-btn").addEventListener("click", safePrev);
$("next-btn").addEventListener("click", safeNext);
window.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft")
        safePrev();
    if (e.key === "ArrowRight")
        safeNext();
});
// scroll to navigate
rendition.on("rendered", (_section, view) => {
    view.document.addEventListener("wheel", (e) => {
        e.preventDefault();
        if (e.deltaY > 0)
            safeNext();
        else
            safePrev();
    }, { passive: false });
});
// click left/right half
$("reader-container").addEventListener("click", (e) => {
    if (e.clientX < window.innerWidth / 2)
        safePrev();
    else
        safeNext();
});
$("font-up").addEventListener("click", () => {
    fontSize = Math.min(fontSize + 10, 200);
    applyFontSize();
});
$("font-down").addEventListener("click", () => {
    fontSize = Math.max(fontSize - 10, 60);
    applyFontSize();
});
if (!sync.signedIn)
    setSync("Saved on this device");
