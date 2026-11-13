// Shapes of the JSON the Go server sends and receives. Keep in sync with the
// Go structs named in each comment.

import type { StoredMarks } from "./reading.js";

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
    /** The user's reading marks, when asked for (?reading=1), as /api/me/reading has them. */
    reading?: StoredMarks;
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
    /** The series and the book's number in it (Go: OPFMetadata.Series). */
    Series?: string;
    SeriesIndex?: string;
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

/** A library book that looks like one being added (Go: duplicateBook in duplicates.go). */
export interface DuplicateBook {
    uuid: string;
    title: string;
    authors: string[] | null;
    /** Same title and authors. */
    same_authors: boolean;
    /** It holds a file identical to one being added: that can't be added again. */
    same_file: boolean;
}

/** Library books a book may duplicate (Go: duplicateCheck). */
export interface DuplicateCheck {
    books: DuplicateBook[];
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
    /** Library books it may duplicate; absent for none. */
    duplicate?: DuplicateCheck;
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
    /** The books a merge took into another (Go: mergeNote in merge.go). */
    merged_into?: MergeNote;
}

/** Go: mergeNote. replaced: the kept book's files the merge replaced, in the batch's replaced/ folder. */
export interface MergeNote {
    uuid: string;
    title: string;
    replaced?: string[];
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

/** A row of the bulk editor, GET /api/library/table (Go: tableBook in bulk_edit.go). */
export interface TableBook {
    id: number;
    uuid: string;
    title: string;
    authors: string;
    series: string;
    series_index: number;
    publisher: string;
    /** YYYY-MM-DD, "" for none. */
    pubdate: string;
    added: string;
    tags: string[];
    languages: string[];
    formats: string[];
    /** metadata.db lists no files: formats are what's in the book's folder (Go: FormatsFromFolder). */
    formats_from_folder?: boolean;
    isbn: string;
    has_cover: boolean;
}

/** What to change in each book (Go: bulkEdit). Fields left out stay as they are. */
export interface BulkEdit {
    title?: string;
    authors?: string;
    publisher?: string;
    date?: string;
    /** The series ("" for none); without series_index each book keeps its number (1 if it had no series). */
    series?: string;
    series_index?: string;
    tags?: string;
    add_tags?: string[];
    remove_tags?: string[];
    languages?: string;
    add_languages?: string[];
    remove_languages?: string[];
}

/** POST /api/library/bulk (Go: bulkResult). */
export interface BulkResult {
    changed: number;
    unchanged: number;
    failed: { uuid: string; error: string }[];
    /** The changed books' rows, as they are now. */
    books: TableBook[];
}

/** Books marked as not duplicates of each other (Go: notDuplicateSet in not_duplicates.go). */
export interface NotDuplicateSet {
    id: number;
    uuids: string[];
}

/** POST /api/library/repair-formats (Go: unlistedRepair in calibre_names.go). */
export interface FormatRepair {
    books: number;
    formats: number;
    skipped: { uuid: string; error: string }[];
}

/** GET /api/admin/repairs: what the Library repairs would change (Go: repairCheck in library_repairs.go). */
export interface RepairCheck {
    /** Formats whose data.name isn't their file's name. */
    misnamed: { uuid: string; title: string; format: string; from: string; to: string }[];
    /** Books with no data rows whose folders hold book files. */
    unlisted: { uuid: string; title: string; formats: string[]; ambiguous: string[] }[];
}
