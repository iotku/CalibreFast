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
        user: null,
        loadProgress: () => Promise.resolve(null),
        saveProgress: () => {},
        loadBookmarks: () => Promise.resolve([]),
        addBookmark: (position, label) => ({ id: "", format: "epub", position: String(position), label, created_at: Date.now() }),
        removeBookmark: () => {},
        bookmarks: () => [],
    };
}

/** Whether a page of the library led here (a link, not an address typed or bookmarked). */
function fromLibraryPage(): boolean {
    if (!document.referrer) return false;
    try {
        return new URL(document.referrer).origin === location.origin;
    } catch {
        return false;
    }
}

/**
 * Whether the browser's Back, past `depth` in-book jumps, returns to a page
 * of the library. A reader opened in a new tab, from a bookmark or from
 * another site has nothing of ours behind it: Back would do nothing, or
 * leave the library.
 */
const canGoBack = (depth: number): boolean => history.length > depth + 1 && fromLibraryPage();

/**
 * The top bar's Back: leaves the reader, skipping its `depth` in-book jumps,
 * or goes to the book's page when there is nothing to go back to.
 * A preview closes its tab (or split view, or window) instead: it was opened
 * beside the editor, which is still there. If the browser won't close it,
 * `onStuck` says so; a preview opened on its own (no page of ours led to it)
 * goes to the editor instead.
 */
export function leaveReader(book: ReaderBook, depth: number, onStuck: (text: string) => void): void {
    if (!book.preview) {
        if (canGoBack(depth)) history.go(-(depth + 1));
        else location.assign(book.id ? `/book/get/${encodeURIComponent(book.id)}` : "/");
        return;
    }
    window.close();
    // Still here: the browser keeps tabs it didn't let a script open.
    setTimeout(() => {
        if (fromLibraryPage() || window.opener) onStuck("Preview — close this tab when you're done");
        else location.assign(book.editor);
    }, 150);
}

/**
 * In-book jumps in a preview don't add history entries: a tab with more than
 * one may not be closed by a script, and Back has to close it.
 */
export const keepsHistory = (book: ReaderBook): boolean => !book.preview;
