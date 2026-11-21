/**
 * Where the EPUB reader hears taps, swipes and clicks on a chapter.
 *
 * Each chapter is a document in an iframe sandboxed without allow-scripts
 * (see docs/SECURITY.md). Chromium and Firefox still run listeners that this
 * page puts on that document, so the reader listens there. Safari (WebKit,
 * macOS and iOS) runs none: no tap, swipe, click or key in the chapter ever
 * reaches us, and a tapped link even navigates the frame itself, to a page
 * of our own origin. This page can still read the chapter's DOM and set its
 * selection, though, so on Safari a transparent surface goes over the
 * chapter and takes its input, and the reader looks up what is under a point
 * (elementFromPoint) instead of being told.
 *
 * The surface stands in for what the browser would have done itself:
 *   - Touch: a long press selects the word under the finger (moving on
 *     extends it). Safari then shows its own handles and menu, and only
 *     while the selection lasts the surface leaves holes around the two
 *     ends so the handles can be dragged. Any other touch clears it.
 *   - Mouse: a drag selects, a double click a word, a triple click a
 *     paragraph, shift-click extends. ⌘C copies the chapter's selection.
 * Clicks that were part of making or clearing a selection go no further.
 */

/** How the reader listens to one chapter. */
export interface ChapterInput {
    /** Where to add listeners: the chapter's document, or the surface over it. */
    target: EventTarget;
    /** True when events come from the surface (Safari), not the document. */
    surface: boolean;
    /** A point of an event (its clientX/Y) in the chapter's own coordinates. */
    toChapter(e: { clientX: number; clientY: number }): { x: number; y: number };
    /** The chapter's element under an event. */
    hit(e: Event & { clientX: number; clientY: number }): Element | null;
}

const LONG_PRESS_MS = 500;
const SLOP = 10; // px a touch may wander and still be a press
const HANDLE = 28; // px around each end of a selection left to Safari's handles
const STYLE_ID = "cf-chapter-input";

/**
 * Whether this browser runs listeners added to a document in a frame
 * sandboxed without scripts. Tested, not sniffed: an event is dispatched at
 * the document and we see whether our listener heard it.
 */
function listenersRun(doc: Document): boolean {
    let ran = false;
    const probe = (): void => {
        ran = true;
    };
    doc.addEventListener("cf-probe", probe);
    doc.dispatchEvent(new Event("cf-probe"));
    doc.removeEventListener("cf-probe", probe);
    return ran;
}

/** Sets up input for a chapter just rendered in `view`. */
export function chapterInput(view: { element: HTMLElement; document: Document; iframe?: HTMLIFrameElement }): ChapterInput {
    const doc = view.document;
    if (listenersRun(doc)) {
        return {
            target: doc,
            surface: false,
            toChapter: (e) => ({ x: e.clientX, y: e.clientY }),
            hit: (e) => {
                // The target lives in the iframe's window: `instanceof Element` (ours) is false.
                const t = e.target as Element | null;
                return typeof t?.closest === "function" ? t : null;
            },
        };
    }
    return surfaceOver(view.element, doc, view.iframe ?? (doc.defaultView?.frameElement as HTMLIFrameElement | null) ?? null);
}

function surfaceOver(holder: HTMLElement, doc: Document, frame: HTMLIFrameElement | null): ChapterInput {
    const surface = document.createElement("div");
    surface.className = "chapter-surface";
    surface.setAttribute("aria-hidden", "true");
    holder.append(surface);
    // Links in the chapter can't be activated by the browser while part of it
    // is uncovered (the holes, a long press): it would navigate the frame.
    if (!doc.getElementById(STYLE_ID)) {
        const style = doc.createElement("style");
        style.id = STYLE_ID;
        style.textContent = "html[data-cf-open] :any-link { pointer-events: none !important; }";
        (doc.head ?? doc.documentElement).append(style);
    }

    const frameRect = (): DOMRect => frame?.getBoundingClientRect() ?? holder.getBoundingClientRect();
    const toChapter = (e: { clientX: number; clientY: number }): { x: number; y: number } => {
        const r = frameRect();
        return { x: e.clientX - r.left, y: e.clientY - r.top };
    };
    const at = (e: { clientX: number; clientY: number }): Element | null => {
        const p = toChapter(e);
        return doc.elementFromPoint(p.x, p.y);
    };
    const selection = (): Selection | null => doc.getSelection();
    const selected = (): boolean => {
        const s = selection();
        return !!s && s.rangeCount > 0 && !s.isCollapsed;
    };
    const caretAt = (e: { clientX: number; clientY: number }): Range | null => {
        const p = toChapter(e);
        return doc.caretRangeFromPoint?.(p.x, p.y) ?? null;
    };

    // ---- what the surface covers -------------------------------------------
    // Normally one tile over all of it. With a selection made by touch, tiles
    // around two holes at its ends. While a long press is held, none (the
    // finger's own touch stays with its tile): Safari only shows a selection
    // whose gesture ended in the chapter.
    let tiles: HTMLDivElement[] = [];
    let holes = false;
    const open = (on: boolean): void => {
        if (on) doc.documentElement.setAttribute("data-cf-open", "");
        else doc.documentElement.removeAttribute("data-cf-open");
    };
    const lay = (rects: [number, number, number, number][]): void => {
        // Tiles are reused, never removed: one being touched must stay in the
        // page, or the rest of its touch is lost. Spare ones shrink to nothing.
        while (tiles.length < rects.length) tiles.push(surface.appendChild(document.createElement("div")));
        tiles.forEach((tile, i) => {
            const [x0, y0, x1, y1] = rects[i] ?? [0, 0, 0, 0];
            const s = tile.style;
            s.left = `${x0}px`;
            s.top = `${y0}px`;
            s.width = `${x1 - x0}px`;
            s.height = `${y1 - y0}px`;
        });
    };
    const cover = (): void => {
        holes = false;
        surface.classList.remove("passing");
        open(false);
        lay([[0, 0, surface.clientWidth, surface.clientHeight]]);
    };
    /** Tiles around squares at the selection's two ends (surface coordinates). */
    const layHoles = (): boolean => {
        const s = selection();
        if (!s || !s.rangeCount || s.isCollapsed) return false;
        const rects = s.getRangeAt(0).getClientRects();
        if (!rects.length) return false;
        const f = frameRect();
        const o = surface.getBoundingClientRect();
        const dx = f.left - o.left;
        const dy = f.top - o.top;
        const a = rects[0];
        const b = rects[rects.length - 1];
        const squares = [
            [a.left + dx - HANDLE, a.top + dy - HANDLE, a.left + dx + HANDLE, a.bottom + dy + HANDLE],
            [b.right + dx - HANDLE, b.top + dy - HANDLE, b.right + dx + HANDLE, b.bottom + dy + HANDLE],
        ];
        const W = surface.clientWidth;
        const H = surface.clientHeight;
        const ys = [0, H, ...squares.flatMap((q) => [q[1], q[3]])].map((y) => Math.max(0, Math.min(H, y))).sort((p, q) => p - q);
        const out: [number, number, number, number][] = [];
        for (let i = 0; i + 1 < ys.length; i++) {
            const [y0, y1] = [ys[i], ys[i + 1]];
            if (y1 <= y0) continue;
            const cuts = squares.filter((q) => q[1] < y1 && q[3] > y0).sort((p, q) => p[0] - q[0]);
            let x = 0;
            for (const q of cuts) {
                if (q[0] > x) out.push([x, y0, q[0], y1]);
                x = Math.max(x, q[2]);
            }
            if (x < W) out.push([x, y0, W, y1]);
        }
        holes = true;
        surface.classList.remove("passing");
        open(true);
        lay(out);
        return true;
    };
    cover();
    new ResizeObserver(() => {
        if (!touching && !surface.classList.contains("passing")) {
            if (!holes || !layHoles()) cover();
        }
    }).observe(holder);
    // The handles move the selection without telling anyone, so it is looked
    // at every frame. While it moves, a handle is being dragged: Safari places
    // the dragged end by what is under the finger, so the tiles stand aside
    // (else the selection can't follow the finger past its hole, onto another
    // line). Once it has held still a while, the holes go where its ends are now.
    const STILL_MS = 700;
    let follow = 0;
    const selKey = (): string => {
        const s = selection();
        return s && s.rangeCount && !s.isCollapsed ? [s.anchorOffset, s.focusOffset, s.toString().length, s.anchorNode?.nodeName, s.focusNode?.nodeName].join() : "";
    };
    const watch = (): void => {
        cancelAnimationFrame(follow);
        let last = selKey();
        let movedAt = -Infinity;
        const look = (now: number): void => {
            if (!surface.isConnected || !doc.defaultView) return;
            follow = requestAnimationFrame(look);
            if (touching) return;
            const key = selKey();
            if (!key) {
                cancelAnimationFrame(follow);
                cover();
                return;
            }
            if (key !== last) {
                last = key;
                movedAt = now;
                surface.classList.add("passing");
            } else if (surface.classList.contains("passing") && now - movedAt > STILL_MS) {
                layHoles();
            }
        };
        follow = requestAnimationFrame(look);
    };

    // ---- clicks that only made or cleared a selection go no further ----------
    let swallow = false;
    surface.addEventListener(
        "click",
        (e) => {
            if (!swallow) return;
            swallow = false;
            e.stopImmediatePropagation();
        },
        true,
    );

    // ---- touch: long press selects ------------------------------------------
    let touching = false;
    let lastTouch = -Infinity;
    let press: { x: number; y: number; timer: ReturnType<typeof setTimeout> } | null = null;
    let word: Range | null = null; // what the long press selected
    const selectWord = (p: { clientX: number; clientY: number }): boolean => {
        const caret = caretAt(p);
        const s = selection();
        if (!caret || !s) return false;
        s.removeAllRanges();
        s.addRange(caret);
        s.modify("move", "backward", "word");
        s.modify("extend", "forward", "word");
        if (!s.rangeCount || s.isCollapsed) return false;
        // Only if the word is where the finger is (not the nearest one to a margin).
        const { x, y } = toChapter(p);
        const hitWord = [...s.getRangeAt(0).getClientRects()].some((r) => x >= r.left - 4 && x <= r.right + 4 && y >= r.top - 4 && y <= r.bottom + 4);
        if (!hitWord || !s.toString().trim()) {
            s.removeAllRanges();
            return false;
        }
        return true;
    };
    /** Selects from the long-pressed word to a point. */
    const extendTo = (p: { clientX: number; clientY: number }): void => {
        const caret = caretAt(p);
        const s = selection();
        if (!caret || !s || !word) return;
        // Before the word: from its end back to the finger; after it: from
        // its start on to the finger; on it: just the word.
        const where = word.comparePoint(caret.startContainer, caret.startOffset);
        if (where < 0) s.setBaseAndExtent(word.endContainer, word.endOffset, caret.startContainer, caret.startOffset);
        else if (where > 0) s.setBaseAndExtent(word.startContainer, word.startOffset, caret.startContainer, caret.startOffset);
        else s.setBaseAndExtent(word.startContainer, word.startOffset, word.endContainer, word.endOffset);
    };

    surface.addEventListener(
        "touchstart",
        (e) => {
            touching = true;
            lastTouch = e.timeStamp;
            swallow = false; // (a long press may have had no click to swallow)
            clearTimeout(press?.timer);
            press = null;
            word = null;
            const t = e.touches.length === 1 ? e.touches[0] : undefined;
            if (selected()) {
                // A touch anywhere but the handles clears the selection, and does
                // nothing else (no turn, no link).
                selection()?.removeAllRanges();
                swallow = true;
                if (holes) cover();
            }
            if (!t) return;
            const p = { clientX: t.clientX, clientY: t.clientY };
            press = {
                x: t.clientX,
                y: t.clientY,
                timer: setTimeout(() => {
                    if (!press) return;
                    swallow = true; // a long press is never a tap
                    if (!selectWord(p)) return;
                    word = selection()?.getRangeAt(0).cloneRange() ?? null;
                    surface.classList.add("passing");
                    open(true);
                }, LONG_PRESS_MS),
            };
        },
        { passive: true },
    );
    surface.addEventListener(
        "touchmove",
        (e) => {
            const t = e.touches[0];
            if (!t) return;
            if (word) {
                e.preventDefault(); // dragging out a selection, not scrolling
                extendTo(t);
                return;
            }
            if (press && Math.hypot(t.clientX - press.x, t.clientY - press.y) > SLOP) {
                clearTimeout(press.timer);
                press = null;
            }
        },
        { passive: false },
    );
    const touchEnd = (e: TouchEvent): void => {
        if (e.touches.length) return;
        touching = false;
        lastTouch = e.timeStamp;
        clearTimeout(press?.timer);
        press = null;
        if (!word) return;
        word = null;
        // Give Safari a moment to put up its handles, then cover all but them.
        setTimeout(() => {
            if (touching) return;
            if (layHoles()) watch();
            else cover();
        }, 400);
    };
    surface.addEventListener("touchend", touchEnd, { passive: true });
    surface.addEventListener("touchcancel", touchEnd, { passive: true });

    // ---- mouse: drag, double and triple click, shift-click -------------------
    // (Mouse events Safari makes up after a tap are left alone.)
    let anchor: Range | null = null;
    let from: { x: number; y: number } | null = null;
    let dragged = false;
    const isMouse = (e: MouseEvent): boolean => e.timeStamp - lastTouch > 1000;
    surface.addEventListener("mousedown", (e) => {
        if (e.button !== 0 || !isMouse(e)) return;
        swallow = false;
        const s = selection();
        anchor = null;
        dragged = false;
        if (!s) return;
        if (e.detail >= 2) {
            const caret = caretAt(e);
            if (!caret) return;
            s.removeAllRanges();
            s.addRange(caret);
            const unit = e.detail === 2 ? "word" : "paragraphboundary";
            s.modify("move", "backward", unit);
            s.modify("extend", "forward", unit);
            swallow = true;
            e.preventDefault();
            return;
        }
        const caret = caretAt(e);
        if (e.shiftKey && s.rangeCount && caret) {
            s.extend(caret.startContainer, caret.startOffset);
            swallow = true;
            e.preventDefault();
            return;
        }
        if (!s.isCollapsed) {
            s.removeAllRanges();
            swallow = true;
        }
        anchor = caret;
        from = { x: e.clientX, y: e.clientY };
        e.preventDefault(); // no selecting in this page
    });
    // On the window, so a drag can carry on past the chapter's edge.
    const mouseMove = (e: MouseEvent): void => {
        if (!surface.isConnected) {
            window.removeEventListener("mousemove", mouseMove);
            window.removeEventListener("mouseup", mouseUp);
            return;
        }
        if (anchor && from && e.buttons & 1) {
            if (!dragged && Math.hypot(e.clientX - from.x, e.clientY - from.y) < 4) return;
            dragged = true;
            const caret = caretAt(e);
            if (caret) selection()?.setBaseAndExtent(anchor.startContainer, anchor.startOffset, caret.startContainer, caret.startOffset);
            return;
        }
        if (e.target === surface || surface.contains(e.target as Node)) {
            surface.style.cursor = at(e)?.closest(":any-link") ? "pointer" : "";
        }
    };
    const mouseUp = (): void => {
        if (anchor && dragged) swallow = true;
        anchor = null;
        from = null;
    };
    window.addEventListener("mousemove", mouseMove);
    window.addEventListener("mouseup", mouseUp);

    // ⌘C / Ctrl+C: focus stays in this page, so its copy would find nothing.
    const copy = (e: ClipboardEvent): void => {
        if (!surface.isConnected) {
            document.removeEventListener("copy", copy);
            return;
        }
        const text = selection()?.toString();
        if (!text || window.getSelection()?.toString()) return;
        e.clipboardData?.setData("text/plain", text);
        e.preventDefault();
    };
    document.addEventListener("copy", copy);

    return {
        target: surface,
        surface: true,
        toChapter,
        hit: (e) => {
            const el = at(e);
            if (!el) return null;
            // While links can't be hit (above), they're looked for by position.
            if (el.closest(":any-link") || !doc.documentElement.hasAttribute("data-cf-open")) return el;
            const { x, y } = toChapter(e);
            for (const a of doc.querySelectorAll(":any-link")) {
                for (const r of a.getClientRects()) {
                    if (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) return a;
                }
            }
            return el;
        },
    };
}
