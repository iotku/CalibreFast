/**
 * Drag to scroll, for the mouse, as a touch screen does: pressing on a
 * sideways row of books (setupDragScroll) or on the library grid
 * (setupPageDragScroll, which scrolls the page) and dragging moves it with
 * the pointer, instead of dragging the cover image or link out of the page.
 *
 *   - Letting go while still: it stays exactly where it is, so the cover
 *     under the pointer stays under it. No snapping.
 *   - Letting go while moving (a flick): it carries on at that speed and
 *     slows to a stop, as on a phone. Only a pointer still moving fast
 *     when let go flicks; one that paused first doesn't.
 *   - Past either end there's a runway: the books keep following the
 *     pointer one to one for a stretch, then with more and more
 *     resistance; letting go there, they recoil back to the end. A flick
 *     that reaches an end runs into the runway and recoils too.
 *   - While dragging (and until the pointer next moves), the cards' hover
 *     buttons and details stay hidden, and the card pressed doesn't keep
 *     focus (which would show them too).
 *
 * Shift-click never drags: it opens the book however much the pointer
 * moves, for unsteady hands (docs/FEATURES.md).
 *
 * A press stops a flick or recoil where it is (opening nothing, as a tap
 * on a phone); so does the wheel, or a key. Touch and pens are left to the
 * browser, which already does all this. A press that moves less than
 * DRAG_SLOP is a click as before; one that dragged doesn't open the book it
 * started on.
 *
 * Reduced motion (the system's setting; on Linux, desktop animations off)
 * keeps the flick, as phones do: it's the user's own throw carried on, and
 * scrolling far without it is hard work. Only the recoil, the page's own
 * flourish, is instant then (style.css).
 *
 * The books' offset past an end is --drag-over on the element dragged, a
 * translate value; style.css moves the books by it.
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

/** What a drag scrolls: a row (sideways), or the page (down). */
interface Scroller {
    el: HTMLElement; // pressed on; carries the classes and --drag-over
    axis: "x" | "y";
    pos(): number;
    setPos(v: number): void;
    max(): number;
    size(): number; // of the view, for the runway
    moved(): Element | null; // an element moved by --drag-over, to read it back
}

function rowScroller(row: HTMLElement): Scroller {
    return {
        el: row,
        axis: "x",
        pos: () => row.scrollLeft,
        setPos: (v) => (row.scrollLeft = v),
        max: () => row.scrollWidth - row.clientWidth,
        size: () => row.clientWidth,
        moved: () => row.firstElementChild,
    };
}

function pageScroller(el: HTMLElement): Scroller {
    return {
        el,
        axis: "y",
        pos: () => window.scrollY,
        setPos: (v) => window.scrollTo({ top: v, behavior: "instant" }),
        max: () => document.documentElement.scrollHeight - window.innerHeight,
        size: () => window.innerHeight,
        moved: () => el,
    };
}

/** The runway past an end: a quarter of the view, at most 160px, one to one. */
function runwayOf(s: Scroller): number {
    return Math.min(160, s.size() / 4);
}

/** How far the books move for a pull of over px past the end. */
function overshoot(over: number, runway: number): number {
    const a = Math.abs(over);
    if (a <= runway) return over;
    // Beyond the runway: resistance that grows, never quite stopping.
    const extra = a - runway;
    const give = runway * (1 - 1 / (1 + extra / (runway * 2)));
    return Math.sign(over) * (runway + give);
}

function setOver(s: Scroller, px: number): void {
    if (px === 0) s.el.style.removeProperty("--drag-over");
    else s.el.style.setProperty("--drag-over", s.axis === "x" ? `${px}px` : `0 ${px}px`);
}

/** Back to the end, if past it; then done (at once if it wasn't). */
function recoil(s: Scroller, done?: () => void): void {
    if (!s.el.style.getPropertyValue("--drag-over")) {
        done?.();
        return;
    }
    s.el.classList.add("drag-recoil");
    setOver(s, 0);
    setTimeout(() => {
        s.el.classList.remove("drag-recoil");
        done?.();
    }, RECOIL_MS + 50);
}

/**
 * Where it is, ends not counted: its scroll less the books' offset past an
 * end (as shown now, mid-recoil too).
 */
function virtualPos(s: Scroller): number {
    const m = s.moved();
    const parts = m ? getComputedStyle(m).translate.split(" ") : [];
    const shown = parseFloat(parts[s.axis === "x" ? 0 : 1] ?? "") || 0;
    return s.pos() - shown;
}

/** Show it at want, which may be past an end (into the runway); how far past. */
function place(s: Scroller, want: number, runway: number): number {
    const at = Math.max(0, Math.min(s.max(), want));
    s.setPos(at);
    // Past an end: the books follow (positive moves them right or down).
    setOver(s, -overshoot(want - at, runway));
    return want - at;
}

const flicks = new WeakMap<HTMLElement, number>(); // the animation frame

function stopFlick(s: Scroller): void {
    const id = flicks.get(s.el);
    if (id !== undefined) cancelAnimationFrame(id);
    flicks.delete(s.el);
}

/** Carry on from want at speed (px/ms, in scroll terms) until it stops; then done. */
function flick(s: Scroller, want: number, speed: number, runway: number, done: () => void): void {
    let last = performance.now();
    const step = (now: number): void => {
        const dt = Math.min(32, now - last); // a stalled tab doesn't jump
        last = now;
        want += speed * dt;
        const over = place(s, want, runway);
        // Past an end: much more friction, and never past the runway.
        speed *= Math.pow(over === 0 ? FRICTION : OVER_FRICTION, dt);
        if (over !== 0 && Math.abs(over) >= runway) speed = 0;
        if (Math.abs(speed) < FLICK_STOP) {
            flicks.delete(s.el);
            recoil(s, done);
            return;
        }
        flicks.set(s.el, requestAnimationFrame(step));
    };
    flicks.set(s.el, requestAnimationFrame(step));
}

/** The wheel (or a key) takes over from a flick. */
function interrupt(s: Scroller): void {
    if (!flicks.has(s.el)) return;
    stopFlick(s);
    recoil(s);
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

/** A press that may drag: a mouse's main button, not on a control, no Shift. */
function draggable(e: PointerEvent): boolean {
    if (e.pointerType !== "mouse" || e.button !== 0 || e.shiftKey) return false;
    // A button on a card (favorite, want to read) stays a button.
    return !(e.target instanceof Element && e.target.closest("button, input, select, textarea, summary"));
}

/** Follow a press on s: a drag, then maybe a flick. */
function press(e: PointerEvent, s: Scroller): void {
    // A flick or recoil under way stops where it is, under the pointer;
    // the press only stops it (as a tap on a phone), opening nothing.
    const stopping = flicks.has(s.el) || s.el.classList.contains("drag-recoil");
    stopFlick(s);
    const start = virtualPos(s);
    s.el.classList.remove("drag-recoil");
    const runway = runwayOf(s);
    place(s, start, runway);
    const coord = (ev: PointerEvent): number => (s.axis === "x" ? ev.clientX : ev.clientY);
    const from = coord(e);
    let dragging = false;
    const samples: { t: number; at: number }[] = [{ t: e.timeStamp, at: from }];

    const move = (ev: PointerEvent): void => {
        if (ev.pointerId !== e.pointerId) return;
        const d = coord(ev) - from;
        if (!dragging) {
            if (Math.abs(d) < DRAG_SLOP) return;
            dragging = true;
            s.el.classList.add("drag-scrolling");
            s.el.setPointerCapture(e.pointerId);
            window.getSelection()?.removeAllRanges();
            // The press focused the card's link: its buttons would stay up.
            const active = document.activeElement;
            if (active instanceof HTMLElement && s.el.contains(active)) active.blur();
        }
        samples.push({ t: ev.timeStamp, at: coord(ev) });
        while (samples.length > 2 && ev.timeStamp - samples[0]!.t > FLICK_WINDOW) samples.shift();
        place(s, start - d, runway);
    };

    const end = (ev: PointerEvent): void => {
        if (ev.pointerId !== e.pointerId) return;
        window.removeEventListener("pointermove", move);
        window.removeEventListener("pointerup", end);
        window.removeEventListener("pointercancel", end);
        if (!dragging) {
            if (stopping) swallowClick();
            recoil(s); // pressed mid-flick past an end, and let go
            return;
        }
        // The click this release makes isn't a click on the card.
        swallowClick();
        s.el.classList.remove("drag-scrolling");
        // Until the pointer next moves, the browser may count the wrong card
        // as hovered (the one the press began on, which held the pointer):
        // only the card actually under the pointer shows its buttons, once
        // the books have stopped (after a flick, or the recoil).
        s.el.classList.add("drag-settling");
        let under: HTMLElement | null = null;
        const settle = (): void => {
            s.el.classList.remove("drag-settling");
            under?.classList.remove("drag-hover");
        };
        window.addEventListener("pointermove", settle, { once: true });
        const markUnder = (): void => {
            if (!s.el.classList.contains("drag-settling")) return; // moved since: the browser knows
            const hit = document.elementFromPoint(ev.clientX, ev.clientY);
            under = hit && s.el.contains(hit) ? hit.closest<HTMLElement>(".book") : null;
            under?.classList.add("drag-hover");
        };

        // The speed at release, over the last moments of movement; none if
        // the pointer had stopped first.
        const lastMove = samples[samples.length - 1]!;
        const first = samples[0]!;
        let speed = 0;
        if (ev.type === "pointerup" && ev.timeStamp - lastMove.t <= FLICK_STILL && lastMove.t > first.t) {
            speed = (lastMove.at - first.at) / (lastMove.t - first.t);
        }
        if (Math.abs(speed) < FLICK_MIN) {
            recoil(s, markUnder);
            return;
        }
        speed = Math.max(-FLICK_MAX, Math.min(FLICK_MAX, speed));
        flick(s, start - (lastMove.at - from), -speed, runway, markUnder);
    };

    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", end);
    window.addEventListener("pointercancel", end);
}

/**
 * The browser's own drag of a cover or link would take the pointer away.
 * And Shift-click opens a book however much the pointer moved: the way to
 * click for an unsteady hand. (The browser would open the link in a new
 * window instead; in select mode, Shift-click selects as a click does.)
 */
function noNativeDrag(root: HTMLElement, selector: string): void {
    root.addEventListener("dragstart", (e) => {
        if (e.target instanceof Element && e.target.closest(selector)) e.preventDefault();
    });
    // Shift-press: not a text selection from the last click to here.
    root.addEventListener("mousedown", (e) => {
        if (e.shiftKey && e.button === 0 && e.target instanceof Element && e.target.closest(`${selector} a.book-link`)) e.preventDefault();
    });
    root.addEventListener(
        "click",
        (e) => {
            if (!e.shiftKey || e.button !== 0 || document.body.classList.contains("selecting")) return;
            const link = e.target instanceof Element ? e.target.closest<HTMLAnchorElement>("a.book-link") : null;
            const href = link?.getAttribute("href");
            if (!link || !href?.startsWith("#") || !(e.target instanceof Element && e.target.closest(selector))) return;
            e.preventDefault();
            window.location.hash = href;
        },
        true,
    );
}

/** Make every row matching selector under root drag-scrollable, sideways. */
export function setupDragScroll(root: HTMLElement, selector: string): void {
    noNativeDrag(root, selector);
    const rowOf = (t: EventTarget | null): HTMLElement | null =>
        t instanceof Element ? t.closest<HTMLElement>(selector) : null;
    root.addEventListener(
        "wheel",
        (e) => {
            const row = rowOf(e.target);
            if (row) interrupt(rowScroller(row));
        },
        { passive: true },
    );
    root.addEventListener("pointerdown", (e) => {
        const row = rowOf(e.target);
        if (!row || !draggable(e) || row.scrollWidth <= row.clientWidth) return;
        press(e, rowScroller(row));
    });
}

/** Make el drag-scrollable up and down: dragging it scrolls the page. */
export function setupPageDragScroll(el: HTMLElement): void {
    noNativeDrag(el, "*");
    const s = pageScroller(el);
    window.addEventListener("wheel", () => interrupt(s), { passive: true });
    window.addEventListener("keydown", () => interrupt(s));
    el.addEventListener("pointerdown", (e) => {
        if (!draggable(e) || s.max() <= 0) return;
        press(e, s);
    });
}
