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
    renderPage(currentPage);
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

async function fitToHeight() {
    const page = await pdf.getPage(currentPage);
    const viewport = page.getViewport({ scale: 1 });
    scale = availableHeight() / viewport.height;
    localStorage.setItem("pdf-scale", scale);
    renderPage(currentPage);
}

document.getElementById("fit-btn").addEventListener("click", fitToHeight);