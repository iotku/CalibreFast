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
    /**
     * A shelf's whole-series entry (Go: seriesCard in shelf_series.go): its
     * uuid is the "series:<name>" key, its title the series, and its cover,
     * path, authors and dates are the first book's.
     */
    series_card?: SeriesCard;
    /**
     * A book not in the library yet (Go: placeholderCard in
     * placeholders.go): its uuid is the placeholder's.
     */
    placeholder?: PlaceholderCard;
}

/** A placeholder book's card (Go: placeholderCard). */
export interface PlaceholderCard {
    /** Its page. */
    href: string;
    /** Its picture; absent for none. */
    cover?: string;
}

/** A series on a shelf, shown as one card (Go: seriesCard). */
export interface SeriesCard {
    name: string;
    /** Books in the series. */
    count: number;
    /** The first book's uuid: the card's cover. */
    cover: string;
    /** The series page. */
    href: string;
}

/** Signed-in user (Go: User in auth.go). */
export interface User {
    id: number;
    username: string;
    is_admin: boolean;
    is_editor: boolean;
    disabled: boolean;
    has_password: boolean;
}

/** GET /api/me (Go: meHandler). */
export interface MeResponse {
    user: User | null;
    can_edit_library?: boolean;
    can_admin_library?: boolean;
    registration?: boolean;
    /** Signed out only: whether visitors who aren't signed in may read here (-public-read). */
    guest_reading?: boolean;
    /**
     * Books from the watch folder waiting for review. Only sent to those who
     * can review them, when there is a watch folder or something is waiting.
     */
    pending_imports?: number;
    /** The user's reading marks, when asked for (?reading=1), as /api/me/reading has them. */
    reading?: StoredMarks;
    /** The tag of the user's marks (listtag.go), with ?reading=1. */
    reading_tag?: string;
    /** The marks are those the browser has (?have=): not sent again. */
    reading_same?: boolean;
    /** The account's preferences (Go: userPrefs; prefs.ts). */
    prefs?: { format: "last" | "epub" | "pdf"; track_reading?: boolean; daily_pages?: number; yearly_books?: number; goal_notice?: boolean };
}

/** A library value suggestion (Go: groupValuesHandler). */
export interface ValueSuggestion {
    value: string;
    label?: string;
    /** Books with this value. */
    count?: number;
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
    /** It has one of the ISBNs asked about (new files for a book). */
    same_isbn?: boolean;
    /** Which of the checked book's files (by name) are identical to one of its. */
    same_files?: string[];
    /** Its formats, upper case ("EPUB"). */
    formats?: string[];
}

/** Library books a book may duplicate (Go: duplicateCheck). */
export interface DuplicateCheck {
    books: DuplicateBook[];
}

/** POST /api/imports/discard-existing (Go: discardExistingResult). */
export interface DiscardExistingResult {
    /** Books discarded whole. */
    discarded: number;
    /** Books that lost only some of their files. */
    split: number;
    /** Files discarded, in all. */
    files: number;
    errors?: string[];
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
    /** Not checked for duplicates yet (it runs in the background): a later poll has the result. */
    checking?: boolean;
    /** New files for this library book (Go: add_format.go), reviewed at link. */
    into?: string;
    into_title?: string;
    link?: string;
}

/** GET /api/imports (Go: importQueue). */
export interface ImportQueue {
    watching: boolean;
    folder?: string;
    /** The folder is checked on a timer: the system doesn't report changes to it. */
    polling?: boolean;
    /** Files in the watch folder not taken yet (still being copied, or just seen). */
    settling: number;
    /** Books being taken into the queue right now. */
    staging: number;
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
    /** Single files taken from books still in the library (Go: recycle_formats.go). */
    formats: RecycledFormat[];
    /** The books a merge took into another (Go: mergeNote in merge.go). */
    merged_into?: MergeNote;
}

/** One file of a book in the Recycling Bin (Go: recycleFormatView). */
export interface RecycledFormat {
    uuid: string;
    title: string;
    authors: string;
    /** "EPUB" */
    format: string;
    file: string;
    size: number;
    /** A merge or a new file took its place (else it was removed). */
    replaced?: boolean;
    reader_data?: Record<string, number>;
    restored_at?: string;
    restored_by?: string;
    /** What POST …/restore takes in formats. */
    key: string;
    in_bin: boolean;
}

/**
 * Go: mergeNote. added_files: the batch is from adding new files to the book
 * (they replaced its files in it). replaced: older batches' replaced files, in
 * their replaced/ folder.
 */
export interface MergeNote {
    uuid: string;
    title: string;
    added_files?: string[];
    replaced?: string[];
}

/** GET /api/recycle-bin (Go: recycleBinHandler). */
export interface RecycleBin {
    folder: string;
    batches: RecycleBatch[];
}

/** POST /api/recycle-bin/{batch}/restore (Go: restoreResult). */
export interface RestoreResponse {
    results: { uuid: string; title: string; format?: string; error?: string }[];
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
    /** A waiting book only (Go: tableBook.Library): "checking", "same file", "in library" or "new". */
    library?: string;
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
    /** Each tag holding this separator (";") becomes its trimmed parts, without ones the book has. */
    split_tags?: string;
    languages?: string;
    add_languages?: string[];
    remove_languages?: string[];
}

/** POST /api/library/bulk (Go: bulkResult). */
export interface BulkResult {
    changed: number;
    unchanged: number;
    failed: { uuid: string; error: string }[];
    /** The changed books' rows, as they are now (or, queued, as they will be). */
    books: TableBook[];
    /** The library's edits are queued (Go: bulk_queue.go): this one's id, to follow it. */
    job?: number;
}

/** GET /api/library/bulk/failures: a book a queued edit couldn't be made to (Go: keptBulkFailure). */
export interface BulkFailureKept {
    id: number;
    uuid: string;
    title: string;
    edit: BulkEdit;
    error: string;
    by: string;
    /** Unix seconds. */
    at: number;
}

/** GET /api/library/bulk/jobs: how far a queued edit has got (Go: bulkJob). */
export interface BulkJob {
    state: "queued" | "running" | "done" | "unknown";
    done: number;
    total: number;
    /** When done: what was written, the rows as they are now. */
    result?: BulkResult;
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

/** An entry of /api/<plural>: a value, or a group of values (Go: AggregateItem in aggregate.go). */
export interface AggregateEntry {
    name: string;
    count: number;
    slug: string;
    /** Human label when it differs from name (languages). */
    display?: string;
    /** Set when name is a group. */
    members?: string[];
}

/** PUT /api/groups/{cat}/{value} (Go: groupEdit in aggregate.go), as the "data" field when a picture is sent. */
export interface GroupEdit {
    name?: string;
    /** Absent: the grouping is left as it is. Empty: the group is dissolved. */
    members?: string[];
    /** Absent: left as it is. */
    description?: string;
    removeImage?: boolean;
}

/** PUT /api/groups/{cat}/{value} response (Go: groupPutHandler). */
export interface GroupSaved {
    url: string;
}

/** PUT/DELETE /api/aggregates/{cat}/featured/{value} (Go: featuredHandler). */
export interface FeaturedResponse {
    featured: boolean;
    name: string;
}

/** The day's use of Hardcover's plan (Go: HardcoverUsage in hardcover_http.go). */
export interface HardcoverUsage {
    used: number;
    limit: number;
    resetsIn: string;
    paused?: string;
}

/** A Hardcover series offered for a library series (Go: hcCandidate). */
export interface HardcoverCandidate {
    id: number;
    name: string;
    slug?: string;
    author?: string;
    booksCount: number;
    primary?: number;
    hits?: number;
    url?: string;
    /** Same-named candidates share it (Hardcover splits some series by author). */
    group: string;
    /** In the likeliest group: ticked. */
    pick?: boolean;
    sure?: boolean;
    evidence?: "isbn" | "author" | "title";
}

/** A queued request to Hardcover (Go: hcJob in hardcover.go). */
export interface HardcoverJob {
    id: number;
    kind: "candidates" | "link" | "refresh" | "batch";
    series: string;
    batch?: string;
    state: "queued" | "held" | "running" | "done" | "failed";
    notBefore?: number;
    error?: string;
    /** candidates: {candidates}; link, refresh, batch: hcLinkResult. */
    result?: {
        candidates?: HardcoverCandidate[];
        linked?: boolean;
        hcName?: string;
        books?: number;
        parts?: number;
        review?: string;
        needsChoice?: boolean;
    };
}

/** POST /api/hardcover/{candidates,link,refresh} (Go: hcJobAnswer). */
export interface HardcoverJobAnswer {
    job: HardcoverJob;
    until?: string;
    usage: HardcoverUsage;
}
