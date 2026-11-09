// Third-party libraries that scripts load themselves, from static/vendor/
// (templates link the others there directly). Paths name the version, so
// upgrading one means a new directory and updating its paths; TestVendor
// checks they all exist.

export const JSZIP_URL = "/static/vendor/jszip@3.10.2/dist/jszip.min.js";
export const EPUBJS_URL = "/static/vendor/epubjs@0.3.93/dist/epub.min.js";
const PDFJS_URL = "/static/vendor/pdfjs-dist@5.7.284/build/pdf.min.mjs";
const PDFJS_WORKER_URL = "/static/vendor/pdfjs-dist@5.7.284/build/pdf.worker.min.mjs";
// JPEG 2000 and JBIG2 images (common in scans) need pdf.js's WebAssembly
// decoders, which it fetches from here.
const PDFJS_WASM_URL = "/static/vendor/pdfjs-dist@5.7.284/wasm/";

/** Opens a PDF with pdf.js, which is loaded on first use. */
export async function openPdf(url: string): Promise<PdfJS.Document> {
    const pdfjs = (await import(PDFJS_URL)) as PdfJS.Lib;
    pdfjs.GlobalWorkerOptions.workerSrc = PDFJS_WORKER_URL;
    return pdfjs.getDocument({ url, wasmUrl: PDFJS_WASM_URL }).promise;
}
