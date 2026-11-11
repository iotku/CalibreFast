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
import { byId, el, errorMessage } from "./dom.js";

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
    /**
     * Exchanging highlights with KOReader (koreader_annotations.go), when
     * signed in: the book, and a sync with the server to run before an
     * export and after an import.
     */
    koreader?: { uuid: string; format: string; sync(): Promise<void> };
}

/** What an import did (Go: koreaderImport). */
interface ImportResult {
    added: number;
    updated: number;
    unchanged: number;
    skipped: number;
    bookmarks: number;
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

/** "1 highlight", "3 highlights". */
const count = (n: number, what: string): string => `${n} ${what}${n === 1 ? "" : "s"}`;

function setupKOReader({ uuid, format, sync }: { uuid: string; format: string; sync(): Promise<void> }): void {
    const url = `/api/me/annotations/${encodeURIComponent(uuid)}/koreader?format=${format}`;
    const status = byId("ko-status");
    const fileInput = byId("ko-file", HTMLInputElement);
    byId("koreader-tools").hidden = false;

    byId("ko-export").addEventListener("click", async () => {
        status.textContent = "Preparing…";
        try {
            await sync(); // what was made offline goes too
            const res = await fetch(url, { credentials: "same-origin" });
            if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
            const name = /filename="?([^";]+)"?/.exec(res.headers.get("content-disposition") ?? "")?.[1] ?? `book.${format}.annotations.lua`;
            const link = el("a", { href: URL.createObjectURL(await res.blob()), download: name });
            link.click();
            setTimeout(() => URL.revokeObjectURL(link.href), 10_000);
            const skipped = Number(res.headers.get("x-annotations-skipped")) || 0;
            status.textContent =
                `Exported ${count(Number(res.headers.get("x-annotations-exported")) || 0, "highlight")} as ${name}.` +
                (skipped ? ` ${count(skipped, "highlight")} couldn't be placed in this copy of the book.` : "") +
                " Put it in the book's .sdr folder on the device (or KOReader's export folder) and open the book.";
        } catch (e) {
            status.textContent = `Couldn't export: ${errorMessage(e)}`;
        }
    });

    byId("ko-import").addEventListener("click", () => fileInput.click());
    fileInput.addEventListener("change", async () => {
        const file = fileInput.files?.[0];
        fileInput.value = ""; // the same file can be chosen again
        if (!file) return;
        status.textContent = "Importing…";
        try {
            const res = await fetch(url, { method: "POST", credentials: "same-origin", body: await file.text() });
            if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
            const r = (await res.json()) as ImportResult;
            await sync();
            const parts = [`Added ${count(r.added, "highlight")}`];
            if (r.updated) parts.push(`updated ${r.updated}`);
            if (r.unchanged) parts.push(`${r.unchanged} already here`);
            let text = `${parts.join(", ")}.`;
            if (r.skipped) text += ` ${count(r.skipped, "highlight")} couldn't be found in this copy of the book.`;
            if (r.bookmarks) text += ` ${count(r.bookmarks, "page bookmark")} left out.`;
            status.textContent = text;
        } catch (e) {
            status.textContent = `Couldn't import: ${errorMessage(e)}`;
        }
    });
}

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
    /** The highlight was made only to carry this note: cancelling takes it away again. */
    let editingIsNew = false;
    function editNote(a: Annotation, isNew = false): void {
        editing = a;
        editingIsNew = isNew;
        noteQuote.textContent = a.text;
        noteQuote.hidden = !a.text;
        noteQuote.style.setProperty("--hl", colorValue(a.color));
        noteText.value = a.note;
        dialog.returnValue = "cancel";
        // Its own history entry (the reader's state, marked), so the
        // browser's Back closes the editor instead of leaving the book.
        history.pushState({ ...(history.state as object | null), noteEditor: true }, "");
        dialog.showModal();
        noteText.focus();
    }
    const inEditorEntry = (): boolean => (history.state as { noteEditor?: boolean } | null)?.noteEditor === true;
    window.addEventListener("popstate", () => {
        if (dialog.open && !inEditorEntry()) dialog.close("cancel");
    });
    dialog.addEventListener("close", () => {
        if (editing && dialog.returnValue === "save") store.update(editing.id, { note: noteText.value.trim() });
        else if (editing && editingIsNew) store.remove(editing.id);
        editing = null;
        if (inEditorEntry()) history.back(); // closed by a button or Escape: drop the entry
    });
    // Ctrl/⌘+Enter saves, as in most note boxes.
    noteText.addEventListener("keydown", (e) => {
        if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) dialog.close("save");
    });

    noteBtn.addEventListener("click", () => {
        const isNew = !!target && !("existing" in target);
        const a = resolve(lastColor());
        hide();
        if (a) editNote(a, isNew);
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

    if (opts.koreader) setupKOReader(opts.koreader);

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
