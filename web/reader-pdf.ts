import { byId, maybeId } from "./dom.js";
import { setupPdfGestures } from "./pdf-gestures.js";
import { setupLoupe } from "./pdf-loupe.js";
import { ACTUAL_SIZE, createPdfView, type PdfView, type ViewLayout } from "./pdf-view.js";
import { setupChrome } from "./reader-chrome.js";
import { getSettings, onSettingsChange, setSettings, type ReaderSettings } from "./reader-settings.js";
import { createReadingSync, renderBookmarks, type Bookmark, type BookmarkHandlers } from "./reading-sync.js";
import { loadPdfJs, openPdf } from "./vendor.js";

const params = new URLSearchParams(window.location.search);
const uuidParam = params.get("uuid");

if (!uuidParam) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}
const uuid: string = uuidParam;

const container = byId("pdf-container");
let view: PdfView | null = null;
const numPages = (): number => view?.numPages ?? 1;

const chrome = setupChrome({
    onScrub(fraction, done) {
        const page = Math.min(numPages(), Math.max(1, Math.round(fraction * (numPages() - 1)) + 1));
        if (!done) return `Page ${page} of ${numPages()}`;
        jump(page);
    },
});
chrome.setScrubbable(false);

// Resume from this device or the server (whichever is newer).
const sync = await createReadingSync(uuid, "pdf", { legacyKey: `pdf-page-${uuid}`, onStatus: chrome.setStatus });
const saved = await sync.loadProgress();
if (!sync.signedIn) chrome.setStatus("Position saved on this device");

// ---- layout from the settings -------------------------------------------------

/** The scale set by hand (pinch, wheel, the zoom buttons), until a fit is chosen again. */
let handZoom: number | null = null;

function layoutFor(s: ReaderSettings): ViewLayout {
    return {
        dual: s.pdfDual,
        rtl: s.pdfRtl,
        coverAlone: s.pdfCover,
        zoom: handZoom === null ? s.pdfZoom : "custom",
        scale: handZoom ?? 1,
    };
}
function applyLayout(): void {
    void view?.setLayout(layoutFor(getSettings())).then(showZoom);
}
/** Back to fitting the page or its width. */
function fit(mode: "page" | "width"): void {
    handZoom = null;
    setSettings({ pdfZoom: mode });
    applyLayout();
}

const zoomLabel = byId("zoom-label");
const fitButtons = [...document.querySelectorAll<HTMLElement>('[data-setting="pdfZoom"]')];
function showZoom(): void {
    if (view) zoomLabel.textContent = `${Math.round((view.scale / ACTUAL_SIZE) * 100)}%`;
    // Neither fit is in effect after zooming by hand.
    for (const b of fitButtons) b.setAttribute("aria-pressed", String(handZoom === null && b.dataset.value === getSettings().pdfZoom));
}
for (const b of fitButtons) b.addEventListener("click", () => fit(b.dataset.value === "width" ? "width" : "page"));

function showDirection(): void {
    const rtl = getSettings().pdfRtl;
    // The buttons stay where they are; what they do follows the reading direction.
    const label = (id: string, text: string): void => {
        const b = byId(id);
        b.title = text;
        b.setAttribute("aria-label", text);
    };
    label("prev-btn", rtl ? "Next page" : "Previous page");
    label("next-btn", rtl ? "Previous page" : "Next page");
    byId("scrubber").dir = rtl ? "rtl" : "ltr";
    const cover = document.querySelector<HTMLInputElement>('[data-setting="pdfCover"]');
    if (cover) cover.disabled = !getSettings().pdfDual;
}
showDirection();

onSettingsChange((_s, changed) => {
    showDirection();
    showZoom();
    if (changed.some((k) => k.startsWith("pdf"))) applyLayout();
});

// ---- position ---------------------------------------------------------------

/** history.state on reader entries; depth = jumps since the reader opened. */
interface PdfState {
    page?: number;
    depth?: number;
}
const state = (): PdfState => (history.state as PdfState | null) ?? {};

// The first page shown is just the book opening; only moving away from it is
// saved (and marks the book as being read).
let openingPage: number | null = null;

function onShown(first: number, last: number): void {
    const n = numPages();
    chrome.setPosition(last > first ? `Pages ${first}–${last} of ${n}` : `Page ${first} of ${n}`, n > 1 ? (first - 1) / (n - 1) : 0);
    showZoom();
    markCurrentChapter(first);
    loupe.refresh();
    history.replaceState({ depth: state().depth ?? 0, page: first } satisfies PdfState, "");
    if (openingPage === null) {
        openingPage = first;
    } else if (first !== openingPage) {
        openingPage = -1; // moved: from now on every page change is saved
        sync.saveProgress(first, last / n);
    }
}

/** Forward (1) or back (-1) by what is on screen: one page, or two. */
function turn(dir: 1 | -1, fromPage = true): void {
    if (fromPage) chrome.hideBars();
    const to = view?.neighbour(dir);
    if (to) void view?.show(to);
}
/** A turn towards the right (1) or left (-1) side of the screen. */
const turnSide = (side: 1 | -1, fromPage = true): void => turn(getSettings().pdfRtl ? (-side as 1 | -1) : side, fromPage);

/** Goes to a page from somewhere else in the book; Back returns. */
function jump(page: number): void {
    history.pushState({ depth: (state().depth ?? 0) + 1 } satisfies PdfState, "");
    void view?.show(page);
}
window.addEventListener("popstate", () => {
    const page = state().page;
    if (page) void view?.show(page);
});
// The top-bar Back leaves the reader, skipping the in-book jumps.
document.querySelector(".btn-back")?.addEventListener("click", () => history.go(-((state().depth ?? 0) + 1)));

async function destPage(dest: PdfJS.Dest, pdf: PdfJS.Document): Promise<number | null> {
    try {
        const d = typeof dest === "string" ? await pdf.getDestination(dest) : dest;
        if (!d || d[0] === undefined) return null;
        // A page reference, or (rarely) the page's index itself.
        return typeof d[0] === "number" ? d[0] + 1 : (await pdf.getPageIndex(d[0])) + 1;
    } catch {
        return null;
    }
}

// ---- gestures, taps and keys -----------------------------------------------------

const gestures = setupPdfGestures(
    container,
    () => view,
    (side) => turnSide(side),
);
const loupeBtn = maybeId("loupe-btn");
const loupe = setupLoupe(
    container,
    () => view,
    (on) => {
        loupeBtn?.setAttribute("aria-pressed", String(on));
        if (on) chrome.toast("Magnifier on. Scroll to change its strength; M or Esc turns it off.");
    },
);
loupeBtn?.addEventListener("click", () => loupe.set(!loupe.isOn()));

function zoomBy(factor: number): void {
    view?.setScale(view.scale * factor);
}
function doubleTap(x: number, y: number): void {
    if (!view) return;
    const fitted = view.fitScale("page");
    if (view.scale > fitted * 1.05) fit("page");
    else view.setScale(fitted * 2.5, view.anchorAt(x, y));
}

// A tap on the left or right third turns the page; in the middle it shows or
// hides the bars, and two taps zoom in on that spot (or back out). A page
// zoomed wider than the window has no turning zones: taps there would mostly
// be mistakes.
let pendingTap: { x: number; y: number; t: number; timer: ReturnType<typeof setTimeout> } | null = null;
container.addEventListener("click", (e) => {
    if (!view || gestures.wasGesture(e) || loupe.isOn()) return;
    const t = e.target instanceof Element ? e.target : null;
    if (t?.closest("a") || window.getSelection()?.toString()) return;
    if (!view.overflows().x) {
        if (e.clientX < window.innerWidth * 0.3) return turnSide(-1);
        if (e.clientX > window.innerWidth * 0.7) return turnSide(1);
    }
    if (pendingTap && e.timeStamp - pendingTap.t < 320 && Math.hypot(e.clientX - pendingTap.x, e.clientY - pendingTap.y) < 30) {
        clearTimeout(pendingTap.timer);
        pendingTap = null;
        // A double click on text selects the word, as anywhere else.
        if (!((e as PointerEvent).pointerType === "mouse" && t?.closest(".textLayer span"))) doubleTap(e.clientX, e.clientY);
        return;
    }
    if (pendingTap) clearTimeout(pendingTap.timer);
    const timer = setTimeout(() => {
        pendingTap = null;
        chrome.toggleBars();
    }, 280);
    pendingTap = { x: e.clientX, y: e.clientY, t: e.timeStamp, timer };
});

// The wheel turns pages when the page fits the window. When it doesn't, it
// scrolls, and a new wheel gesture at the top or bottom edge turns.
let wheelAt = -Infinity;
container.addEventListener(
    "wheel",
    (e) => {
        if (!view || e.ctrlKey || e.metaKey) return; // zooming: web/pdf-gestures.ts
        const over = view.overflows();
        const since = e.timeStamp - wheelAt;
        wheelAt = e.timeStamp;
        const dir = e.deltaY > 0 ? 1 : -1;
        if (over.x || over.y) {
            if (Math.abs(e.deltaY) < Math.abs(e.deltaX) || since < 250) return;
            const max = container.scrollHeight - container.clientHeight;
            const atEdge = dir > 0 ? container.scrollTop >= max - 1 : container.scrollTop <= 0;
            if (!atEdge || over.x) return;
        } else {
            e.preventDefault();
            if (Math.abs(e.deltaY) < 4 || since < 350) return;
        }
        turn(dir);
    },
    { passive: false },
);

window.addEventListener(
    "keydown",
    (e) => {
        if (e.key === "Escape" && loupe.isOn()) {
            loupe.set(false);
            e.stopImmediatePropagation(); // not also the bars
            return;
        }
        if (e.ctrlKey || e.metaKey || e.altKey || !view) return;
        // Keys belong to the control that has focus (the scrubber, a tab).
        if (e.target instanceof Element && e.target.closest(".r-bar, .r-sidebar, .r-sheet")) return;
        const page = (dir: 1 | -1): void => {
            // Most of a screen first, when the page is taller than the window.
            const max = container.scrollHeight - container.clientHeight;
            const atEdge = dir > 0 ? container.scrollTop >= max - 1 : container.scrollTop <= 0;
            if (atEdge) turn(dir);
            else container.scrollBy({ top: dir * container.clientHeight * 0.9 });
        };
        switch (e.key) {
            case "ArrowRight":
                turnSide(1);
                break;
            case "ArrowLeft":
                turnSide(-1);
                break;
            case "PageDown":
                page(1);
                break;
            case "PageUp":
                page(-1);
                break;
            case " ":
                page(e.shiftKey ? -1 : 1);
                break;
            case "Home":
                void view.show(1);
                break;
            case "End":
                void view.show(numPages());
                break;
            case "+":
            case "=":
                zoomBy(1.25);
                break;
            case "-":
                zoomBy(0.8);
                break;
            case "0":
                fit("page");
                break;
            case "m":
            case "M":
                loupe.set(!loupe.isOn());
                break;
            default:
                return;
        }
        e.preventDefault();
    },
    true,
);

byId("prev-btn").addEventListener("click", () => turnSide(-1, false));
byId("next-btn").addEventListener("click", () => turnSide(1, false));
byId("zoom-in").addEventListener("click", () => zoomBy(1.25));
byId("zoom-out").addEventListener("click", () => zoomBy(0.8));
byId("zoom-actual").addEventListener("click", () => view?.setScale(ACTUAL_SIZE));

// ---- contents -------------------------------------------------------------------

/** Contents entries with the page each starts on, once known. */
const tocEntries: { link: HTMLAnchorElement; label: string; page: number | null }[] = [];

function markCurrentChapter(page: number): void {
    // The last entry starting on or before this page.
    let current: (typeof tocEntries)[number] | undefined;
    for (const t of tocEntries) if (t.page !== null && t.page <= page && (!current || t.page >= (current.page ?? 0))) current = t;
    for (const t of tocEntries) {
        if (t === current) t.link.setAttribute("aria-current", "true");
        else t.link.removeAttribute("aria-current");
    }
    chrome.setChapter(current?.label ?? "");
}

async function renderToc(pdf: PdfJS.Document): Promise<void> {
    const outline = await pdf.getOutline().catch(() => null);
    const list = byId("toc-list");
    if (!outline?.length) {
        list.innerHTML = '<li class="toc-empty">This PDF has no contents.</li>';
        return;
    }
    const build = (items: PdfJS.OutlineItem[], el: HTMLElement): void => {
        for (const item of items) {
            const li = document.createElement("li");
            const a = document.createElement("a");
            a.textContent = item.title;
            a.href = "#";
            const entry = { link: a, label: item.title, page: null as number | null, dest: item.dest };
            tocEntries.push(entry);
            a.addEventListener("click", async (e) => {
                e.preventDefault();
                chrome.sidebar.close();
                const page = entry.page ?? (await destPage(item.dest, pdf));
                if (page) jump(page);
            });
            li.appendChild(a);
            if (item.items?.length) {
                const sub = document.createElement("ul");
                build(item.items, sub);
                li.appendChild(sub);
            }
            el.appendChild(li);
        }
    };
    build(outline, list);
    // Which page each entry is on, for marking the current chapter. In the
    // background: a long outline takes a while.
    for (const t of tocEntries as ((typeof tocEntries)[number] & { dest: PdfJS.Dest })[]) t.page = await destPage(t.dest, pdf);
    if (view) markCurrentChapter(view.first);
}

// ---- open -----------------------------------------------------------------------

async function init(): Promise<void> {
    let pdf: PdfJS.Document;
    try {
        pdf = await openPdf(`/download/${encodeURIComponent(uuid)}/pdf`);
    } catch (err) {
        container.className = "reader-message";
        container.textContent = "Couldn't load this PDF.";
        throw err;
    }
    view = createPdfView({
        container,
        pdf,
        lib: await loadPdfJs(),
        layout: layoutFor(getSettings()),
        onShown,
        onZoom(scale) {
            handZoom = scale;
            showZoom();
        },
        onDest: (dest) => void destPage(dest, pdf).then((page) => page && jump(page)),
    });
    chrome.setScrubbable(pdf.numPages > 1);
    // A reload returns to where this tab was, which may be newer than what was saved.
    const start = state().page || parseInt(saved?.position || "1", 10) || 1;
    await view.show(Math.min(pdf.numPages, start));
    void renderToc(pdf);
}
void init();

// ---- bookmarks --------------------------------------------------------------------

const bookmarkHandlers: BookmarkHandlers = {
    onGo: (b) => {
        jump(Math.min(numPages(), Math.max(1, parseInt(b.position, 10) || 1)));
        chrome.sidebar.close();
    },
    onDelete: (b) => {
        sync.removeBookmark(b.id);
        showBookmarks(sync.bookmarks());
    },
};
const showBookmarks = (items: Bookmark[]): void => renderBookmarks(byId("bookmark-list"), items, bookmarkHandlers);
void sync.loadBookmarks().then(showBookmarks);
byId("bookmark-btn").addEventListener("click", () => {
    if (!view) return;
    sync.addBookmark(view.first, `Page ${view.first} of ${numPages()}`);
    showBookmarks(sync.bookmarks());
    chrome.toast("Bookmark added");
});
