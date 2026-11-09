import * as pdfjsLib from "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.mjs";
import { byId, maybeId } from "./dom.js";
import { createReadingSync, renderBookmarks } from "./reading-sync.js";
pdfjsLib.GlobalWorkerOptions.workerSrc = "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.worker.mjs";
const params = new URLSearchParams(window.location.search);
const uuidParam = params.get("uuid");
if (!uuidParam) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}
const uuid = uuidParam;
const progressLabel = byId("progress-label");
const syncLabel = maybeId("sync-status");
const setSync = (text) => {
    if (syncLabel)
        syncLabel.textContent = text;
};
// Resume from this device or the server (whichever is newer).
const sync = await createReadingSync(uuid, "pdf", { legacyKey: `pdf-page-${uuid}`, onStatus: setSync });
const saved = await sync.loadProgress();
if (!sync.signedIn)
    setSync("Saved on this device");
let pdf = null;
/** The loaded document (everything below runs after init loads it). */
function requirePdf() {
    if (!pdf)
        throw new Error("PDF not loaded yet");
    return pdf;
}
/** Page count, or 1 before the document has loaded. */
const numPages = () => pdf?.numPages ?? 1;
let currentPage = parseInt(saved?.position || "1", 10) || 1;
// The first render is just opening the book; only moving away from it is
// saved (and marks the book as being read).
let openingPage = null;
let scale = parseFloat(localStorage.getItem("pdf-scale") || "1.5");
let rendering = false;
let lastWasDual = false; // updated each renderPage
const dualToggle = byId("dual-toggle", HTMLInputElement);
dualToggle.checked = localStorage.getItem("pdf-dual") === "true";
dualToggle.addEventListener("change", () => {
    localStorage.setItem("pdf-dual", String(dualToggle.checked));
    renderPage(currentPage);
});
function isDual() { return dualToggle.checked; }
async function isWideEnough() {
    const page = await requirePdf().getPage(currentPage);
    const viewport = page.getViewport({ scale });
    return window.innerWidth >= viewport.width * 2 + 4; // +4 for the gap
}
async function effectiveDual() {
    return dualToggle.checked && await isWideEnough();
}
async function drawPage(pageNum, canvas) {
    if (pageNum < 1 || pageNum > numPages())
        return;
    const page = await requirePdf().getPage(pageNum);
    const viewport = page.getViewport({ scale });
    canvas.width = viewport.width;
    canvas.height = viewport.height;
    const ctx = canvas.getContext("2d");
    if (!ctx)
        return;
    await page.render({ canvasContext: ctx, viewport }).promise;
}
async function renderPage(pageNum) {
    if (rendering)
        return;
    rendering = true;
    const container = byId("pdf-container");
    container.innerHTML = "";
    const dual = await effectiveDual();
    lastWasDual = dual;
    if (dual) {
        const wrapper = document.createElement("div");
        wrapper.style.cssText = "display:flex; gap:4px; justify-content:center; align-items:flex-start;";
        const left = document.createElement("canvas");
        const right = document.createElement("canvas");
        wrapper.appendChild(left);
        wrapper.appendChild(right);
        container.appendChild(wrapper);
        await Promise.all([
            drawPage(pageNum, left),
            drawPage(pageNum + 1, right),
        ]);
        progressLabel.textContent = `${pageNum}-${Math.min(pageNum + 1, numPages())} / ${numPages()}`;
    }
    else {
        const canvas = document.createElement("canvas");
        container.appendChild(canvas);
        await drawPage(pageNum, canvas);
        progressLabel.textContent = `${pageNum} / ${numPages()}`;
    }
    currentPage = pageNum;
    if (openingPage === null) {
        openingPage = pageNum;
    }
    else if (pageNum !== openingPage) {
        openingPage = -1; // moved: from now on every page change is saved
        const lastShown = dual ? Math.min(pageNum + 1, numPages()) : pageNum;
        sync.saveProgress(pageNum, lastShown / numPages());
    }
    rendering = false;
}
async function init() {
    pdf = await pdfjsLib.getDocument(`/download/${uuid}/pdf`).promise;
    currentPage = Math.min(currentPage, numPages());
    await renderToc();
    if (fitToggle.checked) {
        await fitToScale();
    }
    else {
        renderPage(currentPage);
    }
}
init();
// update prev/next to step by 2 in dual mode
byId("prev-btn").addEventListener("click", () => {
    const step = isDual() ? 2 : 1;
    if (currentPage > 1)
        renderPage(Math.max(1, currentPage - step));
});
byId("next-btn").addEventListener("click", () => {
    const step = isDual() ? 2 : 1;
    if (currentPage < numPages())
        renderPage(Math.min(numPages(), currentPage + step));
});
window.addEventListener("keydown", (e) => {
    const step = isDual() ? 2 : 1;
    if (e.key === "ArrowLeft" && currentPage > 1)
        renderPage(Math.max(1, currentPage - step));
    if (e.key === "ArrowRight" && currentPage < numPages())
        renderPage(Math.min(numPages(), currentPage + step));
});
byId("font-up").addEventListener("click", () => {
    scale = Math.min(scale + 0.25, 4);
    localStorage.setItem("pdf-scale", String(scale));
    renderPage(currentPage);
});
byId("font-down").addEventListener("click", () => {
    scale = Math.max(scale - 0.25, 0.5);
    localStorage.setItem("pdf-scale", String(scale));
    renderPage(currentPage);
});
function availableHeight() {
    return window.innerHeight - (document.querySelector(".top-bar")?.offsetHeight ?? 0);
}
const fitToggle = byId("fit-toggle", HTMLInputElement);
fitToggle.checked = localStorage.getItem("pdf-fit") === "true";
async function fitToScale() {
    const page = await requirePdf().getPage(currentPage);
    const viewport = page.getViewport({ scale: 1 });
    const divisor = lastWasDual ? 2 : 1;
    const heightScale = availableHeight() / viewport.height;
    const widthScale = (window.innerWidth / divisor - 8) / viewport.width;
    scale = Math.min(heightScale, widthScale);
    localStorage.setItem("pdf-scale", String(scale));
    await renderPage(currentPage);
}
async function applyFit() {
    if (fitToggle.checked) {
        localStorage.setItem("pdf-fit", "true");
        await fitToScale();
    }
    else {
        localStorage.setItem("pdf-fit", "false");
    }
}
fitToggle.addEventListener("change", applyFit);
// resize handler — only refit if fit mode is active
window.addEventListener("resize", () => {
    if (fitToggle.checked && pdf)
        fitToScale();
});
// disable zoom buttons when fit is active since fit overrides scale
byId("font-up").addEventListener("click", () => {
    fitToggle.checked = false;
    localStorage.setItem("pdf-fit", "false");
    scale = Math.min(scale + 0.25, 4);
    localStorage.setItem("pdf-scale", String(scale));
    renderPage(currentPage);
});
byId("font-down").addEventListener("click", () => {
    fitToggle.checked = false;
    localStorage.setItem("pdf-fit", "false");
    scale = Math.max(scale - 0.25, 0.5);
    localStorage.setItem("pdf-scale", String(scale));
    renderPage(currentPage);
});
async function renderToc() {
    const outline = await requirePdf().getOutline();
    const list = byId("toc-list");
    if (!outline || outline.length === 0) {
        list.innerHTML = "<li style='color:#888'>No outline available.</li>";
        return;
    }
    async function buildToc(items, el) {
        for (const item of items) {
            const li = document.createElement("li");
            li.style.padding = "6px 0";
            const a = document.createElement("a");
            a.textContent = item.title;
            a.href = "#";
            a.style.cssText = "color:var(--color-text, #eee); text-decoration:none; display:block;";
            a.addEventListener("click", async (e) => {
                e.preventDefault();
                if (item.dest) {
                    const dest = typeof item.dest === "string"
                        ? await requirePdf().getDestination(item.dest)
                        : item.dest;
                    if (dest?.[0] !== undefined) {
                        const pageIndex = await requirePdf().getPageIndex(dest[0]);
                        void renderPage(pageIndex + 1); // pdf.js is 0-indexed
                    }
                }
                byId("toc-sidebar").style.display = "none";
            });
            li.appendChild(a);
            if (item.items?.length) {
                const sub = document.createElement("ul");
                sub.style.paddingLeft = "1rem";
                await buildToc(item.items, sub);
                li.appendChild(sub);
            }
            el.appendChild(li);
        }
    }
    await buildToc(outline, list);
}
byId("toc-btn").addEventListener("click", () => {
    const sidebar = byId("toc-sidebar");
    sidebar.style.display = sidebar.style.display === "none" ? "block" : "none";
});
// scroll to navigate
byId("pdf-container").addEventListener("wheel", (e) => {
    e.preventDefault();
    if (e.deltaY > 0) {
        // scroll down = next
        if (currentPage < numPages())
            renderPage(Math.min(numPages(), currentPage + (effectiveDualSync() ? 2 : 1)));
    }
    else {
        // scroll up = prev
        if (currentPage > 1)
            renderPage(Math.max(1, currentPage - (effectiveDualSync() ? 2 : 1)));
    }
}, { passive: false });
// click left/right half to navigate
byId("pdf-container").addEventListener("click", (e) => {
    const mid = window.innerWidth / 2;
    if (e.clientX < mid) {
        if (currentPage > 1)
            renderPage(Math.max(1, currentPage - (effectiveDualSync() ? 2 : 1)));
    }
    else {
        if (currentPage < numPages())
            renderPage(Math.min(numPages(), currentPage + (effectiveDualSync() ? 2 : 1)));
    }
});
// ---- bookmarks ----
const bookmarkHandlers = {
    onGo: (b) => {
        renderPage(Math.min(numPages(), Math.max(1, parseInt(b.position, 10) || 1)));
        byId("toc-sidebar").style.display = "none";
    },
    onDelete: (b) => {
        sync.removeBookmark(b.id);
        showBookmarks(sync.bookmarks());
    },
};
const showBookmarks = (items) => renderBookmarks(byId("bookmark-list"), items, bookmarkHandlers);
sync.loadBookmarks().then(showBookmarks);
byId("bookmark-btn")?.addEventListener("click", () => {
    sync.addBookmark(currentPage, `Page ${currentPage}${pdf ? ` of ${numPages()}` : ""}`);
    showBookmarks(sync.bookmarks());
    setSync("Bookmark added");
});
function effectiveDualSync() {
    return lastWasDual;
}
