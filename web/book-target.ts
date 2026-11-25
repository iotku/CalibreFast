// What the metadata editor is editing, and where its requests go: a book in
// the library, or one from the watch folder that is waiting in the import
// queue (Go: imports.go). The editor, its ISBN tools and its cover dialog are
// the same for both; only these addresses differ.

export interface BookTarget {
    /** The library book's uuid, or the waiting book's id in the queue. */
    id: string;
    /** Waiting in the import queue: not in the library yet. */
    pending: boolean;
    /** Merging several library books into this one (Go: merge.go). */
    merge?: boolean;
    /** Adding new files to this library book (Go: add_format.go); merge is set too. */
    adding?: boolean;
    /** More to send with the form's values when applying (a merge's choices). */
    extra?: () => Record<string, unknown>;
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
    /** The book's Kindle file (AZW3, AZW, MOBI) converted to EPUB (Go: convert_read.go). */
    asEpub(format: string): string;
    /** GET the cover as it is now. */
    cover: string;
    /** POST a new cover. */
    saveCover: string;
    /** POST: bring back the cover before the last change. */
    revertCover: string;
    /** GET: whether there is a cover, and one to revert to. */
    coverStatus: string;
    /** A waiting book whose metadata is offered as sources too (Go: GetOptionsForBook's ?import=). */
    importSource?: string;
    /** New files that were a book of their own in the import queue: back there once added. */
    fromQueue?: boolean;
}

/** localStorage key for the import review's edits, offered in a library book's editor. */
export const importEditsKey = (importId: string): string => `cf-import-edits:${importId}`;

export function libraryBook(uuid: string, importSource?: string | null): BookTarget {
    const id = encodeURIComponent(uuid);
    return {
        id: uuid,
        pending: false,
        ...(importSource ? { importSource } : {}),
        metadata: `/metadata/${id}${importSource ? `?import=${encodeURIComponent(importSource)}` : ""}`,
        apply: `/metadata/${id}/apply`,
        isbns: `/metadata/${id}/isbns`,
        formats: `/formats/${id}`,
        file: (format) => `/download/${id}/${format}`,
        asEpub: (format) => `/book/as-epub/${id}/${encodeURIComponent(format)}`,
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
        asEpub: (format) => `${base}/as-epub/${encodeURIComponent(format)}`,
        cover: `${base}/cover`,
        saveCover: `${base}/cover`,
        revertCover: `${base}/cover/revert`,
        coverStatus: `${base}/cover/status`,
    };
}

/**
 * Several library books being merged into one (Go: merge.go): the editor
 * edits the merged book's metadata, its sources are every book's, and
 * applying merges them. Everything else is the kept book's. placeholder: a
 * book not in the library yet that the kept book replaces (Go:
 * merge_placeholder.go); then ids may be the kept book alone.
 */
export function mergeBooks(keep: string, ids: string[], extra: () => Record<string, unknown>, placeholder?: string): BookTarget {
    let q = `ids=${ids.map(encodeURIComponent).join(",")}&keep=${encodeURIComponent(keep)}`;
    if (placeholder) q += `&placeholder=${encodeURIComponent(placeholder)}`;
    return {
        ...libraryBook(keep),
        merge: true,
        metadata: `/merge/metadata?${q}`,
        apply: "/merge/apply",
        extra,
    };
}

/**
 * New files being added to a library book (Go: add_format.go): the merge
 * editor, with the uploaded files (waiting in the import queue as importId)
 * as the other book. fromQueue: they were a book of their own there, a
 * possible duplicate of this one; the review's edits are offered too.
 */
export function addFiles(uuid: string, importId: string, extra: () => Record<string, unknown>, fromQueue = false): BookTarget {
    const base = `/book/add-format/${encodeURIComponent(uuid)}/${encodeURIComponent(importId)}`;
    return {
        ...libraryBook(uuid),
        ...(fromQueue ? { fromQueue, importSource: importId } : {}),
        merge: true,
        adding: true,
        metadata: `${base}/metadata`,
        apply: `${base}/apply`,
        extra,
    };
}
