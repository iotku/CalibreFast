/**
 * Files out of an .epub, read without downloading all of it first.
 *
 * The .epub stays a zip on the server (/download/<uuid>/epub, served as an
 * attachment) and is unzipped here: its central directory comes from the end
 * of the file with an HTTP Range request, then each file the reader asks for
 * (container, package, the chapter on screen and its images and styles) is
 * one more Range request, inflated with the browser's DecompressionStream.
 * Once the whole file has arrived (useBuffer), every read is local.
 *
 * Nothing from inside the book is ever served by our origin as a page: the
 * chapters still reach the reader only through epub.js's sandboxed,
 * script-free iframe, as before.
 */

const EOCD_SIG = 0x06054b50;
const CEN_SIG = 0x02014b50;
const LOC_SIG = 0x04034b50;
/** The end of central directory record (22 bytes) plus the longest comment. */
const TAIL_BYTES = 22 + 0xffff;
/** No single file in a book may inflate to more than this (zip bombs). */
const MAX_ENTRY = 64 << 20;
/** Local header slack fetched with an entry: its name and extra field. */
const LOCAL_SLACK = 512;

interface Entry {
    offset: number; // of the local file header
    compressed: number;
    size: number;
    method: number; // 0 stored, 8 deflate
}

export interface EpubSource {
    /** Bytes in the .epub. */
    readonly size: number;
    /** The file's Last-Modified, for revalidating a cached copy. */
    readonly lastModified: string;
    /** A file in the book by its path from the root ("OEBPS/ch1.xhtml"). */
    read(path: string): Promise<Uint8Array>;
    /** Whether the book has the file. */
    has(path: string): boolean;
    /** From now on, read from the whole file (in memory). */
    useBuffer(buf: Uint8Array): void;
    /** Resolves with the whole file once useBuffer has been called. */
    readonly complete: Promise<Uint8Array>;
    readonly whole: boolean;
}

/** The browser can inflate zip entries itself (needed for everything here). */
export const canInflate = typeof DecompressionStream !== "undefined";

class Source implements EpubSource {
    private buf: Uint8Array | null = null;
    private readonly reads = new Map<string, Promise<Uint8Array>>();
    private resolveComplete!: (b: Uint8Array) => void;
    readonly complete = new Promise<Uint8Array>((r) => (this.resolveComplete = r));

    constructor(
        private readonly url: string,
        private readonly entries: Map<string, Entry>,
        readonly size: number,
        readonly lastModified: string,
    ) {}

    get whole(): boolean {
        return this.buf !== null;
    }

    has(path: string): boolean {
        return this.entries.has(clean(path));
    }

    useBuffer(buf: Uint8Array): void {
        if (this.buf) return;
        this.buf = buf;
        this.resolveComplete(buf);
    }

    read(path: string): Promise<Uint8Array> {
        const name = clean(path);
        let p = this.reads.get(name);
        if (!p) {
            p = this.load(name);
            this.reads.set(name, p);
            // Keep only reads in flight: done ones are cheap to repeat from
            // the buffer, and epub.js keeps what it needs itself.
            p.finally(() => this.reads.delete(name)).catch(() => {});
        }
        return p;
    }

    private async load(name: string): Promise<Uint8Array> {
        const e = this.entries.get(name);
        if (!e) throw new Error(`Not in the book: ${name}`);
        if (e.size > MAX_ENTRY || e.compressed > MAX_ENTRY) throw new Error(`Too large: ${name}`);
        let raw: Uint8Array;
        if (this.buf) {
            raw = this.buf.subarray(e.offset, e.offset + 30 + LOCAL_SLACK + e.compressed);
        } else {
            raw = await rangeOf(this.url, e.offset, e.offset + 30 + LOCAL_SLACK + e.compressed, this.lastModified);
        }
        const v = view(raw);
        if (raw.length < 30 || v.getUint32(0, true) !== LOC_SIG) throw new Error(`Bad zip entry: ${name}`);
        const start = 30 + v.getUint16(26, true) + v.getUint16(28, true);
        if (start + e.compressed > raw.length) {
            // A long extra field: fetch the rest.
            const more = this.buf
                ? this.buf.subarray(e.offset + raw.length, e.offset + start + e.compressed)
                : await rangeOf(this.url, e.offset + raw.length, e.offset + start + e.compressed, this.lastModified);
            raw = concat(raw, more);
        }
        const data = raw.subarray(start, start + e.compressed);
        if (e.method === 0) return data;
        if (e.method === 8) return inflate(data, e.size);
        throw new Error(`Unsupported compression in ${name}`);
    }
}

/**
 * Open the .epub at url for reading file by file. Throws when the server or
 * the file can't do that (no Range support, ZIP64, not a zip), so the caller
 * downloads it whole instead.
 */
export async function openRemote(url: string): Promise<EpubSource> {
    if (!canInflate) throw new Error("DecompressionStream unsupported");
    const res = await fetch(url, { credentials: "same-origin", headers: { Range: `bytes=-${TAIL_BYTES}` } });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const lastModified = res.headers.get("last-modified") ?? "";
    if (res.status !== 206) {
        // The whole file came back: use it.
        const buf = new Uint8Array(await res.arrayBuffer());
        return openBuffer(buf, lastModified);
    }
    const total = Number(/\/(\d+)$/.exec(res.headers.get("content-range") ?? "")?.[1]);
    if (!total) throw new Error("no Content-Range");
    const tail = new Uint8Array(await res.arrayBuffer());
    const tailStart = total - tail.length;
    const { cdOffset, cdSize } = endRecord(tail);
    const cd =
        cdOffset >= tailStart
            ? tail.subarray(cdOffset - tailStart, cdOffset - tailStart + cdSize)
            : await rangeOf(url, cdOffset, cdOffset + cdSize, lastModified);
    return new Source(url, centralDirectory(cd), total, lastModified);
}

/** Read a whole .epub already in memory (a cached copy). */
export function openBuffer(buf: Uint8Array, lastModified: string): EpubSource {
    const { cdOffset, cdSize } = endRecord(buf.subarray(Math.max(0, buf.length - TAIL_BYTES)));
    const s = new Source("", centralDirectory(buf.subarray(cdOffset, cdOffset + cdSize)), buf.length, lastModified);
    s.useBuffer(buf);
    return s;
}

// ---- zip structures ----------------------------------------------------------

function endRecord(tail: Uint8Array): { cdOffset: number; cdSize: number } {
    const v = view(tail);
    for (let i = tail.length - 22; i >= 0; i--) {
        if (v.getUint32(i, true) !== EOCD_SIG) continue;
        const count = v.getUint16(i + 10, true);
        const cdSize = v.getUint32(i + 12, true);
        const cdOffset = v.getUint32(i + 16, true);
        if (count === 0xffff || cdSize === 0xffffffff || cdOffset === 0xffffffff) throw new Error("ZIP64 not supported");
        return { cdOffset, cdSize };
    }
    throw new Error("not a zip file");
}

function centralDirectory(cd: Uint8Array): Map<string, Entry> {
    const v = view(cd);
    const utf8 = new TextDecoder();
    const out = new Map<string, Entry>();
    let at = 0;
    while (at + 46 <= cd.length && v.getUint32(at, true) === CEN_SIG) {
        const nameLen = v.getUint16(at + 28, true);
        const extraLen = v.getUint16(at + 30, true);
        const commentLen = v.getUint16(at + 32, true);
        const name = utf8.decode(cd.subarray(at + 46, at + 46 + nameLen));
        if (!name.endsWith("/")) {
            out.set(name, {
                method: v.getUint16(at + 10, true),
                compressed: v.getUint32(at + 20, true),
                size: v.getUint32(at + 24, true),
                offset: v.getUint32(at + 42, true),
            });
        }
        at += 46 + nameLen + extraLen + commentLen;
    }
    if (!out.size) throw new Error("empty zip");
    return out;
}

/** Raw deflate, refusing output past the stated size (or MAX_ENTRY). */
async function inflate(data: Uint8Array, size: number): Promise<Uint8Array> {
    const limit = Math.min(MAX_ENTRY, size);
    const stream = new Blob([data as BlobPart]).stream().pipeThrough(new DecompressionStream("deflate-raw"));
    const reader = stream.getReader();
    const out = new Uint8Array(limit);
    let got = 0;
    for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        if (got + value.length > limit) {
            await reader.cancel();
            throw new Error("entry larger than stated");
        }
        out.set(value, got);
        got += value.length;
    }
    return got === limit ? out : out.subarray(0, got);
}

// ---- HTTP --------------------------------------------------------------------

/**
 * Bytes [start, end) of the file. If-Range makes the server send the whole
 * (changed) file instead of a range of a different one: that is an error
 * here, and the request is abandoned before its body.
 */
async function rangeOf(url: string, start: number, end: number, lastModified: string): Promise<Uint8Array> {
    const headers: Record<string, string> = { Range: `bytes=${start}-${end - 1}` };
    if (lastModified) headers["If-Range"] = lastModified;
    const ctrl = new AbortController();
    const res = await fetch(url, { credentials: "same-origin", headers, signal: ctrl.signal });
    if (res.status !== 206) {
        ctrl.abort();
        throw new Error(res.ok ? "the book file changed" : `HTTP ${res.status}`);
    }
    return new Uint8Array(await res.arrayBuffer());
}

// ---- helpers -----------------------------------------------------------------

function clean(path: string): string {
    return path.replace(/^\/+/, "");
}

function view(b: Uint8Array): DataView {
    return new DataView(b.buffer, b.byteOffset, b.byteLength);
}

function concat(a: Uint8Array, b: Uint8Array): Uint8Array {
    const out = new Uint8Array(a.length + b.length);
    out.set(a);
    out.set(b, a.length);
    return out;
}

/** Media type for a file in a book, by extension. */
export function mediaType(path: string): string {
    const ext = path.slice(path.lastIndexOf(".") + 1).toLowerCase();
    const types: Record<string, string> = {
        xhtml: "application/xhtml+xml",
        html: "text/html",
        htm: "text/html",
        xml: "application/xml",
        opf: "application/oebps-package+xml",
        ncx: "application/x-dtbncx+xml",
        css: "text/css",
        jpg: "image/jpeg",
        jpeg: "image/jpeg",
        png: "image/png",
        gif: "image/gif",
        webp: "image/webp",
        avif: "image/avif",
        svg: "image/svg+xml",
        ttf: "font/ttf",
        otf: "font/otf",
        woff: "font/woff",
        woff2: "font/woff2",
        mp3: "audio/mpeg",
        m4a: "audio/mp4",
        mp4: "video/mp4",
        js: "text/plain", // never run
    };
    return types[ext] ?? "application/octet-stream";
}
