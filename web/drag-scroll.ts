/**
 * Drag to scroll, for the mouse: pressing on a sideways row of books and
 * dragging moves the row with the pointer, instead of dragging the cover
 * image or link out of the page. Letting go leaves the row exactly where
 * it is: no glide and no snapping, so the cover under the pointer stays
 * under it.
 *
 * Touch and pens are left to the browser, which already does this. A press
 * that moves less than DRAG_SLOP is a click as before; one that dragged
 * doesn't open the book it started on.
 */

const DRAG_SLOP = 6; // px before a press becomes a drag

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

        const move = (ev: PointerEvent): void => {
            if (ev.pointerId !== e.pointerId) return;
            const dx = ev.clientX - startX;
            if (!dragging) {
                if (Math.abs(dx) < DRAG_SLOP) return;
                dragging = true;
                row.classList.add("drag-scrolling");
                row.setPointerCapture(e.pointerId);
                window.getSelection()?.removeAllRanges();
            }
            row.scrollLeft = startLeft - dx;
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

            row.classList.remove("drag-scrolling");
        };

        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", end);
        window.addEventListener("pointercancel", end);
    });
}
