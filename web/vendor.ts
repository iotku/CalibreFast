// Third-party libraries that scripts load themselves, from static/vendor/
// (templates link the others there directly). Paths name the version, so
// upgrading one means a new directory and updating its paths; TestVendor
// checks they all exist.

import { scriptUrl } from "./trusted.js";

export const JSZIP_URL = "/static/vendor/jszip@3.10.2/dist/jszip.min.js";
export const EPUBJS_URL = "/static/vendor/epubjs@0.3.93/dist/epub.min.js";
const PDFJS_URL = "/static/vendor/pdfjs-dist@5.7.284/build/pdf.min.mjs";
const PDFJS_WORKER_URL = "/static/vendor/pdfjs-dist@5.7.284/build/pdf.worker.min.mjs";
// What pdf.js fetches when a document needs it, a file at a time:
//   wasm/            decoders for JPEG 2000 and JBIG2 images (common in scans)
//   cmaps/           character maps for CJK fonts that don't embed their own
//   standard_fonts/  the 14 standard fonts (Helvetica, Times, …), which PDFs may use without embedding
//   iccs/            a CMYK colour profile
const PDFJS_DATA = {
    wasmUrl: "/static/vendor/pdfjs-dist@5.7.284/wasm/",
    cMapUrl: "/static/vendor/pdfjs-dist@5.7.284/cmaps/",
    cMapPacked: true,
    standardFontDataUrl: "/static/vendor/pdfjs-dist@5.7.284/standard_fonts/",
    iccUrl: "/static/vendor/pdfjs-dist@5.7.284/iccs/",
};

let pdfjs: Promise<PdfJS.Lib> | undefined;

/** pdf.js itself, loaded on first use. */
export function loadPdfJs(): Promise<PdfJS.Lib> {
    pdfjs ??= (import(PDFJS_URL) as Promise<PdfJS.Lib>).then((lib) => {
        // Our own Worker, so its URL goes through the script-URL policy
        // (Trusted Types); pdf.js would otherwise make one from workerSrc.
        lib.GlobalWorkerOptions.workerPort = new Worker(scriptUrl(PDFJS_WORKER_URL), { type: "module" });
        return lib;
    });
    return pdfjs;
}

/**
 * Opens a PDF with pdf.js, from its URL or from the whole file in memory (a
 * copy kept in this browser; pdf.js takes the buffer over). To read only a
 * few pages of a large file, pass { disableAutoFetch: true, disableStream: true }.
 */
export async function openPdf(
    from: string | Uint8Array,
    options: PdfJS.LoadOptions = {},
    onProgress?: (loaded: number, total: number) => void,
): Promise<PdfJS.Document> {
    const lib = await loadPdfJs();
    const src = typeof from === "string" ? { url: from } : { data: from };
    const task = lib.getDocument({ ...options, ...src, ...PDFJS_DATA });
    if (onProgress) task.onProgress = (p) => onProgress(p.loaded, p.total);
    return task.promise;
}

/** The first read of a PDF opened with openPdfRanges, and the size pdf.js asks for after. */
const PDF_CHUNK = 65536;

/**
 * Opens a PDF at url reading only the parts pdf.js asks for, one range
 * request each: a few pages of a large file cost those pages' bytes. pdf.js
 * on its own starts with a request for the whole file and cancels it once
 * it sees ranges work, by which time much of the file may have arrived.
 * Here the first request is a range too, and its answer gives the file's
 * size. A server that ignores ranges sends the whole file, which is then
 * read from memory.
 */
export async function openPdfRanges(url: string, options: PdfJS.LoadOptions = {}): Promise<PdfJS.Document> {
    const range = async (begin: number, end: number): Promise<Response> => {
        const res = await fetch(url, { headers: { Range: `bytes=${begin}-${end - 1}` }, cache: "no-store" });
        if (!res.ok) throw new Error(`${url}: ${res.status} ${res.statusText}`);
        return res;
    };
    const first = await range(0, PDF_CHUNK);
    const head = new Uint8Array(await first.arrayBuffer());
    const total = Number(/\/(\d+)\s*$/.exec(first.headers.get("Content-Range") ?? "")?.[1]);
    if (first.status !== 206 || !Number.isSafeInteger(total) || head.length >= total) return openPdf(head, options); // all of it
    const lib = await loadPdfJs();
    const transport = new lib.PDFDataRangeTransport(total, head);
    const task = lib.getDocument({ ...options, range: transport, rangeChunkSize: PDF_CHUNK, ...PDFJS_DATA });
    transport.requestDataRange = (begin, end) => {
        range(begin, end)
            .then(async (res) => {
                if (res.status !== 206) throw new Error(`${url}: no range requests`);
                transport.onDataRange(begin, new Uint8Array(await res.arrayBuffer()));
            })
            // pdf.js would wait for it for ever: the document fails instead.
            .catch((err: unknown) => {
                console.warn("PDF range:", err);
                void task.destroy();
            });
    };
    return task.promise;
}
