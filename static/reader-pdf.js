import * as pdfjsLib from "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.mjs";

pdfjsLib.GlobalWorkerOptions.workerSrc = "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.worker.mjs";

const params = new URLSearchParams(window.location.search);
const uuid = params.get("uuid");

if (!uuid) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}

const progressLabel = document.getElementById("progress-label");

let pdf = null;
let currentPage = parseInt(localStorage.getItem(`pdf-page-${uuid}`) || "1");
let scale = parseFloat(localStorage.getItem("pdf-scale") || "1.5");
let rendering = false;
const dualToggle = document.getElementById("dual-toggle");
dualToggle.checked = localStorage.getItem("pdf-dual") === "true";

dualToggle.addEventListener("change", () => {
    localStorage.setItem("pdf-dual", dualToggle.checked);
    renderPage(currentPage);
});

function isDual() { return dualToggle.checked; }
async function isWideEnough() {
    const page = await pdf.getPage(currentPage);
    const viewport = page.getViewport({ scale });
    return window.innerWidth >= viewport.width * 2 + 4; // +4 for the gap
}

async function effectiveDual() {
    return dualToggle.checked && await isWideEnough();
}
async function drawPage(pageNum, canvas) {
    if (pageNum < 1 || pageNum > pdf.numPages) return;
    const page = await pdf.getPage(pageNum);
    const viewport = page.getViewport({ scale });
    canvas.width = viewport.width;
    canvas.height = viewport.height;
    await page.render({ canvasContext: canvas.getContext("2d"), viewport }).promise;
}

async function renderPage(pageNum) {
    if (rendering) return;
    rendering = true;

    const container = document.getElementById("pdf-container");
    container.innerHTML = "";

    if (await effectiveDual()) {
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

        progressLabel.textContent = `${pageNum}-${Math.min(pageNum + 1, pdf.numPages)} / ${pdf.numPages}`;
    } else {
        const canvas = document.createElement("canvas");
        container.appendChild(canvas);
        await drawPage(pageNum, canvas);
        progressLabel.textContent = `${pageNum} / ${pdf.numPages}`;
    }

    currentPage = pageNum;
    localStorage.setItem(`pdf-page-${uuid}`, currentPage);
    rendering = false;
}

async function init() {
    pdf = await pdfjsLib.getDocument(`/download/${uuid}/pdf`).promise;
    currentPage = Math.min(currentPage, pdf.numPages);
    await renderToc();
    if (fitToggle.checked) {
        await fitToScale();
    } else {
        renderPage(currentPage);
    }
}

init();

// update prev/next to step by 2 in dual mode
document.getElementById("prev-btn").addEventListener("click", () => {
    const step = isDual() ? 2 : 1;
    if (currentPage > 1) renderPage(Math.max(1, currentPage - step));
});

document.getElementById("next-btn").addEventListener("click", () => {
    const step = isDual() ? 2 : 1;
    if (currentPage < pdf?.numPages) renderPage(Math.min(pdf.numPages, currentPage + step));
});

window.addEventListener("keydown", (e) => {
    const step = isDual() ? 2 : 1;
    if (e.key === "ArrowLeft" && currentPage > 1) renderPage(Math.max(1, currentPage - step));
    if (e.key === "ArrowRight" && currentPage < pdf?.numPages) renderPage(Math.min(pdf.numPages, currentPage + step));
});
document.getElementById("font-up").addEventListener("click", () => {
    scale = Math.min(scale + 0.25, 4);
    localStorage.setItem("pdf-scale", scale);
    renderPage(currentPage);
});

document.getElementById("font-down").addEventListener("click", () => {
    scale = Math.max(scale - 0.25, 0.5);
    localStorage.setItem("pdf-scale", scale);
    renderPage(currentPage);
});

function availableHeight() {
    return window.innerHeight - document.querySelector(".top-bar").offsetHeight;
}

const fitToggle = document.getElementById("fit-toggle");
fitToggle.checked = localStorage.getItem("pdf-fit") === "true";

async function fitToScale() {
    const page = await pdf.getPage(currentPage);
    const viewport = page.getViewport({ scale: 1 });
    const divisor = lastWasDual ? 2 : 1;
    const heightScale = availableHeight() / viewport.height;
    const widthScale = (window.innerWidth / divisor - 8) / viewport.width;
    scale = Math.min(heightScale, widthScale);
    localStorage.setItem("pdf-scale", scale);
    await renderPage(currentPage);
}

async function applyFit() {
    if (fitToggle.checked) {
        localStorage.setItem("pdf-fit", "true");
        await fitToScale();
    } else {
        localStorage.setItem("pdf-fit", "false");
    }
}

fitToggle.addEventListener("change", applyFit);

// resize handler — only refit if fit mode is active
window.addEventListener("resize", () => {
    if (fitToggle.checked && pdf) fitToScale();
});

// disable zoom buttons when fit is active since fit overrides scale
document.getElementById("font-up").addEventListener("click", () => {
    fitToggle.checked = false;
    localStorage.setItem("pdf-fit", "false");
    scale = Math.min(scale + 0.25, 4);
    localStorage.setItem("pdf-scale", scale);
    renderPage(currentPage);
});

document.getElementById("font-down").addEventListener("click", () => {
    fitToggle.checked = false;
    localStorage.setItem("pdf-fit", "false");
    scale = Math.max(scale - 0.25, 0.5);
    localStorage.setItem("pdf-scale", scale);
    renderPage(currentPage);
});

async function renderToc() {
    const outline = await pdf.getOutline();
    const list = document.getElementById("toc-list");

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
                        ? await pdf.getDestination(item.dest)
                        : item.dest;

                    const pageIndex = await pdf.getPageIndex(dest[0]);
                    renderPage(pageIndex + 1); // pdf.js is 0-indexed
                }
                document.getElementById("toc-sidebar").style.display = "none";
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

document.getElementById("toc-btn").addEventListener("click", () => {
    const sidebar = document.getElementById("toc-sidebar");
    sidebar.style.display = sidebar.style.display === "none" ? "block" : "none";
});

// scroll to navigate
document.getElementById("pdf-container").addEventListener("wheel", (e) => {
    e.preventDefault();
    if (e.deltaY > 0) {
        // scroll down = next
        if (currentPage < pdf?.numPages) renderPage(Math.min(pdf.numPages, currentPage + (effectiveDualSync() ? 2 : 1)));
    } else {
        // scroll up = prev
        if (currentPage > 1) renderPage(Math.max(1, currentPage - (effectiveDualSync() ? 2 : 1)));
    }
}, { passive: false });

// click left/right half to navigate
document.getElementById("pdf-container").addEventListener("click", (e) => {
    const mid = window.innerWidth / 2;
    if (e.clientX < mid) {
        if (currentPage > 1) renderPage(Math.max(1, currentPage - (effectiveDualSync() ? 2 : 1)));
    } else {
        if (currentPage < pdf?.numPages) renderPage(Math.min(pdf.numPages, currentPage + (effectiveDualSync() ? 2 : 1)));
    }
});

let lastWasDual = false; // updated each renderPage
function effectiveDualSync() {
    return lastWasDual;
}