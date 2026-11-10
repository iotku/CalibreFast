/**
 * A magnifier for the PDF reader (mouse only: on touch, pinch instead). The
 * lens follows the pointer and shows what's under it, drawn from the PDF at
 * the lens's own resolution, so small print becomes readable rather than a
 * blown-up blur.
 *
 * Drawing a page region takes a moment, so the lens holds a tile three
 * times its size around the pointer and only slides it as the pointer
 * moves; a new tile is drawn when the pointer nears the edge of that one.
 */

import type { PdfView } from "./pdf-view.js";

export interface Loupe {
    isOn(): boolean;
    set(on: boolean): void;
    /** The page under the lens changed. */
    refresh(): void;
}

const LENS_W = 340;
const LENS_H = 230;
const MIN_POWER = 1.5;
const MAX_POWER = 8;
const MAX_TILE_AREA = 12_000_000;

interface Tile {
    page: number;
    /** The region it shows, in PDF points from the page's top left. */
    x: number;
    y: number;
    w: number;
    h: number;
    /** CSS pixels per point it was drawn for. */
    scale: number;
    canvas: HTMLCanvasElement;
}

export function setupLoupe(container: HTMLElement, getView: () => PdfView | null, onChange: (on: boolean) => void): Loupe {
    const lens = document.createElement("div");
    lens.className = "pdf-loupe";
    lens.hidden = true;
    lens.style.width = `${LENS_W}px`;
    lens.style.height = `${LENS_H}px`;
    document.body.append(lens);

    let on = false;
    /** How many times larger than the page on screen. */
    let power = 2.5;
    let pointer: { x: number; y: number } | null = null;
    let tile: Tile | null = null;
    let drawing: PdfJS.RenderTask | null = null;
    let frame = 0;

    /** Slides the tile so the point under the pointer is at the lens's centre. */
    function place(t: Tile, px: number, py: number, scale: number): void {
        const s = t.canvas.style;
        s.width = `${t.w * scale}px`;
        s.height = `${t.h * scale}px`;
        s.left = `${LENS_W / 2 - (px - t.x) * scale}px`;
        s.top = `${LENS_H / 2 - (py - t.y) * scale}px`;
    }

    function update(): void {
        frame = 0;
        const view = getView();
        const p = on && pointer && view ? view.pointAt(pointer.x, pointer.y) : null;
        const size = p && view ? view.pageSize(p.page) : null;
        if (!view || !pointer || !p || !size) {
            lens.hidden = true;
            return;
        }
        lens.hidden = false;
        lens.style.left = `${pointer.x - LENS_W / 2}px`;
        lens.style.top = `${pointer.y - LENS_H / 2}px`;

        const scale = view.scale * power;
        // What the lens shows, in points, kept to the page.
        const halfW = LENS_W / 2 / scale;
        const halfH = LENS_H / 2 / scale;
        const covers =
            tile !== null &&
            tile.page === p.page &&
            tile.scale === scale &&
            tile.x <= Math.max(0, p.x - halfW) &&
            tile.y <= Math.max(0, p.y - halfH) &&
            tile.x + tile.w >= Math.min(size.w, p.x + halfW) &&
            tile.y + tile.h >= Math.min(size.h, p.y + halfH);
        // The old tile, in the right place, until a better one is ready.
        if (tile && tile.page === p.page) place(tile, p.x, p.y, scale);
        else if (tile) tile.canvas.hidden = true;
        if (covers || drawing) return;

        const dpr = window.devicePixelRatio || 1;
        let x = Math.max(0, p.x - halfW * 3);
        let y = Math.max(0, p.y - halfH * 3);
        let w = Math.min(size.w, p.x + halfW * 3) - x;
        let h = Math.min(size.h, p.y + halfH * 3) - y;
        if (w * h * (scale * dpr) ** 2 > MAX_TILE_AREA) {
            // Very strong lens on a very large screen: just what's needed.
            x = Math.max(0, p.x - halfW);
            y = Math.max(0, p.y - halfH);
            w = Math.min(size.w, p.x + halfW) - x;
            h = Math.min(size.h, p.y + halfH) - y;
        }
        const canvas = document.createElement("canvas");
        canvas.width = Math.max(1, Math.round(w * scale * dpr));
        canvas.height = Math.max(1, Math.round(h * scale * dpr));
        const next: Tile = { page: p.page, x, y, w, h, scale, canvas };
        const task = view.renderRegion(p.page, { x, y, w, h }, canvas);
        if (!task) return;
        drawing = task;
        task.promise
            .then(() => {
                tile?.canvas.remove();
                tile = next;
                lens.append(canvas);
            })
            .catch(() => {
                /* cancelled, or the page went away */
            })
            .finally(() => {
                drawing = null;
                schedule(); // the pointer has probably moved on
            });
    }
    function schedule(): void {
        if (!frame) frame = requestAnimationFrame(update);
    }

    container.addEventListener("pointermove", (e) => {
        if (!on || e.pointerType !== "mouse") return;
        pointer = { x: e.clientX, y: e.clientY };
        schedule();
    });
    container.addEventListener("pointerleave", () => {
        pointer = null;
        schedule();
    });
    // The page under the lens changed (turned, zoomed, scrolled).
    container.addEventListener("scroll", schedule, { passive: true });

    // With the lens on, the wheel changes its strength.
    container.addEventListener(
        "wheel",
        (e) => {
            if (!on || e.ctrlKey || e.metaKey) return;
            e.preventDefault();
            e.stopImmediatePropagation();
            power = Math.min(MAX_POWER, Math.max(MIN_POWER, power * Math.exp(-Math.sign(e.deltaY) * 0.12)));
            schedule();
        },
        { passive: false, capture: true },
    );

    return {
        isOn: () => on,
        refresh: schedule,
        set(next) {
            if (next === on) return;
            on = next;
            container.classList.toggle("loupe-on", on);
            if (!on) {
                drawing?.cancel();
                tile?.canvas.remove();
                tile = null;
            }
            schedule();
            onChange(on);
        },
    };
}
