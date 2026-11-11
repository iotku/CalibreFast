// What the metadata editor is editing, and where its requests go: a book in
// the library, or one from the watch folder that is waiting in the import
// queue (Go: imports.go). The editor, its ISBN tools and its cover dialog are
// the same for both; only these addresses differ.

export interface BookTarget {
    /** The library book's uuid, or the waiting book's id in the queue. */
    id: string;
    /** Waiting in the import queue: not in the library yet. */
    pending: boolean;
    /** GET: the metadata sources ("current", then each file's). */
    metadata: string;
    /** POST the form's values: saves them, or adds a waiting book to the library. */
    apply: string;
    /** GET/POST: ISBNs found in the book's files. */
    isbns: string;
    /** GET: the book's formats ("epub", "pdf", …). */
    formats: string;
    /** The book's file in a format. */
    file(format: string): string;
    /** GET the cover as it is now. */
    cover: string;
    /** POST a new cover. */
    saveCover: string;
    /** POST: bring back the cover before the last change. */
    revertCover: string;
    /** GET: whether there is a cover, and one to revert to. */
    coverStatus: string;
}

export function libraryBook(uuid: string): BookTarget {
    const id = encodeURIComponent(uuid);
    return {
        id: uuid,
        pending: false,
        metadata: `/metadata/${id}`,
        apply: `/metadata/${id}/apply`,
        isbns: `/metadata/${id}/isbns`,
        formats: `/formats/${id}`,
        file: (format) => `/download/${id}/${format}`,
        cover: `/cover/${id}`,
        saveCover: `/upload-cover/${id}`,
        revertCover: `/revert-cover/${id}`,
        coverStatus: `/cover-status/${id}`,
    };
}

export function pendingImport(importId: string): BookTarget {
    const base = `/import/${encodeURIComponent(importId)}`;
    return {
        id: importId,
        pending: true,
        metadata: `${base}/metadata`,
        apply: `${base}/apply`,
        isbns: `${base}/isbns`,
        formats: `${base}/formats`,
        file: (format) => `${base}/file/${format}`,
        cover: `${base}/cover`,
        saveCover: `${base}/cover`,
        revertCover: `${base}/cover/revert`,
        coverStatus: `${base}/cover/status`,
    };
}
