/**
 * Drag to scroll, for the mouse: pressing on a sideways row of books and
 * dragging moves the row with the pointer, instead of dragging the cover
 * image or link out of the page. Letting go leaves the row exactly where
 * it is: no glide and no snapping, so the cover under the pointer stays
 * under it.
 *
 * Past either end there's a runway: the cards keep following the pointer
 * one to one for a stretch, then with more and more resistance; letting go
 * there, they recoil back to the end.
 *
 * While dragging, the cards' hover buttons and details stay hidden, and
 * the card pressed doesn't keep focus (which would show them too).
 *
 * Touch and pens are left to the browser, which already does this. A press
 * that moves less than DRAG_SLOP is a click as before; one that dragged
 * doesn't open the book it started on.
 */

const DRAG_SLOP = 6; // px before a press becomes a drag
const RECOIL_MS = 260;

/** The runway past an end: a quarter of the row, at most 160px, one to one. */
function runwayOf(row: HTMLElement): number {
    return Math.min(160, row.clientWidth / 4);
}

/** How far the cards move for a pull of over px past the end. */
function overshoot(over: number, runway: number): number {
    const a = Math.abs(over);
    if (a <= runway) return over;
    // Beyond the runway: resistance that grows, never quite stopping.
    const extra = a - runway;
    const give = runway * (1 - 1 / (1 + extra / (runway * 2)));
    return Math.sign(over) * (runway + give);
}

function setOver(row: HTMLElement, px: number): void {
    if (px === 0) row.style.removeProperty("--drag-over");
    else row.style.setProperty("--drag-over", `${px}px`);
}

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

        // A recoil still under way: take the cards from where they are.
        const first = row.firstElementChild;
        const shownOver = first ? parseFloat(getComputedStyle(first).translate) || 0 : 0;
        row.classList.remove("drag-recoil");
        setOver(row, shownOver);
        const startX = e.clientX;
        const startLeft = row.scrollLeft;
        const runway = runwayOf(row);
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
                // The press focused the card's link: its buttons would stay up.
                const active = document.activeElement;
                if (active instanceof HTMLElement && row.contains(active)) active.blur();
            }
            const maxLeft = row.scrollWidth - row.clientWidth;
            // Where the row would be with no ends (the cards' offset counts:
            // a drag started mid-recoil).
            const want = startLeft - shownOver - dx;
            const left = Math.max(0, Math.min(maxLeft, want));
            row.scrollLeft = left;
            // Past an end: the cards follow (positive moves them right).
            setOver(row, -overshoot(want - left, runway));
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
            if (row.style.getPropertyValue("--drag-over")) {
                // Back to the end.
                row.classList.add("drag-recoil");
                setOver(row, 0);
                setTimeout(() => row.classList.remove("drag-recoil"), RECOIL_MS + 50);
            }
            // Hover buttons wait for the pointer to move: until then the
            // browser may still count the card it was pressed on as hovered.
            row.classList.add("drag-settling");
            window.addEventListener("pointermove", () => row.classList.remove("drag-settling"), { once: true });
        };

        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", end);
        window.addEventListener("pointercancel", end);
    });
}
