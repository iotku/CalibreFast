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
        /** index: the spine item (chapter); displayed: the screen within it. */
        start: { cfi: string; index: number; percentage?: number; displayed?: { page: number; total: number } };
        end: { cfi: string };
        atStart?: boolean;
        atEnd?: boolean;
    }
    interface Section {
        href: string;
        /** Absolute path inside the book, e.g. "/OEBPS/Text/ch1.xhtml". */
        url: string;
        load(request: (url: string) => Promise<unknown>): Promise<Element>;
        /** The loaded chapter (while loaded). */
        document?: Document;
        /** The chapter's markup as it will be rendered (set while rendering). */
        output?: string;
        /** Matches in the loaded chapter's text. */
        find(query: string): { cfi: string; excerpt: string }[];
        unload(): void;
    }
    interface Spine {
        length?: number;
        hooks: { serialize: { register(fn: (output: string, section: Section) => Promise<void> | void): void } };
        get(target: number | string): Section | null;
    }
    interface View {
        document: Document;
        /** The highlights' SVG layer, once one is drawn; render() places them again from their text. */
        pane?: { render(): void };
    }
    /** A rendered chapter (its iframe's document). */
    interface Contents {
        document: Document;
        window: Window;
    }
    interface Rendition {
        display(target?: string): Promise<void>;
        next(): Promise<void>;
        prev(): Promise<void>;
        on(event: "relocated", cb: (location: Location) => void): void;
        on(event: "rendered", cb: (section: Section, view: View) => void): void;
        /** A text selection was made (it fires once the selection stops changing). */
        on(event: "selected", cb: (cfiRange: string, contents: Contents) => void): void;
        /** The DOM range of a CFI, when its chapter is on screen. */
        getRange(cfi: string): Range | undefined;
        /** Lays the chapter out again for the container's size, at the same place. */
        resize(): void;
        /** "paginated" or "scrolled-doc". */
        flow(flow: string): void;
        getContents(): Contents[];
        /** The chapters on screen: the manager's collection, or [] before there is one. */
        views(): { all(): View[] } | View[];
        hooks: { content: { register(fn: (contents: Contents) => void): void } };
        annotations: {
            highlight(cfi: string, data?: object, cb?: () => void, className?: string, styles?: Record<string, string>): void;
            remove(cfi: string, type: string): void;
        };
    }
    interface Book {
        ready: Promise<void>;
        spine: Spine;
        navigation: { toc: NavItem[] };
        locations: {
            generate(chars: number): Promise<string[]>;
            /** The generated locations, to store and load() later. */
            save(): string;
            load(locations: string): string[];
            percentageFromCfi(cfi: string): number;
            cfiFromPercentage(percentage: number): string;
            locationFromCfi(cfi: string): number;
            length(): number;
        };
        /** Print page numbers, when the book has a page list. */
        pageList?: { pages: string[]; lastPage: number; pageFromCfi(cfi: string): number };
        packaging?: { metadata?: { title?: string; creator?: string; language?: string; direction?: string } };
        renderTo(element: string | Element, options: { width: string; height: string; flow: string }): Rendition;
        coverUrl(): Promise<string | null>;
        load(path: string): Promise<unknown>;
        /** Only when opened from a whole .epub (ArrayBuffer). */
        archive: { createUrl(path: string, options?: { base64?: boolean }): Promise<string> };
    }
}
/**
 * Opening from an ArrayBuffer of the whole .epub needs the JSZip global. A
 * URL with requestMethod reads every file through that function instead.
 */
declare function ePub(
    source: string | ArrayBuffer,
    options?: {
        requestMethod?: (url: string, type?: string | null) => Promise<unknown>;
        replacements?: "none" | "blobUrl" | "base64";
    },
): EpubJS.Book;
interface Window {
    ePub?: typeof ePub;
}

// ---- pdf.js (ES module imported by web/vendor.ts) --------------------------
declare namespace PdfJS {
    interface Viewport {
        /** Device pixels at this viewport's scale. */
        width: number;
        height: number;
        /** [x1, y1, x2, y2] in PDF units to viewport pixels (corners in no particular order). */
        convertToViewportRectangle(rect: number[]): number[];
    }
    interface RenderTask {
        /** Rejects with a RenderingCancelledException after cancel(). */
        promise: Promise<void>;
        cancel(): void;
    }
    interface Page {
        getViewport(options: { scale: number }): Viewport;
        render(options: {
            canvasContext: CanvasRenderingContext2D;
            viewport: Viewport;
            /** Applied after the viewport's own: used to draw one region of the page. */
            transform?: number[];
        }): RenderTask;
        getTextContent(): Promise<{ items: (TextItem | { type: string })[] }>;
        streamTextContent(): ReadableStream;
        getAnnotations(): Promise<Annotation[]>;
    }
    interface TextItem {
        str: string;
        /** The text's line ends after this item. */
        hasEOL: boolean;
    }
    /** A destination inside the document: a name, or [page reference, …]. */
    type Dest = string | unknown[] | null;
    interface Annotation {
        subtype: string;
        /** [x1, y1, x2, y2] in PDF units. */
        rect: number[];
        /** Links: an http(s)/mailto URL pdf.js has checked, */
        url?: string;
        /** or a place in the document. */
        dest?: Dest;
    }
    interface OutlineItem {
        title: string;
        dest: Dest;
        items?: OutlineItem[];
    }
    interface Document {
        numPages: number;
        getPage(n: number): Promise<Page>;
        getOutline(): Promise<OutlineItem[] | null>;
        getDestination(id: string): Promise<unknown[] | null>;
        getPageIndex(ref: unknown): Promise<number>;
        /** metadata: the XMP packet, when there is one. */
        getMetadata(): Promise<{ metadata: { getRaw(): string } | null }>;
        /** Stops loading and frees the worker's copy. */
        destroy(): Promise<void>;
    }
    interface LoadOptions {
        /** Fetch only what's asked for, not the rest in the background. */
        disableAutoFetch?: boolean;
        disableStream?: boolean;
    }
    /** Selectable text over a page: spans placed in % of the container, sized by --total-scale-factor. */
    interface TextLayer {
        render(): Promise<void>;
        cancel(): void;
    }
    interface Lib {
        GlobalWorkerOptions: { workerSrc: string };
        getDocument(
            src: LoadOptions & {
                url: string;
                wasmUrl?: string;
                cMapUrl?: string;
                cMapPacked?: boolean;
                standardFontDataUrl?: string;
                iccUrl?: string;
            },
        ): { promise: Promise<Document> };
        TextLayer: new (options: { textContentSource: ReadableStream; container: HTMLElement; viewport: Viewport }) => TextLayer;
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
