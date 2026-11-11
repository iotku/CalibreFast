/**
 * An epub.js book whose files come from an EpubSource (epub-source.ts).
 *
 * epub.js is given a URL that is never fetched (/epub-virtual/<uuid>/) and a
 * requestMethod that reads the file out of the source instead. Its own
 * resource replacement is off: it would turn every image in the book into a
 * blob before showing the first page. Instead, as each chapter is rendered,
 * its images, styles and fonts become blob: URLs, read when first needed.
 */

import { mediaType, type EpubSource } from "./epub-source.js";

const XLINK = "http://www.w3.org/1999/xlink";

/** The never-fetched URL epub.js sees as the book's root. */
export function virtualRoot(uuid: string): string {
    return `${location.origin}/epub-virtual/${encodeURIComponent(uuid)}/`;
}

export interface OpenedBook {
    book: EpubJS.Book;
    /** A file's path inside the book from a URL epub.js made ("OEBPS/ch1.xhtml"). */
    pathOf(url: string): string;
}

export function openEpub(source: EpubSource, uuid: string): OpenedBook {
    const root = virtualRoot(uuid);
    const pathOf = (url: string): string => {
        const rel = url.startsWith(root) ? url.slice(root.length) : url.replace(/^\/+/, "");
        return safeDecode(rel.split("#")[0]?.split("?")[0] ?? "");
    };

    const requestMethod = async (url: string, type?: string | null): Promise<unknown> => {
        const path = pathOf(url);
        const bytes = await source.read(path);
        const kind = (type || path.slice(path.lastIndexOf(".") + 1)).toLowerCase();
        if (kind === "blob") return new Blob([bytes as BlobPart], { type: mediaType(path) });
        if (kind === "binary") return bytes.slice().buffer;
        const text = new TextDecoder().decode(bytes);
        switch (kind) {
            case "json":
                return JSON.parse(text);
            case "xml":
            case "opf":
            case "ncx":
                return new DOMParser().parseFromString(text, "text/xml");
            case "xhtml":
                return new DOMParser().parseFromString(text, "application/xhtml+xml");
            case "html":
            case "htm":
                return new DOMParser().parseFromString(text, "text/html");
            default:
                return text;
        }
    };

    const book = ePub(root, { requestMethod, replacements: "none" });
    const links = resourceLinker(source);
    book.spine.hooks.serialize.register(async (_output, section) => {
        const doc = section.document;
        if (!doc) return;
        await links(doc, pathOf(section.url));
        section.output = new XMLSerializer().serializeToString(doc);
    });
    return { book, pathOf };
}

/**
 * Points a chapter's images, media, stylesheets (and the fonts and images
 * they use) at blob: URLs of the book's own files. Each file is read and
 * made into a blob once.
 */
function resourceLinker(source: EpubSource): (doc: Document, chapterPath: string) => Promise<void> {
    const blobs = new Map<string, Promise<string>>();

    const blobUrl = (path: string, make?: () => Promise<Blob>): Promise<string> => {
        let p = blobs.get(path);
        if (!p) {
            p = (make ? make() : source.read(path).then((b) => new Blob([b as BlobPart], { type: mediaType(path) }))).then((b) =>
                URL.createObjectURL(b),
            );
            blobs.set(path, p);
            p.catch(() => blobs.delete(path));
        }
        return p;
    };

    /** A stylesheet with its url()s and @imports pointed at blobs. */
    const cssBlob = (path: string): Promise<string> =>
        blobUrl(path, async () => {
            const text = new TextDecoder().decode(await source.read(path));
            return new Blob([await rewriteCss(text, path)], { type: "text/css" });
        });

    const rewriteCss = async (css: string, fromPath: string): Promise<string> => {
        const re = /url\(\s*(['"]?)([^'")]+)\1\s*\)|@import\s+(['"])([^'"]+)\3/g;
        const found = new Map<string, Promise<string | null>>();
        for (const m of css.matchAll(re)) {
            const ref = (m[2] ?? m[4] ?? "").trim();
            if (found.has(ref)) continue;
            const path = resolve(ref, fromPath);
            const isImport = m[4] !== undefined;
            found.set(ref, path && source.has(path) ? (isImport ? cssBlob(path) : blobUrl(path)).catch(() => null) : Promise.resolve(null));
        }
        const urls = new Map<string, string | null>();
        for (const [ref, p] of found) urls.set(ref, await p);
        return css.replace(re, (all, _q, urlRef: string | undefined, _q2, importRef: string | undefined) => {
            const ref = (urlRef ?? importRef ?? "").trim();
            const u = urls.get(ref);
            if (!u) return all;
            return urlRef !== undefined ? `url("${u}")` : `@import "${u}"`;
        });
    };

    return async (doc, chapterPath) => {
        const jobs: Promise<void>[] = [];
        const swap = (el: Element, attr: string, ns: string | null, css = false): void => {
            const ref = ns ? el.getAttributeNS(ns, attr) : el.getAttribute(attr);
            const path = ref ? resolve(ref, chapterPath) : null;
            if (!path || !source.has(path)) return;
            jobs.push(
                (css ? cssBlob(path) : blobUrl(path))
                    .then((u) => (ns ? el.setAttributeNS(ns, attr, u) : el.setAttribute(attr, u)))
                    .catch(() => {}), // a missing file stays a broken link
            );
        };
        for (const el of doc.querySelectorAll("img[src], audio[src], video[src], source[src], track[src], embed[src], input[src]")) swap(el, "src", null);
        for (const el of doc.querySelectorAll("video[poster]")) swap(el, "poster", null);
        for (const el of doc.querySelectorAll("object[data]")) swap(el, "data", null);
        for (const el of doc.querySelectorAll("link[href]")) {
            if (/\bstylesheet\b/i.test(el.getAttribute("rel") ?? "")) swap(el, "href", null, true);
        }
        // SVG images (common for covers): href or xlink:href.
        for (const el of doc.getElementsByTagNameNS("*", "image")) {
            if (el.hasAttribute("href")) swap(el, "href", null);
            if (el.hasAttributeNS(XLINK, "href")) swap(el, "href", XLINK);
        }
        for (const el of doc.getElementsByTagNameNS("*", "style")) {
            const css = el.textContent ?? "";
            if (/url\(|@import/i.test(css)) jobs.push(rewriteCss(css, chapterPath).then((out) => void (el.textContent = out)));
        }
        await Promise.all(jobs);
    };
}

/** A reference from the file at fromPath, as a path in the book; null if it leaves the book. */
function resolve(ref: string, fromPath: string): string | null {
    if (!ref || ref.startsWith("#") || /^[a-z][a-z0-9+.-]*:/i.test(ref)) return null; // data:, http:, blob:…
    try {
        const u = new URL(ref, `http://book.invalid/${fromPath}`);
        if (u.host !== "book.invalid") return null;
        return safeDecode(u.pathname.slice(1));
    } catch {
        return null;
    }
}

function safeDecode(s: string): string {
    try {
        return decodeURIComponent(s);
    } catch {
        return s;
    }
}
