// Which book a reader shows, and whether it is a preview.
//
// A preview is the reader opened from the metadata editor (its Read button)
// to look something up: the copyright page, an ISBN, the series. It reads the
// book like any other visit but saves nothing about it: no position (which
// would also mark the book as being read, progress.go), no bookmarks or
// highlights, no copy kept in this browser. Books in the import queue
// (?import=<id>) are always previews: they aren't in the library yet.

import type { ReadingSync } from "./reading-sync.js";

export interface ReaderBook {
    /** The library book's uuid, or a waiting book's id in the import queue. */
    id: string;
    preview: boolean;
    /** The book's file in a format. */
    file(format: string): string;
    /** Its metadata editor, where a preview's Back goes when it has nowhere else. */
    editor: string;
}

/** The book named by the page's address, or null when there is none. */
export function readerBook(): ReaderBook | null {
    const params = new URLSearchParams(window.location.search);
    const importId = params.get("import");
    if (importId) {
        const base = `/import/${encodeURIComponent(importId)}`;
        return { id: importId, preview: true, file: (format) => `${base}/file/${format}`, editor: base };
    }
    const uuid = params.get("uuid");
    if (!uuid) return null;
    const id = encodeURIComponent(uuid);
    return {
        id: uuid,
        preview: !!params.get("preview"),
        file: (format) => `/download/${id}/${format}`,
        editor: `/book/edit/${id}`,
    };
}

/** A ReadingSync that keeps nothing: no saved place, no bookmarks. */
export function previewSync(onStatus: (text: string) => void): ReadingSync {
    onStatus("Preview — nothing is saved");
    return {
        signedIn: false,
        loadProgress: () => Promise.resolve(null),
        saveProgress: () => {},
        loadBookmarks: () => Promise.resolve([]),
        addBookmark: (position, label) => ({ id: "", format: "epub", position: String(position), label, created_at: Date.now() }),
        removeBookmark: () => {},
        bookmarks: () => [],
    };
}

/**
 * The top bar's Back: leaves the reader, skipping its `depth` in-book jumps.
 * A preview opened in a tab of its own closes it, back to the editor.
 */
export function leaveReader(book: ReaderBook, depth: number): void {
    if (!book.preview) {
        history.go(-(depth + 1));
        return;
    }
    window.close();
    // Still here: the browser wouldn't close this tab, or it isn't one of its own.
    setTimeout(() => {
        if (history.length > depth + 1) history.go(-(depth + 1));
        else window.location.href = book.editor;
    }, 150);
}
