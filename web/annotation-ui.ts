/**
 * What the reader sees of highlights and notes, shared by both readers
 * (markup in templates/reader-parts.html):
 *   - a small toolbar by a text selection (highlight in a colour, add a
 *     note, copy) or by a highlight that was tapped (recolour, edit the
 *     note, delete);
 *   - the note editor;
 *   - the Notes tab of the sidebar.
 *
 * The readers find selections and draw highlights themselves (an EPUB's
 * are CFI ranges in iframes, a PDF's are rectangles on a page); they call
 * offerNew / offerExisting with where on screen the toolbar belongs.
 */

import { COLORS, type Annotation, type AnnotationStore } from "./annotations.js";
import { byId, el } from "./dom.js";

export interface AnnotationUI {
    /**
     * Shows the toolbar by a selection. make() turns the selection into an
     * annotation in the colour chosen (and clears the selection).
     */
    offerNew(at: DOMRect, text: string, make: (color: string) => Annotation | null): void;
    /** Shows the toolbar by an existing highlight. */
    offerExisting(at: DOMRect, a: Annotation): void;
    hide(): void;
    /** Hides the toolbar. True if it was showing (the press that did it shouldn't also turn the page). */
    dismiss(): boolean;
    /** Fills the sidebar's Notes tab. */
    renderList(): void;
}

export interface AnnotationUIOptions {
    store: AnnotationStore;
    /** Go to an annotation from the list. */
    onGo(a: Annotation): void;
    /** Order of the list: negative when a comes before b in the book. */
    compare(a: Annotation, b: Annotation): number;
    toast(text: string): void;
}

/** How each colour is painted (also in web/reader.css, for the buttons). */
export const COLOR_VALUES: Record<string, string> = {
    yellow: "#ffd60a",
    green: "#4cd964",
    cyan: "#3cc8f0",
    purple: "#b884ff",
    red: "#ff6b6b",
    orange: "#ff9f1c",
    // KOReader's other colours, should an import bring them.
    olive: "#9aa33a",
    blue: "#5b8def",
    gray: "#a0a0a0",
};
export const colorValue = (name: string): string => COLOR_VALUES[name] ?? "#ffd60a";

const LAST_COLOR_KEY = "cf-highlight-color";

export function setupAnnotationUI(opts: AnnotationUIOptions): AnnotationUI {
    const { store } = opts;
    const bar = byId("annot-bar");
    const noteBtn = byId("annot-note");
    const copyBtn = byId("annot-copy");
    const deleteBtn = byId("annot-delete");
    const dialog = byId("note-dialog", HTMLDialogElement);
    const noteText = byId("note-text", HTMLTextAreaElement);
    const noteQuote = byId("note-quote");
    const list = byId("annotation-list");

    /** What the toolbar is for: a selection to turn into an annotation, or an existing one. */
    let target: { make: (color: string) => Annotation | null; text: string } | { existing: Annotation } | null = null;

    const lastColor = (): string => {
        try {
            const c = localStorage.getItem(LAST_COLOR_KEY) ?? "";
            return (COLORS as readonly string[]).includes(c) ? c : COLORS[0];
        } catch {
            return COLORS[0];
        }
    };

    const colorButtons = COLORS.map((c) => {
        const b = el("button", { type: "button", className: "hl-color", title: `Highlight ${c}` });
        b.setAttribute("aria-label", `Highlight ${c}`);
        b.style.setProperty("--hl", colorValue(c));
        b.addEventListener("click", () => pick(c));
        return b;
    });
    bar.prepend(...colorButtons);

    function hide(): void {
        bar.hidden = true;
        target = null;
    }

    /** Above the selection when there's room, otherwise below; never off screen. */
    function show(at: DOMRect, current: string | null): void {
        bar.hidden = false;
        deleteBtn.hidden = !target || !("existing" in target);
        COLORS.forEach((c, i) => colorButtons[i]?.setAttribute("aria-pressed", String(c === current)));
        const w = bar.offsetWidth;
        const h = bar.offsetHeight;
        const left = Math.min(window.innerWidth - w - 8, Math.max(8, at.left + at.width / 2 - w / 2));
        let top = at.top - h - 10;
        if (top < 56) top = Math.min(window.innerHeight - h - 8, at.bottom + 10);
        bar.style.left = `${left}px`;
        bar.style.top = `${top}px`;
    }

    /** The annotation the toolbar is about, made now if it was only a selection. */
    function resolve(color: string): Annotation | null {
        if (!target) return null;
        if ("existing" in target) return target.existing;
        return target.make(color);
    }

    function pick(color: string): void {
        if (!target) return;
        try {
            localStorage.setItem(LAST_COLOR_KEY, color);
        } catch {
            /* not remembered */
        }
        if ("existing" in target) store.update(target.existing.id, { color });
        else target.make(color);
        hide();
    }

    // ---- the note editor ----------------------------------------------------

    let editing: Annotation | null = null;
    function editNote(a: Annotation): void {
        editing = a;
        noteQuote.textContent = a.text;
        noteQuote.hidden = !a.text;
        noteQuote.style.setProperty("--hl", colorValue(a.color));
        noteText.value = a.note;
        dialog.showModal();
        noteText.focus();
    }
    dialog.addEventListener("close", () => {
        if (editing && dialog.returnValue === "save") store.update(editing.id, { note: noteText.value.trim() });
        editing = null;
    });
    // Ctrl/⌘+Enter saves, as in most note boxes.
    noteText.addEventListener("keydown", (e) => {
        if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) dialog.close("save");
    });

    noteBtn.addEventListener("click", () => {
        const a = resolve(lastColor());
        hide();
        if (a) editNote(a);
    });
    copyBtn.addEventListener("click", () => {
        const text = target && "existing" in target ? target.existing.text : (target?.text ?? "");
        hide();
        void navigator.clipboard?.writeText(text).then(
            () => opts.toast("Copied"),
            () => opts.toast("Couldn't copy"),
        );
    });
    deleteBtn.addEventListener("click", () => {
        if (target && "existing" in target) store.remove(target.existing.id);
        hide();
    });
    // The toolbar's own buttons mustn't clear the selection they act on.
    bar.addEventListener("pointerdown", (e) => e.preventDefault());

    // ---- the list -----------------------------------------------------------

    function renderList(): void {
        const items = store.list().sort(opts.compare);
        list.replaceChildren();
        if (!items.length) {
            list.append(el("li", { className: "toc-empty", textContent: "No highlights yet. Select some text to highlight it or add a note." }));
            return;
        }
        for (const a of items) {
            const where = [a.chapter, a.page ? `p. ${a.page}` : ""].filter(Boolean).join(" · ");
            const go = el(
                "a",
                { href: "#", className: "annot-go" },
                el("span", { className: "hit-where", textContent: where }),
                el("span", { className: "annot-text", textContent: a.text || "(no text)" }),
            );
            if (a.note) go.append(el("span", { className: "annot-note", textContent: a.note }));
            go.style.setProperty("--hl", colorValue(a.color));
            go.addEventListener("click", (e) => {
                e.preventDefault();
                opts.onGo(a);
            });
            const edit = el("button", { type: "button", className: "bookmark-delete", title: a.note ? "Edit note" : "Add a note", textContent: "✎" });
            edit.setAttribute("aria-label", a.note ? "Edit note" : "Add a note");
            edit.addEventListener("click", () => editNote(a));
            const del = el("button", { type: "button", className: "bookmark-delete", title: "Delete highlight", textContent: "×" });
            del.setAttribute("aria-label", "Delete highlight");
            del.addEventListener("click", () => store.remove(a.id));
            list.append(el("li", { className: "bookmark-item annot-item" }, go, edit, del));
        }
    }

    return {
        offerNew(at, text, make) {
            target = { make, text };
            show(at, null);
        },
        offerExisting(at, a) {
            target = { existing: a };
            show(at, a.color);
        },
        hide,
        dismiss() {
            const was = !bar.hidden;
            hide();
            return was;
        },
        renderList,
    };
}
