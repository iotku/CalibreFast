// Shapes of the JSON the Go server sends and receives. Keep in sync with the
// Go structs named in each comment.

/** A grid entry (Go: Book in main.go). */
export interface Book {
    uuid: string;
    title: string;
    /** Author names, "A & B" (Go: bookAuthorsSQL). */
    authors: string;
    series_index: number;
    pubdate: string;
    path: string;
    last_modified: string;
}

/** Signed-in user (Go: User in auth.go). */
export interface User {
    id: number;
    username: string;
    is_admin: boolean;
    disabled: boolean;
    has_password: boolean;
}

/** GET /api/me (Go: meHandler). */
export interface MeResponse {
    user: User | null;
    can_edit_library?: boolean;
    registration?: boolean;
    /**
     * Books from the watch folder waiting for review. Only sent to those who
     * can review them, when there is a watch folder or something is waiting.
     */
    pending_imports?: number;
}

/** A library value suggestion (Go: groupValuesHandler). */
export interface ValueSuggestion {
    value: string;
    label?: string;
}

/** An identifier as Go sends it (Go: opfIdentifier in opf.go). */
export interface OPFIdentifier {
    Scheme: string;
    Value: string;
    ID: string;
}

/**
 * A metadata source for the editor: the book's current metadata, an uploaded
 * format, or an online lookup (Go: UploadedBookMeta, which embeds
 * OPFMetadata, so its fields are flat).
 */
export interface MetaSource {
    Format: string;
    FileName: string;
    Title: string;
    Creators: string[] | null;
    Description: string;
    Publisher: string;
    Date: string;
    Languages: string[] | null;
    Subjects: string[] | null;
    Identifiers: OPFIdentifier[] | null;
    /** Set by the editor on answers from an online lookup. */
    External?: boolean;
}

/** An ISBN found in the book's files (Go: isbnCandidate in isbn.go). */
export interface IsbnCandidate {
    isbn13: string;
    file: string;
    source: "opf" | "exth" | "xmp" | "text";
    /** Edition named next to it: "ebook", "hardcover", … */
    label?: string;
}

/** GET/POST /metadata/<uuid>/isbns (Go: isbnResponse). */
export interface IsbnResponse {
    candidates: IsbnCandidate[];
    /** The book's PDF, which the editor reads with pdf.js and posts back. */
    pdf?: string;
}

/** Open Library's answer from /metadata/lookup (Go: lookupResult in lookup.go). */
/** One book a lookup found (Go: lookupMatch in lookup.go). */
export interface LookupMatch {
    option: MetaSource;
    /** Same-origin URL of its cover. */
    cover?: string;
    /** One of its editions has the ISBN asked about (match_isbn). */
    hasIsbn?: boolean;
}

/** A source's answer (Go: lookupResult in lookup.go). */
export interface LookupResult {
    source: string;
    /** Best first; none: no match, or an error. */
    matches?: LookupMatch[];
    error?: string;
}

/** A cover from a lookup, for the cover dialog (cf:cover-candidates event). */
export interface CoverCandidate {
    source: string;
    url: string;
}

/** One file of a book waiting in the import queue (Go: importFile in imports.go). */
export interface ImportFile {
    name: string;
    format: string;
    size: number;
}

/** A book waiting in the import queue (Go: importSummary). */
export interface ImportSummary {
    id: string;
    title: string;
    authors: string[] | null;
    files: ImportFile[];
    /** The folder inside the watch folder it came from; "" for its top. */
    from: string;
    added: string;
    has_cover: boolean;
}

/** GET /api/imports (Go: importQueue). */
export interface ImportQueue {
    watching: boolean;
    folder?: string;
    /** The folder is checked on a timer: the system doesn't report changes to it. */
    polling?: boolean;
    /** Files in the watch folder not taken yet (still being copied, or just seen). */
    settling: number;
    problem?: string;
    items: ImportSummary[];
}

/** A book in the Recycling Bin (Go: recycleBookView in recycle.go). */
export interface RecycledBook {
    id: number;
    uuid: string;
    title: string;
    /** "A & B" */
    authors: string;
    path: string;
    formats: string[];
    size: number;
    no_files: boolean;
    /** Readers' rows about it kept with the batch, by table (bookmarks, annotations, …). */
    reader_data?: Record<string, number>;
    restored_at?: string;
    restored_by?: string;
    /** Still in the bin: it can be restored. */
    in_bin: boolean;
}

/** One removal (Go: recycleBatchView). */
export interface RecycleBatch {
    id: string;
    deleted_at: string;
    deleted_by: string;
    books: RecycledBook[];
}

/** GET /api/recycle-bin (Go: recycleBinHandler). */
export interface RecycleBin {
    folder: string;
    batches: RecycleBatch[];
}

/** POST /api/recycle-bin/{batch}/restore (Go: restoreResult). */
export interface RestoreResponse {
    results: { uuid: string; title: string; error?: string }[];
}
