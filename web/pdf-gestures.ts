/**
 * Zooming and moving a PDF page by hand (web/pdf-view.ts does the drawing):
 *   - two fingers: pinch to zoom, anchored between the fingers, which can
 *     also drag the page along;
 *   - one finger: the browser pans a zoomed page itself (touch-action in
 *     web/reader.css); when the page isn't wider than the window, a
 *     sideways swipe turns it;
 *   - Ctrl/⌘ + wheel, or a trackpad pinch: zoom at the pointer;
 *   - mouse: drag a zoomed page by anything that isn't text or a link.
 */

import type { Anchor, PdfView } from "./pdf-view.js";

export interface PdfGestures {
    /** Whether this click is the tail of a drag, pinch or swipe (so it isn't a tap). */
    wasGesture(e: Event): boolean;
}

/** Safari's trackpad pinch (not in TypeScript's DOM types). */
interface GestureEvent extends UIEvent {
    scale: number;
    clientX: number;
    clientY: number;
}

export function setupPdfGestures(container: HTMLElement, getView: () => PdfView | null, onSwipe: (side: 1 | -1) => void): PdfGestures {
    let gestureAt = -Infinity;

    // ---- touch --------------------------------------------------------------

    let pinch: { distance: number; scale: number; anchor: Anchor } | null = null;
    let swipe: { x: number; y: number; t: number } | null = null;
    let touches = 0;
    const between = (a: Touch, b: Touch): { x: number; y: number; distance: number } => ({
        x: (a.clientX + b.clientX) / 2,
        y: (a.clientY + b.clientY) / 2,
        distance: Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY),
    });

    container.addEventListener(
        "touchstart",
        (e) => {
            const view = getView();
            touches = e.touches.length;
            const [a, b] = e.touches;
            if (view && a && b && e.touches.length === 2) {
                e.preventDefault(); // ours, not the browser's page zoom or scroll
                const m = between(a, b);
                pinch = { distance: m.distance || 1, scale: view.scale, anchor: view.anchorAt(m.x, m.y) };
                swipe = null;
            } else if (view && a && e.touches.length === 1 && !view.overflows().x) {
                swipe = { x: a.clientX, y: a.clientY, t: e.timeStamp };
            } else {
                swipe = null;
            }
        },
        { passive: false },
    );
    container.addEventListener(
        "touchmove",
        (e) => {
            const [a, b] = e.touches;
            if (!pinch || !a || !b) return;
            e.preventDefault();
            const m = between(a, b);
            // The point that was between the fingers stays between them.
            getView()?.setScale((pinch.scale * m.distance) / pinch.distance, { ...pinch.anchor, x: m.x, y: m.y });
        },
        { passive: false },
    );
    const touchEnd = (e: TouchEvent): void => {
        touches = e.touches.length;
        if (pinch) {
            if (e.touches.length < 2) pinch = null;
            gestureAt = e.timeStamp;
            swipe = null;
            return;
        }
        const p = e.changedTouches[0];
        if (!swipe || !p || e.type === "touchcancel") {
            swipe = null;
            return;
        }
        const dx = p.clientX - swipe.x;
        const dy = p.clientY - swipe.y;
        if (e.timeStamp - swipe.t < 700 && Math.abs(dx) > 40 && Math.abs(dx) > 1.5 * Math.abs(dy) && !window.getSelection()?.toString()) {
            gestureAt = e.timeStamp;
            onSwipe(dx < 0 ? 1 : -1); // the page follows the finger: swiping left brings the right-hand one
        }
        swipe = null;
    };
    container.addEventListener("touchend", touchEnd);
    container.addEventListener("touchcancel", touchEnd);

    // ---- wheel and trackpad -------------------------------------------------

    container.addEventListener(
        "wheel",
        (e) => {
            const view = getView();
            if (!view || !(e.ctrlKey || e.metaKey)) return;
            e.preventDefault(); // not the browser's own zoom
            const delta = Math.max(-60, Math.min(60, e.deltaMode === WheelEvent.DOM_DELTA_LINE ? e.deltaY * 16 : e.deltaY));
            view.setScale(view.scale * Math.exp(-delta * 0.01), view.anchorAt(e.clientX, e.clientY));
        },
        { passive: false },
    );

    let gestureScale = 1;
    container.addEventListener("gesturestart", (e) => {
        if (touches) return; // fingers on the screen are handled above
        e.preventDefault();
        gestureScale = getView()?.scale ?? 1;
    });
    container.addEventListener("gesturechange", (e) => {
        const view = getView();
        if (touches || !view) return;
        e.preventDefault();
        const g = e as GestureEvent;
        view.setScale(gestureScale * g.scale, view.anchorAt(g.clientX, g.clientY));
    });

    // ---- mouse drag ----------------------------------------------------------

    let drag: { x: number; y: number; left: number; top: number; moved: boolean } | null = null;
    container.addEventListener("pointerdown", (e) => {
        const view = getView();
        const over = view?.overflows();
        if (e.pointerType !== "mouse" || e.button !== 0 || !over || !(over.x || over.y)) return;
        // Text is for selecting and links for clicking.
        if (e.target instanceof Element && e.target.closest(".textLayer span, .textLayer br, a")) return;
        e.preventDefault();
        drag = { x: e.clientX, y: e.clientY, left: container.scrollLeft, top: container.scrollTop, moved: false };
        container.setPointerCapture(e.pointerId);
    });
    container.addEventListener("pointermove", (e) => {
        if (!drag) return;
        const dx = e.clientX - drag.x;
        const dy = e.clientY - drag.y;
        if (!drag.moved && Math.hypot(dx, dy) < 4) return;
        drag.moved = true;
        container.classList.add("dragging");
        container.scrollLeft = drag.left - dx;
        container.scrollTop = drag.top - dy;
    });
    const dragEnd = (e: PointerEvent): void => {
        if (!drag) return;
        if (drag.moved) gestureAt = e.timeStamp;
        drag = null;
        container.classList.remove("dragging");
    };
    container.addEventListener("pointerup", dragEnd);
    container.addEventListener("pointercancel", dragEnd);

    return { wasGesture: (e) => e.timeStamp - gestureAt < 400 };
}
