// Reusable Cover Picker component for upload and book-edit pages.

function resolveEpubPath(baseHref, rel) {
    if (!rel || rel.startsWith("http://") || rel.startsWith("https://") || rel.startsWith("data:")) {
        return rel;
    }
    if (rel.startsWith("/")) {
        return rel.slice(1);
    }
    const parts = (baseHref || "").split("/");
    parts.pop(); // remove filename
    for (const seg of rel.split("/")) {
        if (seg === "." || seg === "") continue;
        if (seg === "..") {
            if (parts.length > 0) parts.pop();
        } else {
            parts.push(seg);
        }
    }
    return parts.join("/");
}

async function uploadCoverDataUrl(uuid, dataUrl) {
    const resp = await fetch(`/upload-cover/${uuid}`, {
        method: "POST",
        body: dataUrl,
    });
    if (!resp.ok) throw new Error(await resp.text());
    return resp.json();
}

async function uploadCoverFile(uuid, file) {
    const fd = new FormData();
    fd.append("cover", file);
    const resp = await fetch(`/upload-cover/${uuid}`, {
        method: "POST",
        body: fd,
    });
    if (!resp.ok) throw new Error(await resp.text());
    return resp.json();
}

function showCoverDone(picker) {
    const done = picker.querySelector(".cover-picker-done");
    if (done) {
        done.classList.add("visible");
        setTimeout(() => done.classList.remove("visible"), 4000);
    }
}

export function buildCoverPicker(uuid, formats, onCoverSaved) {
    const picker = document.createElement("div");
    picker.className = "cover-picker";

    const header = document.createElement("div");
    header.className = "cover-picker-header";
    header.innerHTML = `
        <h3>Cover Image</h3>
        <span class="cover-picker-done">✓ Cover updated</span>
    `;
    picker.appendChild(header);

    const actions = document.createElement("div");
    actions.className = "cover-picker-actions";

    // Upload image input
    const uploadLabel = document.createElement("label");
    uploadLabel.textContent = "Upload image (JPG/PNG)";
    const fileIn = document.createElement("input");
    fileIn.type = "file";
    fileIn.accept = ".jpg,.jpeg,.png";
    uploadLabel.appendChild(fileIn);
    actions.appendChild(uploadLabel);

    fileIn.addEventListener("change", async () => {
        if (!fileIn.files.length) return;
        const status = picker.querySelector(".cover-pages-status");
        const pagesEl = picker.querySelector(".cover-pages");
        if (pagesEl) pagesEl.classList.add("visible");
        if (status) status.textContent = "Uploading cover image…";
        try {
            await uploadCoverFile(uuid, fileIn.files[0]);
            showCoverDone(picker);
            if (status) status.textContent = "Cover uploaded successfully!";
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Upload failed: " + err.message;
        } finally {
            fileIn.value = "";
        }
    });

    const hasPdf = formats.includes("pdf");
    const hasEpub = formats.includes("epub");

    if (hasPdf) {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = "Pick from PDF pages";
        btn.addEventListener("click", () => togglePageStrip(btn, picker, uuid, "pdf", onCoverSaved));
        actions.appendChild(btn);
    }

    if (hasEpub) {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = "Pick from EPUB pages";
        btn.addEventListener("click", () => togglePageStrip(btn, picker, uuid, "epub", onCoverSaved));
        actions.appendChild(btn);
    }

    picker.appendChild(actions);

    const pages = document.createElement("div");
    pages.className = "cover-pages";
    pages.innerHTML = `
        <div class="cover-pages-strip"></div>
        <div class="cover-pages-status">Loading pages…</div>
    `;
    picker.appendChild(pages);

    return picker;
}

export async function mountCoverPicker(container, uuid, formats, onCoverSaved) {
    if (!formats || formats.length === 0) {
        try {
            const res = await fetch(`/formats/${uuid}`);
            if (res.ok) {
                formats = await res.json();
            }
        } catch (_) {
            formats = [];
        }
    }
    const picker = buildCoverPicker(uuid, formats || [], onCoverSaved);
    container.appendChild(picker);
    return picker;
}

function togglePageStrip(btn, picker, uuid, format, onCoverSaved) {
    const pages = picker.querySelector(".cover-pages");
    const strip = pages.querySelector(".cover-pages-strip");
    const status = pages.querySelector(".cover-pages-status");

    const allBtns = picker.querySelectorAll(".cover-picker-actions button");
    const wasActive = btn.classList.contains("active");
    allBtns.forEach(b => b.classList.remove("active"));

    if (wasActive) {
        pages.classList.remove("visible");
        return;
    }

    btn.classList.add("active");
    strip.innerHTML = "";
    status.textContent = "Loading pages…";
    pages.classList.add("visible");

    if (format === "pdf") {
        renderPdfPages(uuid, strip, status, picker, onCoverSaved);
    } else if (format === "epub") {
        renderEpubPages(uuid, strip, status, picker, onCoverSaved);
    }
}

async function renderPdfPages(uuid, strip, status, picker, onCoverSaved) {
    try {
        const pdfjsLib = await import("https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.mjs");
        pdfjsLib.GlobalWorkerOptions.workerSrc = "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.worker.mjs";

        const pdf = await pdfjsLib.getDocument(`/download/${uuid}/pdf`).promise;
        const pageCount = Math.min(pdf.numPages, 5);
        status.textContent = `Showing first ${pageCount} page(s) — click to select cover`;

        for (let i = 1; i <= pageCount; i++) {
            const page = await pdf.getPage(i);
            const viewport = page.getViewport({ scale: 1 });

            const targetHeight = 180;
            const scale = targetHeight / viewport.height;
            const scaledViewport = page.getViewport({ scale });

            const canvas = document.createElement("canvas");
            canvas.width = scaledViewport.width;
            canvas.height = scaledViewport.height;
            canvas.title = `Page ${i}`;

            await page.render({
                canvasContext: canvas.getContext("2d"),
                viewport: scaledViewport,
            }).promise;

            canvas.addEventListener("click", async () => {
                strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
                canvas.classList.add("selected");
                status.textContent = "Saving cover…";

                try {
                    const maxWidth = 1200;
                    const hiresScale = Math.min(maxWidth / viewport.width, 3);
                    const hiresViewport = page.getViewport({ scale: hiresScale });

                    const hiresCanvas = document.createElement("canvas");
                    hiresCanvas.width = hiresViewport.width;
                    hiresCanvas.height = hiresViewport.height;

                    await page.render({
                        canvasContext: hiresCanvas.getContext("2d"),
                        viewport: hiresViewport,
                    }).promise;

                    const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
                    await uploadCoverDataUrl(uuid, dataUrl);
                    showCoverDone(picker);
                    status.textContent = "Cover saved!";
                    if (onCoverSaved) onCoverSaved(uuid);
                } catch (err) {
                    status.textContent = "Failed to save: " + err.message;
                }
            });

            strip.appendChild(canvas);
        }
    } catch (e) {
        status.textContent = "Failed to load PDF pages: " + e.message;
    }
}

async function renderEpubPages(uuid, strip, status, picker, onCoverSaved) {
    try {
        if (typeof window.ePub === "undefined") {
            await new Promise((resolve, reject) => {
                const s = document.createElement("script");
                s.src = "https://cdn.jsdelivr.net/npm/epubjs@0.3.93/dist/epub.min.js";
                s.onload = resolve;
                s.onerror = reject;
                document.head.appendChild(s);
            });
        }

        const book = ePub(`/epub/${uuid}/`);
        await book.ready;

        let hasAnyPreview = false;

        // 1. Try declared cover
        try {
            const coverUrl = await book.coverUrl();
            if (coverUrl) {
                renderImageThumb(uuid, coverUrl, "EPUB Cover", strip, picker, onCoverSaved);
                hasAnyPreview = true;
            }
        } catch (_) {}

        // 2. First 5 spine sections
        const spine = book.spine || [];
        const count = Math.min(spine.length || 0, 5);

        for (let i = 0; i < count; i++) {
            const section = spine.get ? spine.get(i) : spine[i];
            if (!section) continue;

            try {
                const doc = await section.load(book.load.bind(book));
                if (!doc) continue;

                // Look for image in section
                const imgEl = doc.querySelector("img, image");
                if (imgEl) {
                    const rawSrc = imgEl.getAttribute("src") || imgEl.getAttribute("href") || imgEl.getAttribute("xlink:href");
                    if (rawSrc) {
                        const resolvedPath = resolveEpubPath(section.href, rawSrc);
                        const fullUrl = `/epub/${uuid}/${resolvedPath}`;
                        renderImageThumb(uuid, fullUrl, `Section ${i + 1}`, strip, picker, onCoverSaved);
                        hasAnyPreview = true;
                        continue;
                    }
                }

                // Fallback text page preview
                renderTextPageThumb(uuid, doc, `Section ${i + 1}`, strip, picker, onCoverSaved);
                hasAnyPreview = true;
            } catch (err) {
                console.warn("Could not load EPUB section", i, err);
            }
        }

        status.textContent = hasAnyPreview
            ? "Click any page to use as cover"
            : "No previewable pages found in EPUB.";
    } catch (e) {
        status.textContent = "Failed to load EPUB: " + e.message;
    }
}

function renderImageThumb(uuid, imgUrl, title, strip, picker, onCoverSaved) {
    const canvas = document.createElement("canvas");
    canvas.title = title;
    canvas.style.cursor = "pointer";
    strip.appendChild(canvas);

    const img = new Image();
    img.crossOrigin = "anonymous";
    img.onload = () => {
        const targetHeight = 180;
        const scale = targetHeight / (img.height || 180);
        canvas.height = targetHeight;
        canvas.width = Math.max(50, Math.round((img.width || 120) * scale));
        const ctx = canvas.getContext("2d");
        ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
    };
    img.onerror = () => {
        canvas.remove();
    };
    img.src = imgUrl;

    canvas.addEventListener("click", async () => {
        strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
        canvas.classList.add("selected");
        const status = picker.querySelector(".cover-pages-status");
        if (status) status.textContent = "Saving cover…";

        try {
            const hiresCanvas = document.createElement("canvas");
            const maxW = 1200;
            const s = (img.naturalWidth || img.width) > maxW ? maxW / (img.naturalWidth || img.width) : 1;
            hiresCanvas.width = Math.round((img.naturalWidth || img.width) * s);
            hiresCanvas.height = Math.round((img.naturalHeight || img.height) * s);
            const hctx = hiresCanvas.getContext("2d");
            hctx.drawImage(img, 0, 0, hiresCanvas.width, hiresCanvas.height);

            const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
            await uploadCoverDataUrl(uuid, dataUrl);
            showCoverDone(picker);
            if (status) status.textContent = "Cover saved!";
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Failed to save: " + err.message;
        }
    });
}

function renderTextPageThumb(uuid, doc, title, strip, picker, onCoverSaved) {
    const canvas = document.createElement("canvas");
    canvas.width = 127;
    canvas.height = 180;
    canvas.title = title;
    canvas.style.cursor = "pointer";
    const ctx = canvas.getContext("2d");

    ctx.fillStyle = "#fcfaf7";
    ctx.fillRect(0, 0, 127, 180);
    ctx.fillStyle = "#222";
    ctx.font = "bold 9px serif";

    const text = (doc.body ? doc.body.innerText : "") || title;
    const lines = text.split("\n").map(l => l.trim()).filter(Boolean);
    let y = 24;
    for (const line of lines.slice(0, 8)) {
        ctx.fillText(line.slice(0, 24), 10, y);
        y += 14;
        ctx.font = "7px serif";
    }
    strip.appendChild(canvas);

    canvas.addEventListener("click", async () => {
        strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
        canvas.classList.add("selected");
        const status = picker.querySelector(".cover-pages-status");
        if (status) status.textContent = "Saving cover…";

        try {
            const hiresCanvas = document.createElement("canvas");
            hiresCanvas.width = 800;
            hiresCanvas.height = 1130;
            const hctx = hiresCanvas.getContext("2d");
            hctx.fillStyle = "#fcfaf7";
            hctx.fillRect(0, 0, 800, 1130);
            hctx.fillStyle = "#111";
            hctx.font = "bold 32px serif";
            let hy = 120;
            for (const line of lines.slice(0, 15)) {
                hctx.fillText(line.slice(0, 45), 60, hy);
                hy += 44;
                hctx.font = "20px serif";
            }
            const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
            await uploadCoverDataUrl(uuid, dataUrl);
            showCoverDone(picker);
            if (status) status.textContent = "Cover saved!";
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Failed to save: " + err.message;
        }
    });
}

