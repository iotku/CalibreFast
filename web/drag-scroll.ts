/**
 * Drag to scroll, for the mouse: pressing on a sideways row of books and
 * dragging moves the row, as a finger does on a touch screen, instead of
 * dragging the cover image or link out of the page. Letting go while
 * moving carries on in that direction and slows to a stop wherever it
 * gets to (no snapping to a card).
 *
 * Touch and pens are left to the browser, which already does this. A press
 * that moves less than DRAG_SLOP is a click as before; one that dragged
 * doesn't open the book it started on.
 */

const DRAG_SLOP = 6; // px before a press becomes a drag
const FLING_MS = 300; // how far a release carries on: this long at its speed
const SAMPLE_MS = 100; // the speed at release: over the last this long

/** Each row's latest drag: a glide that ends after a new drag began leaves it be. */
const drags = new WeakMap<HTMLElement, number>();

/** Make every element matching selector under root drag-scrollable. */
export function setupDragScroll(root: HTMLElement, selector: string): void {
    // The browser's own drag of a cover or link would take the pointer away.
    root.addEventListener("dragstart", (e) => {
        if (e.target instanceof Element && e.target.closest(selector)) e.preventDefault();
    });

    root.addEventListener("pointerdown", (e) => {
        if (e.pointerType !== "mouse" || e.button !== 0) return;
        const row = e.target instanceof Element ? e.target.closest<HTMLElement>(selector) : null;
        if (!row || row.scrollWidth <= row.clientWidth) return;
        // A button on a card (favorite, want to read) stays a button.
        if (e.target instanceof Element && e.target.closest("button, input, select, textarea, summary")) return;

        const startX = e.clientX;
        const startLeft = row.scrollLeft;
        let dragging = false;
        const samples: { t: number; x: number }[] = [];

        const move = (ev: PointerEvent): void => {
            if (ev.pointerId !== e.pointerId) return;
            const dx = ev.clientX - startX;
            if (!dragging) {
                if (Math.abs(dx) < DRAG_SLOP) return;
                dragging = true;
                drags.set(row, (drags.get(row) ?? 0) + 1);
                row.classList.add("drag-scrolling");
                row.setPointerCapture(e.pointerId);
                window.getSelection()?.removeAllRanges();
            }
            row.scrollLeft = startLeft - dx;
            const now = ev.timeStamp;
            samples.push({ t: now, x: ev.clientX });
            while (samples.length > 2 && now - samples[0]!.t > SAMPLE_MS) samples.shift();
        };

        const end = (ev: PointerEvent): void => {
            if (ev.pointerId !== e.pointerId) return;
            window.removeEventListener("pointermove", move);
            window.removeEventListener("pointerup", end);
            window.removeEventListener("pointercancel", end);
            if (!dragging) return;
            // The click this release makes isn't a click on the card.
            const swallow = (c: MouseEvent): void => {
                c.preventDefault();
                c.stopPropagation();
            };
            window.addEventListener("click", swallow, { capture: true, once: true });
            setTimeout(() => window.removeEventListener("click", swallow, { capture: true }), 0);

            // Speed at release (px/ms, as the row scrolls), then rest on a card.
            let v = 0;
            const first = samples[0];
            const last = samples[samples.length - 1];
            if (first && last && last.t > first.t && ev.timeStamp - last.t < SAMPLE_MS) {
                v = -(last.x - first.x) / (last.t - first.t);
            }
            fling(row, row.scrollLeft + v * FLING_MS);
        };

        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", end);
        window.addEventListener("pointercancel", end);
    });
}

/** Glide row on to target, smoothly. */
function fling(row: HTMLElement, target: number): void {
    const max = row.scrollWidth - row.clientWidth;
    const left = Math.max(0, Math.min(max, target));
    const drag = drags.get(row);
    const done = (): void => {
        if (drags.get(row) === drag) row.classList.remove("drag-scrolling");
    };
    if (Math.abs(left - row.scrollLeft) < 1) {
        done();
        return;
    }
    row.scrollTo({ left, behavior: "smooth" });
    // The drag ends once it's there (scrollend; a timer where that isn't
    // supported).
    let timer = 0;
    const settle = (): void => {
        clearTimeout(timer);
        row.removeEventListener("scrollend", settle);
        done();
    };
    row.addEventListener("scrollend", settle);
    timer = window.setTimeout(settle, 700);
}
