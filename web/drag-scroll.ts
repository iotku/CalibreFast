/**
 * Drag to scroll, for the mouse: pressing on a sideways row of books and
 * dragging moves the row with the pointer, instead of dragging the cover
 * image or link out of the page, as a touch screen does.
 *
 *   - Letting go while still: the row stays exactly where it is, so the
 *     cover under the pointer stays under it. No snapping.
 *   - Letting go while moving (a flick): the row carries on at that speed
 *     and slows to a stop, as on a phone. Only a pointer still moving fast
 *     when let go flicks; one that paused first doesn't.
 *   - Past either end there's a runway: the cards keep following the
 *     pointer one to one for a stretch, then with more and more
 *     resistance; letting go there, they recoil back to the end. A flick
 *     that reaches an end runs into the runway and recoils too.
 *   - While dragging (and until the pointer next moves), the cards' hover
 *     buttons and details stay hidden, and the card pressed doesn't keep
 *     focus (which would show them too).
 *
 * A press stops a flick or recoil where it is (opening nothing, as a tap
 * on a phone); so does the wheel. Touch and pens are left to the browser,
 * which already does all this. A press that moves less than DRAG_SLOP is a
 * click as before; one that dragged doesn't open the book it started on.
 */

const DRAG_SLOP = 6; // px before a press becomes a drag
const RECOIL_MS = 260;

// Flicks (speeds in px/ms).
const FLICK_MIN = 0.35; // slower than this at release: no flick
const FLICK_MAX = 6;
const FLICK_WINDOW = 80; // ms of movement the release speed is taken over
const FLICK_STILL = 50; // ms without movement before release: no flick
const FRICTION = 0.995; // speed kept per ms (a phone's feel)
const OVER_FRICTION = 0.96; // per ms, past an end
const FLICK_STOP = 0.02;

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

/** Back to the end, if past it. */
function recoil(row: HTMLElement): void {
    if (!row.style.getPropertyValue("--drag-over")) return;
    row.classList.add("drag-recoil");
    setOver(row, 0);
    setTimeout(() => row.classList.remove("drag-recoil"), RECOIL_MS + 50);
}

/**
 * Where the row is, ends not counted: its scroll less the cards' offset
 * past an end (as shown now, mid-recoil too).
 */
function virtualLeft(row: HTMLElement): number {
    const first = row.firstElementChild;
    const shown = first ? parseFloat(getComputedStyle(first).translate) || 0 : 0;
    return row.scrollLeft - shown;
}

/**
 * Show the row at want, which may be past an end (into the runway); how
 * far past it is.
 */
function place(row: HTMLElement, want: number, runway: number): number {
    const maxLeft = row.scrollWidth - row.clientWidth;
    const left = Math.max(0, Math.min(maxLeft, want));
    row.scrollLeft = left;
    // Past an end: the cards follow (positive moves them right).
    setOver(row, -overshoot(want - left, runway));
    return want - left;
}

const flicks = new WeakMap<HTMLElement, number>(); // the row's animation frame

function stopFlick(row: HTMLElement): void {
    const id = flicks.get(row);
    if (id !== undefined) cancelAnimationFrame(id);
    flicks.delete(row);
}

/** Carry on from want at speed (px/ms, in scroll terms) until it stops. */
function flick(row: HTMLElement, want: number, speed: number, runway: number): void {
    let last = performance.now();
    const step = (now: number): void => {
        const dt = Math.min(32, now - last); // a stalled tab doesn't jump
        last = now;
        want += speed * dt;
        const over = place(row, want, runway);
        // Past an end: much more friction, and never past the runway.
        speed *= Math.pow(over === 0 ? FRICTION : OVER_FRICTION, dt);
        if (over !== 0 && Math.abs(over) >= runway) speed = 0;
        if (Math.abs(speed) < FLICK_STOP) {
            flicks.delete(row);
            recoil(row);
            return;
        }
        flicks.set(row, requestAnimationFrame(step));
    };
    flicks.set(row, requestAnimationFrame(step));
}

/** The click the release about to happen makes: it isn't a click on a card. */
function swallowClick(): void {
    const swallow = (c: MouseEvent): void => {
        c.preventDefault();
        c.stopPropagation();
    };
    window.addEventListener("click", swallow, { capture: true, once: true });
    setTimeout(() => window.removeEventListener("click", swallow, { capture: true }), 0);
}

const reducedMotion = (): boolean => window.matchMedia("(prefers-reduced-motion: reduce)").matches;

/** Make every element matching selector under root drag-scrollable. */
export function setupDragScroll(root: HTMLElement, selector: string): void {
    // The browser's own drag of a cover or link would take the pointer away.
    root.addEventListener("dragstart", (e) => {
        if (e.target instanceof Element && e.target.closest(selector)) e.preventDefault();
    });

    // The wheel takes over from a flick.
    root.addEventListener(
        "wheel",
        (e) => {
            const row = e.target instanceof Element ? e.target.closest<HTMLElement>(selector) : null;
            if (row && flicks.has(row)) {
                stopFlick(row);
                recoil(row);
            }
        },
        { passive: true },
    );

    root.addEventListener("pointerdown", (e) => {
        if (e.pointerType !== "mouse" || e.button !== 0) return;
        const row = e.target instanceof Element ? e.target.closest<HTMLElement>(selector) : null;
        if (!row || row.scrollWidth <= row.clientWidth) return;
        // A button on a card (favorite, want to read) stays a button.
        if (e.target instanceof Element && e.target.closest("button, input, select, textarea, summary")) return;

        // A flick or recoil under way stops where it is, under the pointer;
        // the press only stops it (as a tap on a phone), opening nothing.
        const stopping = flicks.has(row) || row.classList.contains("drag-recoil");
        stopFlick(row);
        const startVirtual = virtualLeft(row);
        row.classList.remove("drag-recoil");
        const runway = runwayOf(row);
        place(row, startVirtual, runway);
        const startX = e.clientX;
        let dragging = false;
        const samples: { t: number; x: number }[] = [{ t: e.timeStamp, x: e.clientX }];

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
            samples.push({ t: ev.timeStamp, x: ev.clientX });
            while (samples.length > 2 && ev.timeStamp - samples[0]!.t > FLICK_WINDOW) samples.shift();
            place(row, startVirtual - dx, runway);
        };

        const end = (ev: PointerEvent): void => {
            if (ev.pointerId !== e.pointerId) return;
            window.removeEventListener("pointermove", move);
            window.removeEventListener("pointerup", end);
            window.removeEventListener("pointercancel", end);
            if (!dragging) {
                if (stopping) swallowClick();
                recoil(row); // pressed mid-flick past an end, and let go
                return;
            }
            // The click this release makes isn't a click on the card.
            swallowClick();

            row.classList.remove("drag-scrolling");
            // Hover buttons wait for the pointer to move: until then the
            // browser may still count the card it was pressed on as hovered.
            row.classList.add("drag-settling");
            window.addEventListener("pointermove", () => row.classList.remove("drag-settling"), { once: true });

            // The speed at release, over the last moments of movement; none
            // if the pointer had stopped first.
            const lastMove = samples[samples.length - 1]!;
            const first = samples[0]!;
            let speed = 0;
            if (ev.type === "pointerup" && ev.timeStamp - lastMove.t <= FLICK_STILL && lastMove.t > first.t) {
                speed = (lastMove.x - first.x) / (lastMove.t - first.t);
            }
            if (Math.abs(speed) < FLICK_MIN || reducedMotion()) {
                recoil(row);
                return;
            }
            speed = Math.max(-FLICK_MAX, Math.min(FLICK_MAX, speed));
            flick(row, startVirtual - (lastMove.x - startX), -speed, runway);
        };

        window.addEventListener("pointermove", move);
        window.addEventListener("pointerup", end);
        window.addEventListener("pointercancel", end);
    });
}
