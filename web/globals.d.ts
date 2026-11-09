// Types for the third-party libraries in static/vendor/ (htmx, epub.js,
// pdf.js, DOMPurify). Only the parts CalibreFast uses are described — extend as needed.

// ---- htmx (global, loaded by <script> on library pages) --------------------
interface HtmxApi {
    /** source: the element the request is for (its hx-preload response is used). */
    ajax(method: string, url: string, options: { target: string; swap?: string; source?: Element }): Promise<void>;
    process(element: Element): void;
}
declare const htmx: HtmxApi | undefined;

// ---- epub.js (global ePub, loaded by <script>) -----------------------------
declare namespace EpubJS {
    interface NavItem {
        label: string;
        href: string;
        subitems?: NavItem[];
    }
    interface Location {
        start: { cfi: string; percentage?: number; displayed?: { page: number; total: number } };
        end: { cfi: string };
        atStart?: boolean;
        atEnd?: boolean;
    }
    interface Section {
        href: string;
        /** Absolute path inside the book, e.g. "/OEBPS/Text/ch1.xhtml". */
        url: string;
        load(request: (url: string) => Promise<unknown>): Promise<Element>;
    }
    interface Spine {
        length?: number;
        get(target: number | string): Section | null;
    }
    interface View {
        document: Document;
    }
    interface Rendition {
        display(target?: string): Promise<void>;
        next(): Promise<void>;
        prev(): Promise<void>;
        on(event: "relocated", cb: (location: Location) => void): void;
        on(event: "rendered", cb: (section: Section, view: View) => void): void;
        themes: { fontSize(size: string): void };
    }
    interface Book {
        ready: Promise<void>;
        spine: Spine;
        navigation: { toc: NavItem[] };
        locations: {
            generate(chars: number): Promise<string[]>;
            percentageFromCfi(cfi: string): number;
            locationFromCfi(cfi: string): number;
            length(): number;
        };
        /** Print page numbers, when the book has a page list. */
        pageList?: { pages: string[]; lastPage: number; pageFromCfi(cfi: string): number };
        renderTo(element: string | Element, options: { width: string; height: string; flow: string }): Rendition;
        coverUrl(): Promise<string | null>;
        load(path: string): Promise<unknown>;
        /** Only when opened from a whole .epub (ArrayBuffer). */
        archive: { createUrl(path: string, options?: { base64?: boolean }): Promise<string> };
    }
}
/** Opening from an ArrayBuffer of the whole .epub needs the JSZip global. */
declare function ePub(source: string | ArrayBuffer): EpubJS.Book;
interface Window {
    ePub?: typeof ePub;
}

// ---- pdf.js (ES module imported by web/vendor.ts) --------------------------
declare namespace PdfJS {
    interface Viewport {
        width: number;
        height: number;
    }
    interface Page {
        getViewport(options: { scale: number }): Viewport;
        render(options: { canvasContext: CanvasRenderingContext2D; viewport: Viewport }): { promise: Promise<void> };
    }
    interface OutlineItem {
        title: string;
        dest: string | unknown[] | null;
        items?: OutlineItem[];
    }
    interface Document {
        numPages: number;
        getPage(n: number): Promise<Page>;
        getOutline(): Promise<OutlineItem[] | null>;
        getDestination(id: string): Promise<unknown[] | null>;
        getPageIndex(ref: unknown): Promise<number>;
    }
    interface Lib {
        GlobalWorkerOptions: { workerSrc: string };
        getDocument(src: { url: string; wasmUrl?: string }): { promise: Promise<Document> };
    }
}

// ---- DOMPurify (global, loaded by <script> on the metadata editor) ---------
declare namespace DOMPurifyTypes {
    interface Config {
        ALLOWED_TAGS?: string[];
        ALLOWED_ATTR?: string[];
        ADD_FORBID_CONTENTS?: string[];
        RETURN_DOM_FRAGMENT?: boolean;
    }
    interface AttrHookData {
        attrName: string;
        attrValue: string;
        keepAttr: boolean;
    }
    interface Purifier {
        sanitize(dirty: string, cfg: Config & { RETURN_DOM_FRAGMENT: true }): DocumentFragment;
        addHook(hook: "uponSanitizeElement", fn: (node: Node) => void): void;
        addHook(hook: "uponSanitizeAttribute", fn: (node: Element, data: AttrHookData) => void): void;
    }
}
declare const DOMPurify: DOMPurifyTypes.Purifier;
