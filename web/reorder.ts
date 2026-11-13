/**
 * Drag to reorder, with Move up / Move down buttons as the keyboard and
 * screen-reader way (docs/notes/Discover.md, section 4a). Used for the
 * shelf rows on the Discover page and the lists on /shelves.
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
            c.querySelector<HTMLButtonElement>("[data-move=up]")!.disabled = i === 0;
            c.querySelector<HTMLButtonElement>("[data-move=down]")!.disabled = i === all.length - 1;
        });
    }

    function controls(item: HTMLElement): HTMLElement {
        const name = opts.label(item);
        const c = document.createElement("span");
        c.className = "reorder";
        c.innerHTML =
            `<span class="reorder-grip" title="Drag to move">${GRIP}</span>` +
            `<button type="button" class="reorder-btn" data-move="up">${UP}</button>` +
            `<button type="button" class="reorder-btn" data-move="down">${DOWN}</button>`;
        c.querySelector("[data-move=up]")!.setAttribute("aria-label", `Move ${name} up`);
        c.querySelector("[data-move=down]")!.setAttribute("aria-label", `Move ${name} down`);
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
        if (btn.dataset["move"] === "up") item.previousElementSibling?.before(item);
        else item.nextElementSibling?.after(item);
        moved(before);
        // Keep the keyboard on the moved item (or its other button at an end).
        const again = item.querySelector<HTMLButtonElement>(`.reorder-btn[data-move=${btn.dataset["move"]}]`);
        (again && !again.disabled ? again : item.querySelector<HTMLButtonElement>(".reorder-btn:not(:disabled)"))?.focus();
    };

    // Dragging by the grip: the item follows the pointer among its siblings.
    let dragging: { item: HTMLElement; before: string[]; id: number } | null = null;
    const onDown = (e: PointerEvent): void => {
        const grip = e.target instanceof Element ? e.target.closest<HTMLElement>(".reorder-grip") : null;
        const item = grip?.closest<HTMLElement>(opts.items);
        if (!grip || !item || item.parentElement !== list || e.button !== 0) return;
        e.preventDefault();
        grip.setPointerCapture(e.pointerId);
        dragging = { item, before: ids(), id: e.pointerId };
        item.classList.add("dragging");
    };
    const onMove = (e: PointerEvent): void => {
        if (!dragging || e.pointerId !== dragging.id) return;
        const { item } = dragging;
        for (const other of items()) {
            if (other === item) continue;
            const r = other.getBoundingClientRect();
            if (e.clientY < r.top || e.clientY > r.bottom) continue;
            const mid = r.top + r.height / 2;
            if (e.clientY < mid && other.previousElementSibling !== item) other.before(item);
            else if (e.clientY >= mid && other.nextElementSibling !== item) other.after(item);
            break;
        }
    };
    const onUp = (e: PointerEvent): void => {
        if (!dragging || e.pointerId !== dragging.id) return;
        const { item, before } = dragging;
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
