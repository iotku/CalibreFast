// Third-party libraries that scripts load themselves, from static/vendor/
// (templates link the others there directly). Paths name the version, so
// upgrading one means a new directory and updating its paths; TestVendor
// checks they all exist.

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
        lib.GlobalWorkerOptions.workerSrc = PDFJS_WORKER_URL;
        return lib;
    });
    return pdfjs;
}

/**
 * Opens a PDF with pdf.js. To read only a few pages of a large file, pass
 * { disableAutoFetch: true, disableStream: true }.
 */
export async function openPdf(url: string, options: PdfJS.LoadOptions = {}): Promise<PdfJS.Document> {
    const lib = await loadPdfJs();
    return lib.getDocument({ ...options, url, ...PDFJS_DATA }).promise;
}
