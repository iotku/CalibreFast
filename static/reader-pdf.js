import * as pdfjsLib from "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.mjs";

pdfjsLib.GlobalWorkerOptions.workerSrc = "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.worker.mjs";

const params = new URLSearchParams(window.location.search);
const uuid = params.get("uuid");

if (!uuid) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}

const canvas = document.getElementById("pdf-canvas");
const ctx = canvas.getContext("2d");
const progressLabel = document.getElementById("progress-label");

let pdf = null;
let currentPage = parseInt(localStorage.getItem(`pdf-page-${uuid}`) || "1");
let scale = parseFloat(localStorage.getItem("pdf-scale") || "1.5");
let rendering = false;

async function renderPage(pageNum) {
    if (rendering) return;
    rendering = true;

    const page = await pdf.getPage(pageNum);
    const viewport = page.getViewport({ scale });

    canvas.width = viewport.width;
    canvas.height = viewport.height;

    await page.render({ canvasContext: ctx, viewport }).promise;

    currentPage = pageNum;
    localStorage.setItem(`pdf-page-${uuid}`, currentPage);
    progressLabel.textContent = `${currentPage} / ${pdf.numPages}`;
    rendering = false;
}

async function init() {
    pdf = await pdfjsLib.getDocument(`/download/${uuid}/pdf`).promise;
    currentPage = Math.min(currentPage, pdf.numPages);
    await renderToc();
    if (fitToggle.checked) {
        await fitToHeight();
    } else {
        renderPage(currentPage);
    }
}

init();

document.getElementById("prev-btn").addEventListener("click", () => {
    if (currentPage > 1) renderPage(currentPage - 1);
});

document.getElementById("next-btn").addEventListener("click", () => {
    if (currentPage < pdf?.numPages) renderPage(currentPage + 1);
});

window.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft" && currentPage > 1) renderPage(currentPage - 1);
    if (e.key === "ArrowRight" && currentPage < pdf?.numPages) renderPage(currentPage + 1);
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

async function fitToHeight() {
    const page = await pdf.getPage(currentPage);
    const viewport = page.getViewport({ scale: 1 });
    scale = availableHeight() / viewport.height;
    localStorage.setItem("pdf-scale", scale);
    renderPage(currentPage);
}

async function applyFit() {
    if (fitToggle.checked) {
        localStorage.setItem("pdf-fit", "true");
        await fitToHeight();
    } else {
        localStorage.setItem("pdf-fit", "false");
    }
}

fitToggle.addEventListener("change", applyFit);

// resize handler — only refit if fit mode is active
window.addEventListener("resize", () => {
    if (fitToggle.checked && pdf) fitToHeight();
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
