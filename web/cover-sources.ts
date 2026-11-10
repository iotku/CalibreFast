// Where a book's cover can come from: its current cover, the first pages of
// its PDF or EPUB, an image found online or uploaded. Each is a CoverChoice
// that knows its size and saves itself as the cover. Used by the upload
// page's strip (cover-picker.ts) and the editor's dialog (cover-dialog.ts).

import { EPUBJS_URL, JSZIP_URL, openPdf } from "./vendor.js";

/** Response of /upload-cover, /revert-cover and /cover-status. */
export interface CoverStatus {
    hasCover?: boolean;
    hasBackup?: boolean;
}

/** A picture that can become the book's cover. */
export interface CoverChoice {
    /** Where it's from: "Current cover", "PDF page 1", "EPUB cover"… */
    label: string;
    /** A same-origin URL showing it (often an object URL). */
    src: string;
    /** In pixels: an image's own size, a page's as it would be saved. */
    width: number;
    height: number;
    /** The image file's size, or a page's as the JPEG that would be saved. */
    bytes: number;
    /** Makes it the book's cover; absent for the current cover. */
    save?: () => Promise<CoverStatus>;
}

/** Pictures are saved at most this wide (the server re-encodes them). */
const SAVE_MAX_WIDTH = 1200;
/** Pages of a book offered as covers. */
const PAGES = 5;
/** A text-only EPUB page is drawn at this size. */
const TEXT_PAGE = { width: 800, height: 1130 };

/** One block of readable text from an EPUB section. */
interface TextBlock {
    text: string;
    heading: boolean;
}

/** A canvas's 2D context; these canvases are always 2D-capable. */
function ctx2d(canvas: HTMLCanvasElement): CanvasRenderingContext2D {
    const ctx = canvas.getContext("2d");
    if (!ctx) throw new Error("2D canvas not supported");
    return ctx;
}

/** "1.2 MB", "245 KB". */
export function formatBytes(n: number): string {
    return n >= 1 << 20 ? `${(n / (1 << 20)).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`;
}

// ---- Saving ------------------------------------------------------------------

async function postCover(uuid: string, body: string | FormData): Promise<CoverStatus> {
    const resp = await fetch(`/upload-cover/${encodeURIComponent(uuid)}`, { method: "POST", body });
    if (!resp.ok) throw new Error(await resp.text());
    return resp.json();
}

/** Brings back the cover before the last change. */
export async function revertCover(uuid: string): Promise<CoverStatus> {
    const resp = await fetch(`/revert-cover/${encodeURIComponent(uuid)}`, { method: "POST" });
    if (!resp.ok) throw new Error(await resp.text());
    return resp.json();
}

/** Whether the book has a cover, and one to revert to. */
export async function fetchCoverStatus(uuid: string): Promise<CoverStatus> {
    try {
        const resp = await fetch(`/cover-status/${encodeURIComponent(uuid)}`);
        if (resp.ok) return resp.json();
    } catch {
        /* ignore */
    }
    return { hasCover: false, hasBackup: false };
}

function jpegBlob(canvas: HTMLCanvasElement): Promise<Blob> {
    return new Promise((resolve, reject) =>
        canvas.toBlob((b) => (b ? resolve(b) : reject(new Error("couldn't encode the page"))), "image/jpeg", 0.92));
}

async function decode(src: string): Promise<HTMLImageElement> {
    const img = new Image();
    img.src = src;
    await img.decode();
    return img;
}

// ---- Choices -------------------------------------------------------------------

/** A rendered page: shown and saved as the same JPEG. */
async function canvasChoice(uuid: string, label: string, canvas: HTMLCanvasElement): Promise<CoverChoice> {
    const blob = await jpegBlob(canvas);
    return {
        label,
        src: URL.createObjectURL(blob),
        width: canvas.width,
        height: canvas.height,
        bytes: blob.size,
        save: () => postCover(uuid, canvas.toDataURL("image/jpeg", 0.92)),
    };
}

/** An image at a same-origin (or blob:) URL; null if it can't be loaded. */
export async function imageChoice(uuid: string, url: string, label: string): Promise<CoverChoice | null> {
    try {
        const resp = await fetch(url, { credentials: "same-origin" });
        if (!resp.ok) return null;
        const blob = await resp.blob();
        const src = URL.createObjectURL(blob);
        const img = await decode(src);
        return {
            label,
            src,
            width: img.naturalWidth,
            height: img.naturalHeight,
            bytes: blob.size,
            save: () => {
                const s = Math.min(1, SAVE_MAX_WIDTH / img.naturalWidth);
                const canvas = document.createElement("canvas");
                canvas.width = Math.round(img.naturalWidth * s);
                canvas.height = Math.round(img.naturalHeight * s);
                ctx2d(canvas).drawImage(img, 0, 0, canvas.width, canvas.height);
                return postCover(uuid, canvas.toDataURL("image/jpeg", 0.92));
            },
        };
    } catch {
        return null;
    }
}

/** The book's cover as it is now; null if it has none. */
export async function currentCover(uuid: string): Promise<CoverChoice | null> {
    const c = await imageChoice(uuid, `/cover/${encodeURIComponent(uuid)}?t=${Date.now()}`, "Current cover");
    if (c) delete c.save;
    return c;
}

/** An image file picked by the admin; saved as uploaded. */
export async function fileChoice(uuid: string, file: File): Promise<CoverChoice> {
    const src = URL.createObjectURL(file);
    const img = await decode(src);
    const fd = new FormData();
    fd.append("cover", file);
    return {
        label: file.name,
        src,
        width: img.naturalWidth,
        height: img.naturalHeight,
        bytes: file.size,
        save: () => postCover(uuid, fd),
    };
}

/** The first pages of the book's PDF, rendered as they would be saved. */
export async function* pdfPages(uuid: string): AsyncGenerator<CoverChoice> {
    const pdf = await openPdf(`/download/${encodeURIComponent(uuid)}/pdf`);
    try {
        for (let n = 1; n <= Math.min(pdf.numPages, PAGES); n++) {
            const page = await pdf.getPage(n);
            const scale = Math.min(SAVE_MAX_WIDTH / page.getViewport({ scale: 1 }).width, 3);
            const viewport = page.getViewport({ scale });
            const canvas = document.createElement("canvas");
            canvas.width = Math.round(viewport.width);
            canvas.height = Math.round(viewport.height);
            await page.render({ canvasContext: ctx2d(canvas), viewport }).promise;
            yield await canvasChoice(uuid, `PDF page ${n}`, canvas);
        }
    } finally {
        void pdf.destroy();
    }
}

/** Loads a classic <script> (the EPUB libraries). */
function loadScript(src: string): Promise<void> {
    return new Promise((resolve, reject) => {
        const s = document.createElement("script");
        s.src = src;
        s.onload = () => resolve();
        s.onerror = () => reject(new Error(`failed to load ${src}`));
        document.head.appendChild(s);
    });
}

/**
 * The EPUB's declared cover, then its first sections: each section's first
 * image, or its text drawn as a page. The same picture is offered once.
 */
export async function* epubPages(uuid: string): AsyncGenerator<CoverChoice> {
    // JSZip first: epub.js opens the whole .epub from memory with it.
    if (!("JSZip" in window)) await loadScript(JSZIP_URL);
    if (typeof window.ePub === "undefined") await loadScript(EPUBJS_URL);
    const res = await fetch(`/download/${encodeURIComponent(uuid)}/epub`, { credentials: "same-origin" });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const book = ePub(await res.arrayBuffer());
    await book.ready;

    const seen = new Set<string>();
    const fresh = (c: CoverChoice | null): c is CoverChoice => {
        if (!c) return false;
        const key = `${c.width}x${c.height}:${c.bytes}`;
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
    };

    try {
        const coverUrl = await book.coverUrl();
        const cover = coverUrl ? await imageChoice(uuid, coverUrl, "EPUB cover") : null;
        if (fresh(cover)) yield cover;
    } catch {
        /* no declared cover */
    }

    for (let i = 0; i < Math.min(book.spine.length || 0, PAGES); i++) {
        const section = book.spine.get(i);
        if (!section) continue;
        try {
            const doc = await section.load(book.load.bind(book));
            if (!doc) continue;
            const label = `EPUB section ${i + 1}`;
            const imgEl = doc.querySelector("img, image");
            const rawSrc = imgEl?.getAttribute("src") || imgEl?.getAttribute("href") || imgEl?.getAttribute("xlink:href");
            if (rawSrc) {
                // Resolve against the section's path inside the book; external
                // and data: URLs are left alone.
                const resolved = new URL(rawSrc, `epub:${section.url}`);
                const url = resolved.protocol === "epub:" ? await book.archive.createUrl(resolved.pathname, { base64: false }) : rawSrc;
                const c = await imageChoice(uuid, url, label);
                if (fresh(c)) yield c;
                continue;
            }
            const canvas = document.createElement("canvas");
            canvas.width = TEXT_PAGE.width;
            canvas.height = TEXT_PAGE.height;
            drawTextPage(ctx2d(canvas), canvas.width, canvas.height, sectionBlocks(doc), label);
            const c = await canvasChoice(uuid, label, canvas);
            if (fresh(c)) yield c;
        } catch (err: unknown) {
            console.warn("Could not load EPUB section", i, err);
        }
    }
}

// ---- Text pages ------------------------------------------------------------------

/**
 * Readable text blocks from an EPUB section. epub.js's section.load() gives
 * the section's root <html> element (not a Document), and EPUB XHTML is XML,
 * so there's no .body / .innerText: find <body> and read block elements.
 */
function sectionBlocks(root: Element | null): TextBlock[] {
    const body = root?.querySelector?.("body") || root;
    if (!body) return [];
    const blocks = [...body.querySelectorAll("h1,h2,h3,h4,h5,h6,p,li,blockquote,pre,dt,dd,figcaption,td")]
        .map((el) => ({
            text: (el.textContent || "").replace(/\s+/g, " ").trim(),
            heading: /^h[1-6]$/i.test(el.localName || el.tagName),
        }))
        .filter((b) => b.text);
    if (blocks.length) return blocks;
    const text = (body.textContent || "").replace(/\s+/g, " ").trim();
    return text ? [{ text, heading: false }] : [];
}

/**
 * Draw blocks onto a page-shaped canvas, word-wrapped to its width. The
 * first heading is bold and larger. Sizes are relative to the canvas width.
 */
function drawTextPage(ctx: CanvasRenderingContext2D, width: number, height: number, blocks: TextBlock[], fallbackTitle: string): void {
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
        if (block.heading) firstHeadingDone = true;
        const size = isHead ? headSize : bodySize;
        ctx.font = `${isHead ? "bold " : ""}${size}px Georgia, serif`;
        const lineHeight = Math.round(size * 1.35);
        let line = "";
        for (const word of block.text.split(" ")) {
            const test = line ? `${line} ${word}` : word;
            if (ctx.measureText(test).width > maxWidth && line) {
                if (y + lineHeight > height - margin) return;
                ctx.fillText(line, margin, y);
                y += lineHeight;
                line = word;
            } else {
                line = test;
            }
        }
        if (line) {
            if (y + lineHeight > height - margin) return;
            ctx.fillText(line, margin, y);
            y += lineHeight;
        }
        y += Math.round(lineHeight * (isHead ? 0.6 : 0.4)); // paragraph gap
    }
}
