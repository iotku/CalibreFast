// Reusable Cover Picker component for upload and book-edit pages.
import { errorMessage } from "./dom.js";
/** A canvas's 2D context; these canvases are always 2D-capable. */
function ctx2d(canvas) {
    const ctx = canvas.getContext("2d");
    if (!ctx)
        throw new Error("2D canvas not supported");
    return ctx;
}
/**
 * Readable text blocks from an EPUB section. epub.js's section.load() gives
 * the section's root <html> element (not a Document), and EPUB XHTML is XML,
 * so there's no .body / .innerText: find <body> and read block elements.
 * Returns [{text, heading}].
 */
function sectionBlocks(root) {
    const body = root?.querySelector?.("body") || root;
    if (!body)
        return [];
    const blocks = [...body.querySelectorAll("h1,h2,h3,h4,h5,h6,p,li,blockquote,pre,dt,dd,figcaption,td")]
        .map((el) => ({
        text: (el.textContent || "").replace(/\s+/g, " ").trim(),
        heading: /^h[1-6]$/i.test(el.localName || el.tagName),
    }))
        .filter((b) => b.text);
    if (blocks.length)
        return blocks;
    const text = (body.textContent || "").replace(/\s+/g, " ").trim();
    return text ? [{ text, heading: false }] : [];
}
/**
 * Draw blocks onto a page-shaped canvas, word-wrapped to its width. The
 * first heading is bold and larger. Sizes are relative to the canvas width
 * so the thumbnail and the full-size cover look the same.
 */
function drawTextPage(ctx, width, height, blocks, fallbackTitle) {
    const margin = Math.round(width * 0.08);
    const maxWidth = width - margin * 2;
    const bodySize = Math.max(6, Math.round(width * 0.045));
    const headSize = Math.round(bodySize * 1.6);
    ctx.fillStyle = "#fcfaf7";
    ctx.fillRect(0, 0, width, height);
    ctx.fillStyle = "#1a1a1a";
    ctx.textBaseline = "top";
    const items = blocks.length ? blocks : [{ text: fallbackTitle, heading: true }];
    let y = margin;
    let firstHeadingDone = false;
    for (const block of items) {
        const isHead = block.heading && !firstHeadingDone;
        if (block.heading)
            firstHeadingDone = true;
        const size = isHead ? headSize : bodySize;
        ctx.font = `${isHead ? "bold " : ""}${size}px Georgia, serif`;
        const lineHeight = Math.round(size * 1.35);
        let line = "";
        for (const word of block.text.split(" ")) {
            const test = line ? `${line} ${word}` : word;
            if (ctx.measureText(test).width > maxWidth && line) {
                if (y + lineHeight > height - margin)
                    return;
                ctx.fillText(line, margin, y);
                y += lineHeight;
                line = word;
            }
            else {
                line = test;
            }
        }
        if (line) {
            if (y + lineHeight > height - margin)
                return;
            ctx.fillText(line, margin, y);
            y += lineHeight;
        }
        y += Math.round(lineHeight * (isHead ? 0.6 : 0.4)); // paragraph gap
    }
}
/** Loads a classic <script> once (the CDN libraries the picker needs). */
function loadScript(src, integrity) {
    return new Promise((resolve, reject) => {
        const s = document.createElement("script");
        s.src = src;
        if (integrity) {
            s.integrity = integrity;
            s.crossOrigin = "anonymous";
        }
        s.onload = () => resolve();
        s.onerror = () => reject(new Error(`failed to load ${src}`));
        document.head.appendChild(s);
    });
}
async function uploadCoverDataUrl(uuid, dataUrl) {
    const resp = await fetch(`/upload-cover/${uuid}`, {
        method: "POST",
        body: dataUrl,
    });
    if (!resp.ok)
        throw new Error(await resp.text());
    return resp.json();
}
async function uploadCoverFile(uuid, file) {
    const fd = new FormData();
    fd.append("cover", file);
    const resp = await fetch(`/upload-cover/${uuid}`, {
        method: "POST",
        body: fd,
    });
    if (!resp.ok)
        throw new Error(await resp.text());
    return resp.json();
}
async function revertCover(uuid) {
    const resp = await fetch(`/revert-cover/${uuid}`, {
        method: "POST",
    });
    if (!resp.ok)
        throw new Error(await resp.text());
    return resp.json();
}
async function fetchCoverStatus(uuid) {
    try {
        const resp = await fetch(`/cover-status/${uuid}`);
        if (resp.ok)
            return resp.json();
    }
    catch {
        /* ignore */
    }
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
        if (pagesEl)
            pagesEl.classList.add("visible");
        if (status)
            status.textContent = "Reverting cover…";
        try {
            await revertCover(uuid);
            showCoverDone(picker, "✓ Reverted to previous cover");
            if (status)
                status.textContent = "Restored previous cover.";
            picker.querySelectorAll(".cover-pages-strip canvas").forEach(c => c.classList.remove("selected"));
            if (onCoverSaved)
                onCoverSaved(uuid);
        }
        catch (err) {
            if (status)
                status.textContent = "Revert failed: " + errorMessage(err);
        }
        finally {
            revertBtn.disabled = false;
        }
    });
    fileIn.addEventListener("change", async () => {
        if (!fileIn.files?.length)
            return;
        const status = picker.querySelector(".cover-pages-status");
        const pagesEl = picker.querySelector(".cover-pages");
        if (pagesEl)
            pagesEl.classList.add("visible");
        if (status)
            status.textContent = "Uploading cover image…";
        try {
            const file = fileIn.files?.[0];
            if (!file)
                return;
            const res = await uploadCoverFile(uuid, file);
            showCoverDone(picker, "✓ Cover uploaded");
            if (status)
                status.textContent = "Cover uploaded successfully!";
            if (res && res.hasBackup)
                updateRevertBtn(true);
            if (onCoverSaved)
                onCoverSaved(uuid);
        }
        catch (err) {
            if (status)
                status.textContent = "Upload failed: " + errorMessage(err);
        }
        finally {
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
        const btn = pdfBtn;
        btn.addEventListener("click", () => togglePageStrip(btn, picker, uuid, "pdf", onCoverSaved, false, updateRevertBtn));
        actions.appendChild(pdfBtn);
    }
    if (hasEpub) {
        epubBtn = document.createElement("button");
        epubBtn.type = "button";
        epubBtn.textContent = "Pick from EPUB pages";
        const btn = epubBtn;
        btn.addEventListener("click", () => togglePageStrip(btn, picker, uuid, "epub", onCoverSaved, false, updateRevertBtn));
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
        }
        else if (hasPdf && pdfBtn) {
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
        }
        catch (_) {
            formats = [];
        }
    }
    const picker = buildCoverPicker(uuid, formats || [], onCoverSaved, options);
    container.appendChild(picker);
    return picker;
}
function togglePageStrip(btn, picker, uuid, format, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
    const pages = picker.querySelector(".cover-pages");
    const strip = pages?.querySelector(".cover-pages-strip");
    const status = pages?.querySelector(".cover-pages-status");
    if (!pages || !strip || !status)
        return;
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
    }
    else if (format === "epub") {
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
            canvas.style.height = "180px";
            canvas.style.maxHeight = "180px";
            canvas.style.width = "auto";
            canvas.style.maxWidth = "140px";
            canvas.style.objectFit = "contain";
            await page.render({
                canvasContext: ctx2d(canvas),
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
                        canvasContext: ctx2d(hiresCanvas),
                        viewport: hiresViewport,
                    }).promise;
                    const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
                    const res = await uploadCoverDataUrl(uuid, dataUrl);
                    if (isDefault) {
                        showCoverDone(picker, "✓ Default cover saved from page 1");
                        status.textContent = "✓ Default cover saved from page 1 (click another to change)";
                    }
                    else {
                        showCoverDone(picker, "✓ Cover saved");
                        status.textContent = "Cover saved!";
                    }
                    if (updateRevertBtn && res && res.hasBackup)
                        updateRevertBtn(true);
                    if (onCoverSaved)
                        onCoverSaved(uuid);
                }
                catch (err) {
                    status.textContent = "Failed to save: " + errorMessage(err);
                }
            };
            canvas.addEventListener("click", () => saveHiresCover(false));
            strip.appendChild(canvas);
            // Auto-save page 1 on upload
            if (i === 1 && autoSelect) {
                saveHiresCover(true);
            }
        }
    }
    catch (e) {
        status.textContent = "Failed to load PDF pages: " + errorMessage(e);
    }
}
async function renderEpubPages(uuid, strip, status, picker, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
    try {
        // JSZip first: epub.js opens the whole .epub from memory with it.
        if (!("JSZip" in window)) {
            await loadScript("https://cdn.jsdelivr.net/npm/jszip@3.10.2/dist/jszip.min.js", "sha384-DdGlNq+wVXAn83gXqwyeFHB7iyXy6L0rLrNzgDmhKWPjiJLg9vj6qosYmZmkMfQ+");
        }
        if (typeof window.ePub === "undefined") {
            await loadScript("https://cdn.jsdelivr.net/npm/epubjs@0.3.93/dist/epub.min.js");
        }
        const res = await fetch(`/download/${encodeURIComponent(uuid)}/epub`, { credentials: "same-origin" });
        if (!res.ok)
            throw new Error(`HTTP ${res.status}`);
        const book = ePub(await res.arrayBuffer());
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
                if (shouldAuto)
                    defaultSaved = true;
            }
        }
        catch {
            /* ignore */
        }
        // 2. First 5 spine sections
        const spine = book.spine;
        const count = Math.min(spine.length || 0, 5);
        for (let i = 0; i < count; i++) {
            const section = spine.get(i);
            if (!section)
                continue;
            try {
                const doc = await section.load(book.load.bind(book));
                if (!doc)
                    continue;
                const shouldAuto = autoSelect && !defaultSaved;
                // Look for image in section
                const imgEl = doc.querySelector("img, image");
                if (imgEl) {
                    const rawSrc = imgEl.getAttribute("src") || imgEl.getAttribute("href") || imgEl.getAttribute("xlink:href");
                    if (rawSrc) {
                        // Resolve against the section's path inside the book; external
                        // and data: URLs are left alone.
                        const resolved = new URL(rawSrc, `epub:${section.url}`);
                        const fullUrl = resolved.protocol === "epub:" ? await book.archive.createUrl(resolved.pathname, { base64: false }) : rawSrc;
                        renderImageThumb(uuid, fullUrl, `Section ${i + 1}`, strip, picker, onCoverSaved, shouldAuto, updateRevertBtn);
                        hasAnyPreview = true;
                        if (shouldAuto)
                            defaultSaved = true;
                        continue;
                    }
                }
                // Fallback text page preview
                renderTextPageThumb(uuid, doc, `Section ${i + 1}`, strip, picker, onCoverSaved, shouldAuto, updateRevertBtn);
                hasAnyPreview = true;
                if (shouldAuto)
                    defaultSaved = true;
            }
            catch (err) {
                console.warn("Could not load EPUB section", i, err);
            }
        }
        if (!autoSelect) {
            status.textContent = hasAnyPreview
                ? "Click any page to use as cover"
                : "No previewable pages found in EPUB.";
        }
    }
    catch (e) {
        status.textContent = "Failed to load EPUB: " + errorMessage(e);
    }
}
function renderImageThumb(uuid, imgUrl, title, strip, picker, onCoverSaved, autoSelect = false, updateRevertBtn = null) {
    const canvas = document.createElement("canvas");
    canvas.title = title;
    canvas.style.cursor = "pointer";
    canvas.style.height = "180px";
    canvas.style.maxHeight = "180px";
    canvas.style.width = "auto";
    canvas.style.maxWidth = "140px";
    canvas.style.objectFit = "contain";
    strip.appendChild(canvas);
    const status = picker.querySelector(".cover-pages-status");
    const doSave = async (isDefault = false) => {
        strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
        canvas.classList.add("selected");
        if (status)
            status.textContent = isDefault ? "Saving default cover…" : "Saving cover…";
        try {
            const hiresCanvas = document.createElement("canvas");
            const maxW = 1200;
            const w = img.naturalWidth || img.width || 800;
            const h = img.naturalHeight || img.height || 1130;
            const s = w > maxW ? maxW / w : 1;
            hiresCanvas.width = Math.round(w * s);
            hiresCanvas.height = Math.round(h * s);
            const hctx = ctx2d(hiresCanvas);
            hctx.drawImage(img, 0, 0, hiresCanvas.width, hiresCanvas.height);
            const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
            const res = await uploadCoverDataUrl(uuid, dataUrl);
            if (isDefault) {
                showCoverDone(picker, "✓ Default cover saved from page 1");
                if (status)
                    status.textContent = "✓ Default cover saved from page 1 (click another to change)";
            }
            else {
                showCoverDone(picker, "✓ Cover saved");
                if (status)
                    status.textContent = "Cover saved!";
            }
            if (updateRevertBtn && res && res.hasBackup)
                updateRevertBtn(true);
            if (onCoverSaved)
                onCoverSaved(uuid);
        }
        catch (err) {
            if (status)
                status.textContent = "Failed to save: " + errorMessage(err);
        }
    };
    const img = new Image();
    img.crossOrigin = "anonymous";
    img.onload = () => {
        const targetHeight = 180;
        const scale = targetHeight / (img.height || 180);
        canvas.height = targetHeight;
        canvas.width = Math.max(50, Math.round((img.width || 120) * scale));
        const ctx = ctx2d(canvas);
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
    canvas.style.height = "180px";
    canvas.style.maxHeight = "180px";
    canvas.style.width = "auto";
    canvas.style.maxWidth = "140px";
    canvas.style.objectFit = "contain";
    const ctx = ctx2d(canvas);
    const blocks = sectionBlocks(doc);
    drawTextPage(ctx, 127, 180, blocks, title);
    strip.appendChild(canvas);
    const status = picker.querySelector(".cover-pages-status");
    const doSave = async (isDefault = false) => {
        strip.querySelectorAll("canvas").forEach(c => c.classList.remove("selected"));
        canvas.classList.add("selected");
        if (status)
            status.textContent = isDefault ? "Saving default cover…" : "Saving cover…";
        try {
            const hiresCanvas = document.createElement("canvas");
            hiresCanvas.width = 800;
            hiresCanvas.height = 1130;
            const hctx = ctx2d(hiresCanvas);
            drawTextPage(hctx, 800, 1130, blocks, title);
            const dataUrl = hiresCanvas.toDataURL("image/jpeg", 0.92);
            const res = await uploadCoverDataUrl(uuid, dataUrl);
            if (isDefault) {
                showCoverDone(picker, "✓ Default cover saved from page 1");
                if (status)
                    status.textContent = "✓ Default cover saved from page 1 (click another to change)";
            }
            else {
                showCoverDone(picker, "✓ Cover saved");
                if (status)
                    status.textContent = "Cover saved!";
            }
            if (updateRevertBtn && res && res.hasBackup)
                updateRevertBtn(true);
            if (onCoverSaved)
                onCoverSaved(uuid);
        }
        catch (err) {
            if (status)
                status.textContent = "Failed to save: " + errorMessage(err);
        }
    };
    canvas.addEventListener("click", () => doSave(false));
    if (autoSelect) {
        doSave(true);
    }
}
