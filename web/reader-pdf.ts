import { byId } from "./dom.js";
import { setupChrome } from "./reader-chrome.js";
import { getSettings, onSettingsChange, setSettings } from "./reader-settings.js";
import { createReadingSync, renderBookmarks, type Bookmark, type BookmarkHandlers } from "./reading-sync.js";
import { openPdf } from "./vendor.js";

const params = new URLSearchParams(window.location.search);
const uuidParam = params.get("uuid");

if (!uuidParam) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}
const uuid: string = uuidParam;

const container = byId("pdf-container");
const chrome = setupChrome({
    onScrub(fraction, done) {
        const page = Math.min(numPages(), Math.max(1, Math.round(fraction * (numPages() - 1)) + 1));
        if (!done) return `Page ${page} of ${numPages()}`;
        void renderPage(page);
    },
});
chrome.setScrubbable(false);

// Resume from this device or the server (whichever is newer).
const sync = await createReadingSync(uuid, "pdf", { legacyKey: `pdf-page-${uuid}`, onStatus: chrome.setStatus });
const saved = await sync.loadProgress();
if (!sync.signedIn) chrome.setStatus("Position saved on this device");

let pdf: PdfJS.Document | null = null;

/** The loaded document (everything below runs after init loads it). */
function requirePdf(): PdfJS.Document {
    if (!pdf) throw new Error("PDF not loaded yet");
    return pdf;
}
/** Page count, or 1 before the document has loaded. */
const numPages = (): number => pdf?.numPages ?? 1;
let currentPage = parseInt(saved?.position || "1", 10) || 1;
// The first render is just opening the book; only moving away from it is
// saved (and marks the book as being read).
let openingPage: number | null = null;
let scale = getSettings().pdfScale;
/** Whether the last render showed two pages (dual is on and they fit). */
let shownDual = false;

async function drawPage(pageNum: number, canvas: HTMLCanvasElement): Promise<void> {
    if (pageNum < 1 || pageNum > numPages()) return;
    const page = await requirePdf().getPage(pageNum);
    const viewport = page.getViewport({ scale });
    canvas.width = viewport.width;
    canvas.height = viewport.height;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    await page.render({ canvasContext: ctx, viewport }).promise;
}

/** The scale at which the page (or two) fills the window. */
async function fitScale(dual: boolean): Promise<number> {
    const page = await requirePdf().getPage(currentPage);
    const viewport = page.getViewport({ scale: 1 });
    return Math.min(window.innerHeight / viewport.height, (window.innerWidth / (dual ? 2 : 1) - 8) / viewport.width);
}

// One render at a time; a request made meanwhile replaces any waiting one.
let rendering = false;
let waiting: number | null = null;

async function renderPage(pageNum: number): Promise<void> {
    if (rendering) {
        waiting = pageNum;
        return;
    }
    rendering = true;
    try {
        currentPage = pageNum;
        const s = getSettings();
        if (s.pdfFit) scale = await fitScale(s.pdfDual && window.innerWidth > window.innerHeight);
        const first = await requirePdf().getPage(pageNum);
        const dual = s.pdfDual && window.innerWidth >= first.getViewport({ scale }).width * 2 + 4;
        shownDual = dual;

        const canvases = [document.createElement("canvas")];
        if (dual && pageNum < numPages()) canvases.push(document.createElement("canvas"));
        await Promise.all(canvases.map((c, i) => drawPage(pageNum + i, c)));
        const spread = document.createElement("div");
        spread.className = "pdf-spread";
        spread.append(...canvases);
        container.replaceChildren(spread);
        container.scrollTo(0, 0);

        const last = pageNum + canvases.length - 1;
        const text = last > pageNum ? `Pages ${pageNum}–${last} of ${numPages()}` : `Page ${pageNum} of ${numPages()}`;
        chrome.setPosition(text, numPages() > 1 ? (pageNum - 1) / (numPages() - 1) : 0);
        byId("zoom-label").textContent = `${Math.round(scale * 100)}%`;

        if (openingPage === null) {
            openingPage = pageNum;
        } else if (pageNum !== openingPage) {
            openingPage = -1; // moved: from now on every page change is saved
            sync.saveProgress(pageNum, last / numPages());
        }
    } finally {
        rendering = false;
        if (waiting !== null) {
            const next = waiting;
            waiting = null;
            void renderPage(next);
        }
    }
}

/** Forward (1) or back (-1) by what is on screen: one page, or two. */
function turn(dir: 1 | -1, fromPage = true): void {
    if (fromPage) chrome.hideBars();
    const to = Math.min(numPages(), Math.max(1, currentPage + dir * (shownDual ? 2 : 1)));
    if (to !== currentPage) void renderPage(to);
}

async function init(): Promise<void> {
    try {
        pdf = await openPdf(`/download/${encodeURIComponent(uuid)}/pdf`);
    } catch (err) {
        container.className = "reader-message";
        container.textContent = "Couldn't load this PDF.";
        throw err;
    }
    currentPage = Math.min(currentPage, numPages());
    chrome.setScrubbable(numPages() > 1);
    await renderToc();
    await renderPage(currentPage);
}
void init();

byId("prev-btn").addEventListener("click", () => turn(-1, false));
byId("next-btn").addEventListener("click", () => turn(1, false));
document.querySelector(".btn-back")?.addEventListener("click", () => history.back());

window.addEventListener("keydown", (e) => {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    // Keys belong to the control that has focus (the scrubber, a tab).
    if (e.target instanceof Element && e.target.closest(".r-bar, .r-sidebar, .r-sheet")) return;
    if (e.key === "ArrowRight" || e.key === "PageDown" || (e.key === " " && !e.shiftKey)) turn(1);
    else if (e.key === "ArrowLeft" || e.key === "PageUp" || e.key === " ") turn(-1);
    else return;
    e.preventDefault();
});

// ---- settings ----

onSettingsChange((s, changed) => {
    if (!pdf) return;
    if (changed.includes("pdfFit") && !s.pdfFit) return; // nothing changes until the next zoom
    if (changed.some((k) => k === "pdfDual" || k === "pdfFit")) void renderPage(currentPage);
});

function zoomBy(step: number): void {
    scale = Math.min(4, Math.max(0.5, scale + step));
    setSettings({ pdfFit: false, pdfScale: scale });
    if (pdf) void renderPage(currentPage);
}
byId("zoom-in").addEventListener("click", () => zoomBy(0.25));
byId("zoom-out").addEventListener("click", () => zoomBy(-0.25));

window.addEventListener("resize", () => {
    if (getSettings().pdfFit && pdf) void renderPage(currentPage);
});

// ---- contents ----

async function renderToc(): Promise<void> {
    const outline = await requirePdf().getOutline();
    const list = byId("toc-list");

    if (!outline || outline.length === 0) {
        list.innerHTML = '<li class="toc-empty">This PDF has no contents.</li>';
        return;
    }

    function buildToc(items: PdfJS.OutlineItem[], el: HTMLElement): void {
        for (const item of items) {
            const li = document.createElement("li");
            const a = document.createElement("a");
            a.textContent = item.title;
            a.href = "#";
            a.addEventListener("click", async (e) => {
                e.preventDefault();
                chrome.sidebar.close();
                if (!item.dest) return;
                const dest = typeof item.dest === "string" ? await requirePdf().getDestination(item.dest) : item.dest;
                if (dest?.[0] === undefined) return;
                const pageIndex = await requirePdf().getPageIndex(dest[0]);
                void renderPage(pageIndex + 1); // pdf.js is 0-indexed
            });
            li.appendChild(a);
            if (item.items?.length) {
                const sub = document.createElement("ul");
                buildToc(item.items, sub);
                li.appendChild(sub);
            }
            el.appendChild(li);
        }
    }
    buildToc(outline, list);
}

// ---- the page: wheel and taps ----

// The wheel turns pages when the page fits the window (one per gesture);
// when it doesn't, it scrolls.
let wheelAt = 0;
container.addEventListener(
    "wheel",
    (e) => {
        if (e.ctrlKey || container.scrollHeight > container.clientHeight + 1 || container.scrollWidth > container.clientWidth + 1) return;
        e.preventDefault();
        if (Math.abs(e.deltaY) < 4 || e.timeStamp - wheelAt < 350) return;
        wheelAt = e.timeStamp;
        turn(e.deltaY > 0 ? 1 : -1);
    },
    { passive: false },
);

// A tap on the left or right third turns the page; the middle shows or
// hides the bars.
container.addEventListener("click", (e) => {
    if (e.clientX < window.innerWidth * 0.3) turn(-1);
    else if (e.clientX > window.innerWidth * 0.7) turn(1);
    else chrome.toggleBars();
});

// ---- bookmarks ----
const bookmarkHandlers: BookmarkHandlers = {
    onGo: (b) => {
        void renderPage(Math.min(numPages(), Math.max(1, parseInt(b.position, 10) || 1)));
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
    sync.addBookmark(currentPage, `Page ${currentPage}${pdf ? ` of ${numPages()}` : ""}`);
    showBookmarks(sync.bookmarks());
    chrome.toast("Bookmark added");
});
