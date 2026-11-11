/**
 * Shows a PDF one page (or two, side by side) at a time, sharp at any zoom.
 *
 * Each page is a box sized in CSS pixels (`scale` = CSS pixels per PDF
 * point), holding:
 *   - a base canvas: the whole page at scale × devicePixelRatio, so one
 *     canvas pixel is one screen pixel. Past MAX_BASE_AREA the base stays at
 *     the largest size that fits and is stretched;
 *   - a detail canvas, only then: the part of the page on screen at full
 *     resolution, drawn again when scrolling or zooming settles. Its size is
 *     bounded by the window, not the zoom;
 *   - pdf.js's text layer (transparent, selectable text) and the links.
 *     Both are placed in percentages, so zooming never rebuilds them.
 *
 * While a zoom gesture is under way only CSS sizes change (the canvases
 * stretch); the canvases are drawn again when it settles.
 */

/** Canvas pixels per page before the detail canvas takes over (iOS allows 16.7M per canvas). */
const MAX_BASE_AREA = 8_000_000;
const MAX_DETAIL_AREA = 14_000_000;
const SETTLE_MS = 140;
/** 100% is the page's printed size at CSS's 96 pixels an inch. */
export const ACTUAL_SIZE = 96 / 72;
const MAX_SCALE = ACTUAL_SIZE * 10;

export type ZoomMode = "page" | "width" | "custom";

export interface ViewLayout {
    dual: boolean;
    /** Right-to-left: in a spread the first page is on the right. */
    rtl: boolean;
    /** Dual: page 1 (the cover) stands alone, so later spreads are 2–3, 4–5, … */
    coverAlone: boolean;
    zoom: ZoomMode;
    /** The scale for zoom "custom". */
    scale: number;
}

/** A point of the spread, as fractions of it, and where on screen it should be. */
export interface Anchor {
    fx: number;
    fy: number;
    x: number;
    y: number;
}

export interface PagePoint {
    page: number;
    /** In PDF points from the page's top left. */
    x: number;
    y: number;
}

export interface PdfView {
    readonly numPages: number;
    /** Pages on screen (last === first for a single page). */
    readonly first: number;
    readonly last: number;
    readonly scale: number;
    /** Whether the spread is wider or taller than the window. */
    overflows(): { x: boolean; y: boolean };
    show(page: number): Promise<void>;
    /** The first page of the spread before (-1) or after (1) the one shown, or null at either end. */
    neighbour(dir: 1 | -1): number | null;
    setLayout(layout: ViewLayout): Promise<void>;
    fitScale(mode: "page" | "width"): number;
    anchorAt(x: number, y: number): Anchor;
    /** Zooms, keeping the anchor's point where it says. Canvases are redrawn once changes stop. */
    setScale(scale: number, anchor?: Anchor): void;
    /** The page and point under a screen position. */
    pointAt(x: number, y: number): PagePoint | null;
    /** Draws a region of a page (PDF points, from its top left) to fill a canvas. */
    renderRegion(page: number, region: { x: number; y: number; w: number; h: number }, canvas: HTMLCanvasElement): PdfJS.RenderTask | null;
    /** The element of a page on screen (annotations are drawn into it). */
    pageElement(page: number): HTMLElement | null;
    pageSize(page: number): { w: number; h: number } | null;
}

export interface PdfViewOptions {
    container: HTMLElement;
    pdf: PdfJS.Document;
    lib: PdfJS.Lib;
    layout: ViewLayout;
    /** A spread is on screen (its text and links may still be loading). */
    onShown(first: number, last: number): void;
    /** The reader zoomed by hand: the scale to remember. */
    onZoom(scale: number): void;
    /** A link to a place in the document was clicked. */
    onDest(dest: PdfJS.Dest): void;
}

interface PageView {
    num: number;
    page: PdfJS.Page;
    /** Size in PDF points (after the page's own rotation). */
    w: number;
    h: number;
    el: HTMLDivElement;
    base: HTMLCanvasElement;
    /** Canvas pixels per point the base was drawn at (0: not yet). */
    baseScale: number;
    baseTask: PdfJS.RenderTask | null;
    detail: HTMLCanvasElement | null;
    /** What the detail canvas covers, in CSS pixels of the page at detailScale. */
    detailRect: { x0: number; y0: number; x1: number; y1: number } | null;
    detailScale: number;
    detailTask: PdfJS.RenderTask | null;
    textLayer: PdfJS.TextLayer | null;
}

/** Rendering that was cancelled isn't an error. */
const ignoreCancel = (e: unknown): void => {
    if ((e as { name?: string } | null)?.name !== "RenderingCancelledException") console.error(e);
};

export function createPdfView(opts: PdfViewOptions): PdfView {
    const { container, pdf, lib } = opts;
    let layout = opts.layout;
    let scale = layout.scale;
    let views: PageView[] = [];
    let first = 1;
    let showing = 0; // counts show() calls: a newer one abandons the older

    const spread = document.createElement("div");
    spread.className = "pdf-spread";
    container.replaceChildren(spread);

    const dpr = (): number => window.devicePixelRatio || 1;
    const dualNow = (): boolean => layout.dual;

    function spreadPages(page: number): number[] {
        if (!dualNow()) return [page];
        const offset = layout.coverAlone ? 0 : 1; // pairs start on even pages, or odd
        if (layout.coverAlone && page === 1) return [1];
        const start = page - ((page + offset) % 2);
        return start + 1 <= pdf.numPages ? [start, start + 1] : [start];
    }

    function fitScale(mode: "page" | "width", of: PageView[] = views): number {
        if (!of.length) return scale;
        const gap = 4 * (of.length - 1);
        const w = of.reduce((sum, v) => sum + v.w, 0);
        const h = Math.max(...of.map((v) => v.h));
        // The whole box: a scrollbar from the zoom being left mustn't shrink the fit.
        const byWidth = (container.offsetWidth - gap) / w;
        return mode === "width" ? byWidth : Math.min(byWidth, container.offsetHeight / h);
    }
    /**
     * Zooming out stops once the whole spread is on screen: any further would
     * only shrink it, showing nothing more.
     */
    const clampScale = (s: number, of: PageView[] = views): number => Math.max(Math.min(MAX_SCALE, s), fitScale("page", of));
    /** The scale the layout asks for: a fit, or the one set by hand (within its limits). */
    function layoutScale(of: PageView[] = views): number {
        // Zoomed all the way out is fitting the page, and stays so as the window or the pages change.
        if (layout.zoom === "custom" && of.length && scale <= fitScale("page", of)) layout = { ...layout, zoom: "page" };
        return layout.zoom === "custom" ? clampScale(scale, of) : fitScale(layout.zoom, of);
    }

    /** Page boxes at the current scale, on whole device pixels so canvases map 1:1. */
    function applyScale(of: PageView[] = views): void {
        const r = dpr();
        for (const v of of) {
            v.el.style.width = `${Math.round(v.w * scale * r) / r}px`;
            v.el.style.height = `${Math.round(v.h * scale * r) / r}px`;
            v.el.style.setProperty("--total-scale-factor", String(scale));
            v.el.style.setProperty("--scale-round-x", `${1 / r}px`);
            v.el.style.setProperty("--scale-round-y", `${1 / r}px`);
        }
    }

    // ---- drawing ------------------------------------------------------------

    /** Draws the whole page at the current scale (or as large as allowed). */
    async function renderBase(v: PageView): Promise<void> {
        const want = scale * dpr();
        const s = Math.min(want, Math.sqrt(MAX_BASE_AREA / (v.w * v.h)));
        if (Math.abs(v.baseScale - s) < s * 0.001) return;
        v.baseTask?.cancel();
        // Draw off screen and swap, so the old picture stays until the new one is ready.
        const canvas = document.createElement("canvas");
        canvas.className = "pdf-base";
        canvas.width = Math.round(v.w * s);
        canvas.height = Math.round(v.h * s);
        const ctx = canvas.getContext("2d", { alpha: false });
        if (!ctx) return;
        const viewport = v.page.getViewport({ scale: s });
        const task = v.page.render({
            canvasContext: ctx,
            viewport,
            // The canvas is a whole number of pixels; the viewport rarely is.
            transform: [canvas.width / viewport.width, 0, 0, canvas.height / viewport.height, 0, 0],
        });
        v.baseTask = task;
        try {
            await task.promise;
        } catch (e) {
            ignoreCancel(e);
            return;
        } finally {
            if (v.baseTask === task) v.baseTask = null;
        }
        v.base.replaceWith(canvas);
        v.base = canvas;
        v.baseScale = s;
    }

    function dropDetail(v: PageView): void {
        v.detailTask?.cancel();
        v.detailTask = null;
        v.detail?.remove();
        v.detail = null;
        v.detailRect = null;
    }

    /** The part of a page inside the window, in CSS pixels of the page; null when none is. */
    function visiblePart(v: PageView): { x0: number; y0: number; x1: number; y1: number; rect: DOMRect } | null {
        const rect = v.el.getBoundingClientRect();
        const c = container.getBoundingClientRect();
        const x0 = Math.max(0, c.left - rect.left);
        const y0 = Math.max(0, c.top - rect.top);
        const x1 = Math.min(rect.width, c.right - rect.left);
        const y1 = Math.min(rect.height, c.bottom - rect.top);
        return x1 > x0 && y1 > y0 ? { x0, y0, x1, y1, rect } : null;
    }

    /** Draws what's on screen at full resolution, when the base canvas couldn't be that large. */
    async function renderDetail(v: PageView): Promise<void> {
        const r = dpr();
        if (v.baseScale >= scale * r * 0.999) {
            dropDetail(v);
            return;
        }
        const vis = visiblePart(v);
        if (!vis) return;
        const d = v.detailRect;
        if (d && v.detailScale === scale && vis.x0 >= d.x0 && vis.y0 >= d.y0 && vis.x1 <= d.x1 && vis.y1 <= d.y1) return;

        // Some of the surroundings too, so small scrolls don't need a redraw.
        let { x0, y0, x1, y1 } = vis;
        const fits = (mx: number, my: number): boolean => (x1 - x0 + 2 * mx) * (y1 - y0 + 2 * my) * r * r <= MAX_DETAIL_AREA;
        const mx = container.clientWidth / 4;
        const my = container.clientHeight / 4;
        if (fits(mx, my)) {
            x0 = Math.max(0, x0 - mx);
            y0 = Math.max(0, y0 - my);
            x1 = Math.min(vis.rect.width, x1 + mx);
            y1 = Math.min(vis.rect.height, y1 + my);
        }
        // Whole device pixels.
        const px0 = Math.floor(x0 * r);
        const py0 = Math.floor(y0 * r);
        const canvas = document.createElement("canvas");
        canvas.className = "pdf-detail";
        canvas.width = Math.ceil(x1 * r) - px0;
        canvas.height = Math.ceil(y1 * r) - py0;
        const ctx = canvas.getContext("2d", { alpha: false });
        if (!ctx) return;
        v.detailTask?.cancel();
        const sx = (vis.rect.width * r) / v.w; // canvas pixels per point, as laid out
        const sy = (vis.rect.height * r) / v.h;
        const viewport = v.page.getViewport({ scale: sx });
        const task = v.page.render({
            canvasContext: ctx,
            viewport,
            transform: [1, 0, 0, sy / sx, -px0, -py0],
        });
        v.detailTask = task;
        const at = scale;
        try {
            await task.promise;
        } catch (e) {
            ignoreCancel(e);
            return;
        } finally {
            if (v.detailTask === task) v.detailTask = null;
        }
        if (at !== scale) return; // zoomed meanwhile: the next settle draws again
        // In percentages of the page, so it stays in place while a zoom gesture stretches it.
        const pct = (n: number, of: number): string => `${(n / r / of) * 100}%`;
        canvas.style.left = pct(px0, vis.rect.width);
        canvas.style.top = pct(py0, vis.rect.height);
        canvas.style.width = pct(canvas.width, vis.rect.width);
        canvas.style.height = pct(canvas.height, vis.rect.height);
        v.detail?.remove();
        v.base.after(canvas);
        v.detail = canvas;
        v.detailRect = { x0: px0 / r, y0: py0 / r, x1: (px0 + canvas.width) / r, y1: (py0 + canvas.height) / r };
        v.detailScale = scale;
    }

    let settleTimer: ReturnType<typeof setTimeout> | undefined;
    /** Redraws at the current scale once zooming or scrolling has stopped. */
    function settleSoon(): void {
        clearTimeout(settleTimer);
        settleTimer = setTimeout(() => {
            for (const v of views) void renderBase(v).then(() => renderDetail(v));
        }, SETTLE_MS);
    }
    container.addEventListener("scroll", settleSoon, { passive: true });

    // ---- text and links -----------------------------------------------------

    function addText(v: PageView): void {
        const div = document.createElement("div");
        div.className = "textLayer";
        v.el.append(div);
        const layer = new lib.TextLayer({ textContentSource: v.page.streamTextContent(), container: div, viewport: v.page.getViewport({ scale: 1 }) });
        v.textLayer = layer;
        layer
            .render()
            .then(() => {
                // While selecting, this covers the page so the selection doesn't
                // jump when the pointer is between lines (as pdf.js's viewer does).
                const end = document.createElement("div");
                end.className = "endOfContent";
                div.append(end);
            })
            .catch(() => {
                /* cancelled with the page */
            });
        div.addEventListener("pointerdown", () => div.classList.add("selecting"));
    }
    document.addEventListener("pointerup", () => {
        for (const el of spread.querySelectorAll(".textLayer.selecting")) el.classList.remove("selecting");
    });

    async function addLinks(v: PageView): Promise<void> {
        let annotations: PdfJS.Annotation[];
        try {
            annotations = await v.page.getAnnotations();
        } catch {
            return;
        }
        const links = annotations.filter((a) => a.subtype === "Link" && (a.url || a.dest));
        if (!links.length || !views.includes(v)) return;
        const layer = document.createElement("div");
        layer.className = "pdf-links";
        const viewport = v.page.getViewport({ scale: 1 });
        for (const link of links) {
            const [ax, ay, bx, by] = viewport.convertToViewportRectangle(link.rect) as [number, number, number, number];
            const a = document.createElement("a");
            a.style.left = `${(Math.min(ax, bx) / v.w) * 100}%`;
            a.style.top = `${(Math.min(ay, by) / v.h) * 100}%`;
            a.style.width = `${(Math.abs(bx - ax) / v.w) * 100}%`;
            a.style.height = `${(Math.abs(by - ay) / v.h) * 100}%`;
            if (link.url) {
                // pdf.js only passes on http(s), mailto, ftp and tel URLs.
                a.href = link.url;
                a.target = "_blank";
                a.rel = "noopener noreferrer";
                a.title = link.url;
            } else {
                a.href = "#";
                a.title = "Go to this place in the document";
                a.addEventListener("click", (e) => {
                    e.preventDefault();
                    opts.onDest(link.dest ?? null);
                });
            }
            layer.append(a);
        }
        v.el.append(layer);
    }

    // ---- showing a spread -----------------------------------------------------

    function discard(old: PageView[]): void {
        for (const v of old) {
            v.baseTask?.cancel();
            v.detailTask?.cancel();
            v.textLayer?.cancel();
        }
    }

    async function show(page: number): Promise<void> {
        const run = ++showing;
        const nums = spreadPages(Math.min(pdf.numPages, Math.max(1, page)));
        const pages = await Promise.all(nums.map((n) => pdf.getPage(n)));
        if (run !== showing) return;
        const next: PageView[] = pages.map((p, i) => {
            const size = p.getViewport({ scale: 1 });
            const el = document.createElement("div");
            el.className = "pdf-page";
            el.dataset.page = String(nums[i]);
            const base = document.createElement("canvas");
            base.className = "pdf-base";
            el.append(base);
            return {
                num: nums[i] ?? 1,
                page: p,
                w: size.width,
                h: size.height,
                el,
                base,
                baseScale: 0,
                baseTask: null,
                detail: null,
                detailRect: null,
                detailScale: 0,
                detailTask: null,
                textLayer: null,
            };
        });
        scale = layoutScale(next);
        applyScale(next);
        // The old spread stays until the new one has its picture.
        await Promise.all(next.map(renderBase));
        if (run !== showing) {
            discard(next);
            return;
        }
        // Zoomed (or the window resized) while it was drawn: the old spread took that, not this one.
        const drawnAt = scale;
        scale = layoutScale(next);
        applyScale(next);
        if (scale !== drawnAt) settleSoon();
        discard(views);
        views = next;
        first = nums[0] ?? 1;
        spread.classList.toggle("rtl", layout.rtl);
        spread.replaceChildren(...next.map((v) => v.el));
        container.scrollTop = 0;
        // Reading starts at the leading edge: the right, for right-to-left.
        container.scrollLeft = layout.rtl ? container.scrollWidth : 0;
        for (const v of next) {
            addText(v);
            void addLinks(v);
            void renderDetail(v);
        }
        opts.onShown(first, next[next.length - 1]?.num ?? first);
        // Have the next page parsed by the time it's asked for.
        const ahead = (next[next.length - 1]?.num ?? first) + 1;
        if (ahead <= pdf.numPages) void pdf.getPage(ahead).catch(() => {});
    }

    // ---- zoom -----------------------------------------------------------------

    function anchorAt(x: number, y: number): Anchor {
        const r = spread.getBoundingClientRect();
        return { fx: r.width ? (x - r.left) / r.width : 0.5, fy: r.height ? (y - r.top) / r.height : 0.5, x, y };
    }

    function setScale(next: number, anchor?: Anchor): void {
        next = clampScale(next);
        if (next === scale) return;
        const c = container.getBoundingClientRect();
        const a = anchor ?? anchorAt(c.left + c.width / 2, c.top + c.height / 2);
        scale = next;
        layout = { ...layout, zoom: "custom", scale };
        layoutScale(); // at the limit, it's a fit again
        applyScale();
        // Scroll so the anchored point is back under the fingers (or pointer).
        const r = spread.getBoundingClientRect();
        container.scrollLeft += r.left + a.fx * r.width - a.x;
        container.scrollTop += r.top + a.fy * r.height - a.y;
        settleSoon();
        opts.onZoom(scale);
    }

    // The window changed size: fit again.
    let lastSize = "";
    new ResizeObserver(() => {
        const size = `${container.clientWidth}x${container.clientHeight}`;
        const before = lastSize;
        lastSize = size;
        if (before === "" || size === before || !views.length) return;
        // A scale set by hand stays, unless the window grew past it.
        scale = layoutScale();
        applyScale();
        settleSoon();
    }).observe(container);

    function viewOf(page: number): PageView | undefined {
        return views.find((v) => v.num === page);
    }

    return {
        numPages: pdf.numPages,
        get first() {
            return first;
        },
        get last() {
            return views[views.length - 1]?.num ?? first;
        },
        get scale() {
            return scale;
        },
        overflows: () => ({
            x: container.scrollWidth > container.clientWidth + 1,
            y: container.scrollHeight > container.clientHeight + 1,
        }),
        show,
        neighbour(dir) {
            const to = dir > 0 ? (views[views.length - 1]?.num ?? first) + 1 : first - 1;
            if (to < 1 || to > pdf.numPages) return null;
            return spreadPages(to)[0] ?? to;
        },
        async setLayout(next) {
            const respread = next.dual !== layout.dual || next.rtl !== layout.rtl || next.coverAlone !== layout.coverAlone;
            layout = next;
            if (next.zoom === "custom") scale = next.scale;
            if (respread || !views.length) {
                await show(first);
                return;
            }
            scale = layoutScale();
            applyScale();
            settleSoon();
        },
        fitScale: (mode) => fitScale(mode),
        anchorAt,
        setScale,
        pointAt(x, y) {
            for (const v of views) {
                const r = v.el.getBoundingClientRect();
                if (x >= r.left && x < r.right && y >= r.top && y < r.bottom) {
                    return { page: v.num, x: ((x - r.left) / r.width) * v.w, y: ((y - r.top) / r.height) * v.h };
                }
            }
            return null;
        },
        renderRegion(page, region, canvas) {
            const v = viewOf(page);
            const ctx = canvas.getContext("2d", { alpha: false });
            if (!v || !ctx) return null;
            const s = canvas.width / region.w;
            return v.page.render({
                canvasContext: ctx,
                viewport: v.page.getViewport({ scale: s }),
                transform: [1, 0, 0, canvas.height / region.h / s, -region.x * s, -region.y * (canvas.height / region.h)],
            });
        },
        pageElement: (page) => viewOf(page)?.el ?? null,
        pageSize(page) {
            const v = viewOf(page);
            return v ? { w: v.w, h: v.h } : null;
        },
    };
}
