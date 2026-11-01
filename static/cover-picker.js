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

async function revertCover(uuid) {
    const resp = await fetch(`/revert-cover/${uuid}`, {
        method: "POST",
    });
    if (!resp.ok) throw new Error(await resp.text());
    return resp.json();
}

async function fetchCoverStatus(uuid) {
    try {
        const resp = await fetch(`/cover-status/${uuid}`);
        if (resp.ok) return resp.json();
    } catch (_) {}
    return { hasCover: false, hasBackup: false };
}

function showCoverDone(picker, msg = "✓ Cover updated") {
    const done = picker.querySelector(".cover-picker-done");
    if (done) {
        done.textContent = msg;
        done.classList.add("visible");
        setTimeout(() => done.classList.remove("visible"), 4000);
    }
}

export function buildCoverPicker(uuid, formats = [], onCoverSaved = null, options = {}) {
    const isEditPage = Boolean(options.isEditPage);
    const autoSelectDefault = Boolean(options.autoSelectDefault);

    const picker = document.createElement("div");
    picker.className = "cover-picker";

    const header = document.createElement("div");
    header.className = "cover-picker-header";
    header.innerHTML = `
        <h3>Cover Image</h3>
        <span class="cover-picker-done">✓ Cover updated</span>
    `;
    picker.appendChild(header);

    // Edit page helper notice
    if (isEditPage) {
        const hint = document.createElement("div");
        hint.className = "cover-auto-save-hint";
        hint.innerHTML = `<span>⚡</span> <span>Selecting a page or uploading an image saves the new cover automatically on click.</span>`;
        picker.appendChild(hint);
    }

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

    // Revert button
    const revertBtn = document.createElement("button");
    revertBtn.type = "button";
    revertBtn.className = "btn-revert-cover";
    revertBtn.textContent = "↺ Revert to previous cover";
    revertBtn.style.display = "none";
    actions.appendChild(revertBtn);

    const updateRevertBtn = (hasBackup) => {
        if (isEditPage && hasBackup) {
            revertBtn.style.display = "inline-flex";
        }
    };

    if (isEditPage) {
        fetchCoverStatus(uuid).then(status => {
            if (status && status.hasBackup) {
                updateRevertBtn(true);
            }
        });
    }

    revertBtn.addEventListener("click", async () => {
        revertBtn.disabled = true;
        const status = picker.querySelector(".cover-pages-status");
        const pagesEl = picker.querySelector(".cover-pages");
        if (pagesEl) pagesEl.classList.add("visible");
        if (status) status.textContent = "Reverting cover…";

        try {
            await revertCover(uuid);
            showCoverDone(picker, "✓ Reverted to previous cover");
            if (status) status.textContent = "Restored previous cover.";
            picker.querySelectorAll(".cover-pages-strip canvas").forEach(c => c.classList.remove("selected"));
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Revert failed: " + err.message;
        } finally {
            revertBtn.disabled = false;
        }
    });

    fileIn.addEventListener("change", async () => {
        if (!fileIn.files.length) return;
        const status = picker.querySelector(".cover-pages-status");
        const pagesEl = picker.querySelector(".cover-pages");
        if (pagesEl) pagesEl.classList.add("visible");
        if (status) status.textContent = "Uploading cover image…";
        try {
            const res = await uploadCoverFile(uuid, fileIn.files[0]);
            showCoverDone(picker, "✓ Cover uploaded");
            if (status) status.textContent = "Cover uploaded successfully!";
            if (res && res.hasBackup) updateRevertBtn(true);
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Upload failed: " + err.message;
        } finally {
            fileIn.value = "";
        }
    });

    const hasPdf = formats.includes("pdf");
    const hasEpub = formats.includes("epub");

    let pdfBtn = null;
    let epubBtn = null;

    if (hasPdf) {
        pdfBtn = document.createElement("button");
        pdfBtn.type = "button";
        pdfBtn.textContent = "Pick from PDF pages";
        pdfBtn.addEventListener("click", () => togglePageStrip(pdfBtn, picker, uuid, "pdf", onCoverSaved, false, updateRevertBtn));
        actions.appendChild(pdfBtn);
    }

    if (hasEpub) {
        epubBtn = document.createElement("button");
        epubBtn.type = "button";
        epubBtn.textContent = "Pick from EPUB pages";
        epubBtn.addEventListener("click", () => togglePageStrip(epubBtn, picker, uuid, "epub", onCoverSaved, false, updateRevertBtn));
        actions.appendChild(epubBtn);
    }

    picker.appendChild(actions);

    const pages = document.createElement("div");
    pages.className = "cover-pages";
    pages.innerHTML = `
        <div class="cover-pages-strip"></div>
        <div class="cover-pages-status">Loading pages…</div>
    `;
    picker.appendChild(pages);

    // Auto-select default cover from page 1 on upload
    if (autoSelectDefault) {
        if (hasEpub && epubBtn) {
            togglePageStrip(epubBtn, picker, uuid, "epub", onCoverSaved, true, updateRevertBtn);
        } else if (hasPdf && pdfBtn) {
            togglePageStrip(pdfBtn, picker, uuid, "pdf", onCoverSaved, true, updateRevertBtn);
        }
    }

    return picker;
}

export async function mountCoverPicker(container, uuid, formats = null, onCoverSaved = null, options = {}) {
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
    const picker = buildCoverPicker(uuid, formats || [], onCoverSaved, options);
    container.appendChild(picker);
    return picker;
}

function togglePageStrip(btn, picker, uuid, format, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
    const pages = picker.querySelector(".cover-pages");
    const strip = pages.querySelector(".cover-pages-strip");
    const status = pages.querySelector(".cover-pages-status");

    const allBtns = picker.querySelectorAll(".cover-picker-actions button:not(.btn-revert-cover)");
    const wasActive = btn.classList.contains("active");

    if (wasActive && !autoSelect) {
        allBtns.forEach(b => b.classList.remove("active"));
        pages.classList.remove("visible");
        return;
    }

    allBtns.forEach(b => b.classList.remove("active"));
    btn.classList.add("active");
    strip.innerHTML = "";
    status.textContent = autoSelect ? "Saving default cover from page 1…" : "Loading pages…";
    pages.classList.add("visible");

    if (format === "pdf") {
        renderPdfPages(uuid, strip, status, picker, onCoverSaved, autoSelect, updateRevertBtn);
    } else if (format === "epub") {
        renderEpubPages(uuid, strip, status, picker, onCoverSaved, autoSelect, updateRevertBtn);
    }
}

async function renderPdfPages(uuid, strip, status, picker, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
    try {
        const pdfjsLib = await import("https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.mjs");
        pdfjsLib.GlobalWorkerOptions.workerSrc = "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.worker.mjs";

        const pdf = await pdfjsLib.getDocument(`/download/${uuid}/pdf`).promise;
        const pageCount = Math.min(pdf.numPages, 5);
        status.textContent = autoSelect
            ? "Saving default cover from page 1…"
            : `Showing first ${pageCount} page(s) — click to select cover`;

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

            const saveHiresCover = async (isDefault = false) => {
                strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
                canvas.classList.add("selected");
                status.textContent = isDefault ? "Saving default cover from page 1…" : "Saving cover…";

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
                    const res = await uploadCoverDataUrl(uuid, dataUrl);
                    if (isDefault) {
                        showCoverDone(picker, "✓ Default cover saved from page 1");
                        status.textContent = "✓ Default cover saved from page 1 (click another to change)";
                    } else {
                        showCoverDone(picker, "✓ Cover saved");
                        status.textContent = "Cover saved!";
                    }
                    if (updateRevertBtn && res && res.hasBackup) updateRevertBtn(true);
                    if (onCoverSaved) onCoverSaved(uuid);
                } catch (err) {
                    status.textContent = "Failed to save: " + err.message;
                }
            };

            canvas.addEventListener("click", () => saveHiresCover(false));
            strip.appendChild(canvas);

            // Auto-save page 1 on upload
            if (i === 1 && autoSelect) {
                saveHiresCover(true);
            }
        }
    } catch (e) {
        status.textContent = "Failed to load PDF pages: " + e.message;
    }
}

async function renderEpubPages(uuid, strip, status, picker, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
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
        let defaultSaved = false;

        // 1. Try declared cover
        try {
            const coverUrl = await book.coverUrl();
            if (coverUrl) {
                const shouldAuto = autoSelect && !defaultSaved;
                renderImageThumb(uuid, coverUrl, "EPUB Cover", strip, picker, onCoverSaved, shouldAuto, updateRevertBtn);
                hasAnyPreview = true;
                if (shouldAuto) defaultSaved = true;
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

                const shouldAuto = autoSelect && !defaultSaved;

                // Look for image in section
                const imgEl = doc.querySelector("img, image");
                if (imgEl) {
                    const rawSrc = imgEl.getAttribute("src") || imgEl.getAttribute("href") || imgEl.getAttribute("xlink:href");
                    if (rawSrc) {
                        const resolvedPath = resolveEpubPath(section.href, rawSrc);
                        const fullUrl = `/epub/${uuid}/${resolvedPath}`;
                        renderImageThumb(uuid, fullUrl, `Section ${i + 1}`, strip, picker, onCoverSaved, shouldAuto, updateRevertBtn);
                        hasAnyPreview = true;
                        if (shouldAuto) defaultSaved = true;
                        continue;
                    }
                }

                // Fallback text page preview
                renderTextPageThumb(uuid, doc, `Section ${i + 1}`, strip, picker, onCoverSaved, shouldAuto, updateRevertBtn);
                hasAnyPreview = true;
                if (shouldAuto) defaultSaved = true;
            } catch (err) {
                console.warn("Could not load EPUB section", i, err);
            }
        }

        if (!autoSelect) {
            status.textContent = hasAnyPreview
                ? "Click any page to use as cover"
                : "No previewable pages found in EPUB.";
        }
    } catch (e) {
        status.textContent = "Failed to load EPUB: " + e.message;
    }
}

function renderImageThumb(uuid, imgUrl, title, strip, picker, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
    const canvas = document.createElement("canvas");
    canvas.title = title;
    canvas.style.cursor = "pointer";
    strip.appendChild(canvas);

    const status = picker.querySelector(".cover-pages-status");

    const doSave = async (isDefault = false) => {
        strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
        canvas.classList.add("selected");
        if (status) status.textContent = isDefault ? "Saving default cover…" : "Saving cover…";

        try {
            const hiresCanvas = document.createElement("canvas");
            const maxW = 1200;
            const w = img.naturalWidth || img.width || 800;
            const h = img.naturalHeight || img.height || 1130;
            const s = w > maxW ? maxW / w : 1;
            hiresCanvas.width = Math.round(w * s);
            hiresCanvas.height = Math.round(h * s);
            const hctx = hiresCanvas.getContext("2d");
            hctx.drawImage(img, 0, 0, hiresCanvas.width, hiresCanvas.height);

            const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
            const res = await uploadCoverDataUrl(uuid, dataUrl);
            if (isDefault) {
                showCoverDone(picker, "✓ Default cover saved from page 1");
                if (status) status.textContent = "✓ Default cover saved from page 1 (click another to change)";
            } else {
                showCoverDone(picker, "✓ Cover saved");
                if (status) status.textContent = "Cover saved!";
            }
            if (updateRevertBtn && res && res.hasBackup) updateRevertBtn(true);
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Failed to save: " + err.message;
        }
    };

    const img = new Image();
    img.crossOrigin = "anonymous";
    img.onload = () => {
        const targetHeight = 180;
        const scale = targetHeight / (img.height || 180);
        canvas.height = targetHeight;
        canvas.width = Math.max(50, Math.round((img.width || 120) * scale));
        const ctx = canvas.getContext("2d");
        ctx.drawImage(img, 0, 0, canvas.width, canvas.height);

        if (autoSelect) {
            doSave(true);
        }
    };
    img.onerror = () => {
        canvas.remove();
    };
    img.src = imgUrl;

    canvas.addEventListener("click", () => doSave(false));
}

function renderTextPageThumb(uuid, doc, title, strip, picker, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
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

    const status = picker.querySelector(".cover-pages-status");

    const doSave = async (isDefault = false) => {
        strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
        canvas.classList.add("selected");
        if (status) status.textContent = isDefault ? "Saving default cover…" : "Saving cover…";

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
            const res = await uploadCoverDataUrl(uuid, dataUrl);
            if (isDefault) {
                showCoverDone(picker, "✓ Default cover saved from page 1");
                if (status) status.textContent = "✓ Default cover saved from page 1 (click another to change)";
            } else {
                showCoverDone(picker, "✓ Cover saved");
                if (status) status.textContent = "Cover saved!";
            }
            if (updateRevertBtn && res && res.hasBackup) updateRevertBtn(true);
            if (onCoverSaved) onCoverSaved(uuid);
        } catch (err) {
            if (status) status.textContent = "Failed to save: " + err.message;
        }
    };

    canvas.addEventListener("click", () => doSave(false));

    if (autoSelect) {
        doSave(true);
    }
}
