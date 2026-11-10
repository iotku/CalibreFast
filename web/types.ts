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
export interface LookupResult {
    source: string;
    option?: MetaSource;
    /** Same-origin URL of its cover. */
    cover?: string;
    error?: string;
}

/** A cover from a lookup, for the cover dialog (cf:cover-candidates event). */
export interface CoverCandidate {
    source: string;
    url: string;
}
