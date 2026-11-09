// Types for the third-party libraries loaded from CDNs (htmx, epub.js,
// pdf.js). Only the parts CalibreFast uses are described — extend as needed.

// ---- htmx (global, loaded by <script> on library pages) --------------------
interface HtmxApi {
    ajax(method: string, url: string, options: { target: string; swap?: string }): Promise<void>;
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
        start: { cfi: string; percentage?: number };
        end: { cfi: string };
        atStart?: boolean;
        atEnd?: boolean;
    }
    interface Section {
        href: string;
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
        };
        renderTo(element: string | Element, options: { width: string; height: string; flow: string }): Rendition;
        coverUrl(): Promise<string | null>;
        load(path: string): Promise<unknown>;
    }
}
declare function ePub(url: string): EpubJS.Book;
interface Window {
    ePub?: typeof ePub;
}

// ---- pdf.js (ES module imported from the CDN) ------------------------------
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
        getDocument(src: string): { promise: Promise<Document> };
    }
}
declare module "https://cdn.jsdelivr.net/npm/pdfjs-dist@5.7.284/build/pdf.mjs" {
    export const GlobalWorkerOptions: PdfJS.Lib["GlobalWorkerOptions"];
    export function getDocument(src: string): { promise: Promise<PdfJS.Document> };
}
