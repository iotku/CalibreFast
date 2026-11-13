/**
 * Drag to reorder, with Move to start / up / down / to end buttons as the
 * keyboard and screen-reader way (docs/notes/Discover.md, section 4a). Used
 * for the rows on the Discover page, the lists on /shelves, and the books
 * of a list or shelf (list-edit.ts).
 *
 * Each item has data-id. After every move, onChange gets the whole list in
 * its new order: the server stores it as given, so two tabs end with the
 * last one's order rather than a mix.
 */

export interface Reorderable {
    /** Take the handles and buttons away again. */
    stop(): void;
}

export interface ReorderOptions {
    list: HTMLElement;
    /** Selector for the items: children of list. */
    items: string;
    /** The item's name, for the buttons' labels. */
    label(item: HTMLElement): string;
    onChange(ids: string[]): void;
    /** Where in the item the controls go (default: the item itself). */
    slot?(item: HTMLElement): HTMLElement;
}

const GRIP =
    '<svg viewBox="0 0 24 24" width="16" height="16" fill="currentColor" aria-hidden="true">' +
    '<circle cx="9" cy="6" r="1.6"/><circle cx="15" cy="6" r="1.6"/><circle cx="9" cy="12" r="1.6"/>' +
    '<circle cx="15" cy="12" r="1.6"/><circle cx="9" cy="18" r="1.6"/><circle cx="15" cy="18" r="1.6"/></svg>';
const UP = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 15l6-6 6 6"/></svg>';
const FIRST = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 5h12M6 17l6-6 6 6"/></svg>';
const LAST = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 19h12M6 7l6 6 6-6"/></svg>';
const DOWN = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 9l6 6 6-6"/></svg>';

export function makeReorderable(opts: ReorderOptions): Reorderable {
    const { list } = opts;
    const items = (): HTMLElement[] => [...list.querySelectorAll<HTMLElement>(`:scope > ${opts.items}`)];
    const ids = (): string[] => items().map((el) => el.dataset["id"] ?? "");

    function update(): void {
        const all = items();
        all.forEach((el, i) => {
            const c = el.querySelector<HTMLElement>(":scope .reorder");
            if (!c) return;
            for (const b of c.querySelectorAll<HTMLButtonElement>(".reorder-btn")) {
                const m = b.dataset["move"];
                b.disabled = m === "first" || m === "up" ? i === 0 : i === all.length - 1;
            }
        });
    }

    function controls(item: HTMLElement): HTMLElement {
        const name = opts.label(item);
        const c = document.createElement("span");
        c.className = "reorder";
        c.innerHTML =
            `<span class="reorder-grip" title="Drag to move">${GRIP}</span>` +
            `<button type="button" class="reorder-btn" data-move="first">${FIRST}</button>` +
            `<button type="button" class="reorder-btn" data-move="up">${UP}</button>` +
            `<button type="button" class="reorder-btn" data-move="down">${DOWN}</button>` +
            `<button type="button" class="reorder-btn" data-move="last">${LAST}</button>`;
        const labels: Record<string, string> = { first: "to the start", up: "up", down: "down", last: "to the end" };
        for (const b of c.querySelectorAll<HTMLButtonElement>(".reorder-btn")) {
            const label = `Move ${name} ${labels[b.dataset["move"] ?? ""]}`;
            b.setAttribute("aria-label", label);
            b.title = label.replace(` ${name} `, " ");
        }
        return c;
    }

    for (const item of items()) (opts.slot?.(item) ?? item).prepend(controls(item));
    list.classList.add("reordering");
    update();

    function moved(before: string[]): void {
        update();
        const after = ids();
        if (after.join() !== before.join()) opts.onChange(after);
    }

    const onClick = (e: MouseEvent): void => {
        const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".reorder-btn") : null;
        const item = btn?.closest<HTMLElement>(opts.items);
        if (!btn || !item || item.parentElement !== list) return;
        e.preventDefault();
        const before = ids();
        const all = items();
        const move = btn.dataset["move"];
        if (move === "up") all[all.indexOf(item) - 1]?.before(item);
        else if (move === "down") all[all.indexOf(item) + 1]?.after(item);
        else if (move === "first" && all[0] !== item) all[0]?.before(item);
        else if (move === "last" && all.at(-1) !== item) all.at(-1)?.after(item);
        moved(before);
        if (move === "first" || move === "last") item.scrollIntoView({ block: "nearest" });
        // Keep the keyboard on the moved item (or its other button at an end).
        const again = item.querySelector<HTMLButtonElement>(`.reorder-btn[data-move=${btn.dataset["move"]}]`);
        (again && !again.disabled ? again : item.querySelector<HTMLButtonElement>(".reorder-btn:not(:disabled)"))?.focus();
    };

    // Dragging by the grip: the item follows the pointer among its siblings,
    // and the page scrolls when it's held near the top or bottom.
    let dragging: { item: HTMLElement; before: string[]; id: number; y: number; frame: number } | null = null;
    const EDGE = 56; // px from the screen's edge (below the top bar) where it scrolls
    const top = (): number => document.querySelector<HTMLElement>(".top-bar")?.getBoundingClientRect().bottom ?? 0;

    /** Puts the dragged item where y is: before the first item whose middle is below y. */
    function place(y: number): void {
        if (!dragging) return;
        const { item } = dragging;
        const all = items();
        let lo = 0;
        let hi = all.length; // binary search: the items are top to bottom
        while (lo < hi) {
            const mid = (lo + hi) >> 1;
            const r = all[mid]!.getBoundingClientRect();
            if (y < r.top + r.height / 2) hi = mid;
            else lo = mid + 1;
        }
        const next = all[lo];
        if (next === item || next?.previousElementSibling === item) return;
        if (next) next.before(item);
        else if (all.at(-1) !== item) all.at(-1)?.after(item);
    }

    function autoScroll(): void {
        if (!dragging) return;
        const { y } = dragging;
        const t = top();
        const step =
            y < t + EDGE ? -Math.ceil((t + EDGE - y) / 4) : y > window.innerHeight - EDGE ? Math.ceil((y - window.innerHeight + EDGE) / 4) : 0;
        if (step) {
            window.scrollBy(0, step);
            place(y);
        }
        dragging.frame = requestAnimationFrame(autoScroll);
    }

    const onDown = (e: PointerEvent): void => {
        const grip = e.target instanceof Element ? e.target.closest<HTMLElement>(".reorder-grip") : null;
        const item = grip?.closest<HTMLElement>(opts.items);
        if (!grip || !item || item.parentElement !== list || e.button !== 0) return;
        e.preventDefault();
        grip.setPointerCapture(e.pointerId);
        dragging = { item, before: ids(), id: e.pointerId, y: e.clientY, frame: 0 };
        dragging.frame = requestAnimationFrame(autoScroll);
        item.classList.add("dragging");
    };
    const onMove = (e: PointerEvent): void => {
        if (!dragging || e.pointerId !== dragging.id) return;
        dragging.y = e.clientY;
        // Over the dragged item itself: it stays.
        const r = dragging.item.getBoundingClientRect();
        if (e.clientY >= r.top && e.clientY <= r.bottom) return;
        place(e.clientY);
    };
    const onUp = (e: PointerEvent): void => {
        if (!dragging || e.pointerId !== dragging.id) return;
        const { item, before, frame } = dragging;
        cancelAnimationFrame(frame);
        dragging = null;
        item.classList.remove("dragging");
        moved(before);
    };

    list.addEventListener("click", onClick);
    list.addEventListener("pointerdown", onDown);
    list.addEventListener("pointermove", onMove);
    list.addEventListener("pointerup", onUp);
    list.addEventListener("pointercancel", onUp);

    return {
        stop(): void {
            list.removeEventListener("click", onClick);
            list.removeEventListener("pointerdown", onDown);
            list.removeEventListener("pointermove", onMove);
            list.removeEventListener("pointerup", onUp);
            list.removeEventListener("pointercancel", onUp);
            list.classList.remove("reordering");
            for (const c of list.querySelectorAll(".reorder")) c.remove();
        },
    };
}
