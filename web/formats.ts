/**
 * The formats the web readers open. EPUB and PDF are read as they are;
 * Kindle formats (AZW3, AZW, MOBI) are converted to EPUB by the server
 * (/book/as-epub/<uuid>/<format>, convert_read.go) and read in the EPUB
 * reader, with their places saved under their own format.
 */

export type ConvertedFormat = "azw3" | "azw" | "mobi";
export type BookFormat = "epub" | "pdf" | ConvertedFormat;

/** In the order one is picked for a book with several (as convertedFormats in Go). */
export const CONVERTED_FORMATS: readonly ConvertedFormat[] = ["azw3", "azw", "mobi"];

export function isConvertedFormat(f: string | null | undefined): f is ConvertedFormat {
    return (CONVERTED_FORMATS as readonly string[]).includes(f ?? "");
}

/** The reader for a book in a format (as readerLink in Go). */
export function readerUrl(uuid: string, format: BookFormat): string {
    const id = encodeURIComponent(uuid);
    if (format === "pdf") return `/read-pdf?uuid=${id}`;
    if (isConvertedFormat(format)) return `/read?uuid=${id}&format=${format}`;
    return `/read?uuid=${id}`;
}

/** The format a reader's address opens ("/read?uuid=…&format=azw3"), or null. */
export function readerFormat(url: string): BookFormat | null {
    let u: URL;
    try {
        u = new URL(url, location.origin);
    } catch {
        return null;
    }
    if (u.pathname === "/read-pdf") return "pdf";
    if (u.pathname !== "/read") return null;
    const f = u.searchParams.get("format");
    return isConvertedFormat(f) ? f : "epub";
}
