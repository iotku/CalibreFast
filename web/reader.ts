import { colorValue, setupAnnotationUI } from "./annotation-ui.js";
import { createAnnotationStore, type Annotation } from "./annotations.js";
import { currentUser, getBook, registerWorker, updateBook, type CachedBook } from "./book-cache.js";
import { shouldKeep, storeBook, sweep } from "./retention.js";
import { holdBackgroundDownloads } from "./page-busy.js";
import { createSyncIndicator } from "./sync-indicator.js";
import { keepCover } from "./offline-books.js";
import { byId, el, errorMessage, maybeId, modalOpen } from "./dom.js";
import { openEpub } from "./epub-open.js";
import { allowEpubChapters } from "./trusted.js";
import { canInflate, openBuffer, openRemote, type EpubSource } from "./epub-source.js";
import { syncGuestBook } from "./guest-sync.js";
import { offerSignIn, pageColors, setupChrome } from "./reader-chrome.js";
import { keepsHistory, leaveReader, previewSync, readerBook, type ReaderBook } from "./reader-preview.js";
import { getSettings, onSettingsChange, type ReaderSettings } from "./reader-settings.js";
import { noteFormatOpened } from "./last-format.js";
import { createActivityTracker } from "./activity.js";
import { createReadingSync, renderBookmarks, type Bookmark, type BookmarkHandlers } from "./reading-sync.js";

const found = readerBook();
if (!found) {
    document.body.replaceChildren(el("p", {}, "No book specified."));
    throw new Error("missing uuid");
}
const target: ReaderBook = found;
const uuid: string = target.id;
/** Opened from the metadata editor: nothing is kept or saved (reader-preview.ts). */
const preview = target.preview;
/**
 * "epub", or a Kindle format read converted to EPUB (formats.ts): places are
 * saved under it. A converted book isn't kept in this browser (there is no
 * store for it): it is read from the server each time, as with "never keep".
 */
const format = target.format;
const keeps = !preview && format === "epub";

const container = byId("reader-container");
const settings = getSettings;
document.body.dataset.margin = settings().margin;

const chrome = setupChrome({
    onScrub: (fraction, done) => scrub(fraction, done),
    onTheme: () => restyle(),
});
chrome.setScrubbable(false);

/**
 * Opening a book:
 *   - a copy kept in this browser (book-cache.ts) opens at once, offline too;
 *   - otherwise only what the first page needs is read from the server
 *     (epub-source.ts: HTTP ranges of the .epub), and the whole file follows
 *     in the background, to be kept for next time and for offline reading;
 *   - browsers without DecompressionStream download it whole first, as before.
 * Chapters are always rendered by epub.js in a sandboxed iframe without
 * scripts; nothing from the book is served by our origin as a page.
 * A preview neither uses nor keeps a copy in this browser.
 */
allowEpubChapters(); // epub.js sets each chapter as its sandboxed iframe's srcdoc
if (!preview) registerWorker();
// Background book downloads in other tabs wait while this book loads (page-busy.ts).
const releaseDownloads = preview ? (): void => {} : holdBackgroundDownloads();
// The top bar's sync button: saved offline, and synced with the server (sync-indicator.ts).
const syncButton = keeps ? createSyncIndicator(uuid, "epub") : null;
const downloadUrl = target.file(format);
/** Below this the whole file is fetched at once: it usually beats the ranges. */
const SMALL_BOOK = 3 << 20;

/** The whole .epub, streamed; onProgress gets the text to show, and the fraction (null: size unknown). */
async function downloadBook(onProgress?: (text: string, fraction: number | null) => void): Promise<{ buf: Uint8Array; lastModified: string }> {
    const res = await fetch(downloadUrl, { credentials: "same-origin" });
    // 422: a Kindle book that can't be converted (DRM); the server says which (convert_read.go).
    if (res.status === 422) throw new Error((await res.text()).trim().slice(0, 200));
    if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
    const total = Number(res.headers.get("content-length")) || 0;
    const chunks: Uint8Array[] = [];
    let got = 0;
    const reader = res.body.getReader();
    for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
        got += value.length;
        onProgress?.(total ? `Loading ${Math.round((got / total) * 100)}%` : `Loading ${(got / 1048576).toFixed(1)} MB`, total ? got / total : null);
    }
    const buf = new Uint8Array(got);
    let at = 0;
    for (const c of chunks) {
        buf.set(c, at);
        at += c.length;
    }
    return { buf, lastModified: res.headers.get("last-modified") ?? "" };
}

let source: EpubSource | null = null;
let cached: CachedBook | null = null;
let book: EpubJS.Book;
/** A file's path inside the book from a URL epub.js made. */
let pathOf = (url: string): string => decodeURIComponent(url.replace(/^\/+/, "").split("#")[0] ?? "");
try {
    cached = keeps ? await getBook(uuid).catch(() => null) : null;
    if (cached && canInflate) {
        source = openBuffer(new Uint8Array(await cached.bytes.arrayBuffer()), cached.lastModified);
    } else if (canInflate) {
        source = await openRemote(downloadUrl).catch((err: unknown) => {
            console.warn("reading the book in parts failed, downloading it whole:", err);
            return null;
        });
        if (!source) {
            const { buf, lastModified } = await downloadBook((t) => chrome.setPosition(t));
            source = openBuffer(buf, lastModified);
        }
    }
    if (source) {
        const opened = openEpub(source, uuid);
        book = opened.book;
        pathOf = opened.pathOf;
    } else {
        const { buf } = await downloadBook((t) => chrome.setPosition(t));
        book = ePub(buf.buffer as ArrayBuffer); // JSZip, in memory
    }
    chrome.setPosition("—");
} catch (err) {
    releaseDownloads(); // nothing more is loading: background downloads needn't wait
    chrome.setPosition("Failed to load");
    container.className = "reader-message";
    container.textContent = `Couldn't load this book (${errorMessage(err)}).`;
    throw err;
}

/** Keep the whole file in this browser, with the book's title for the offline page. */
async function keepCopy(buf: Uint8Array, lastModified: string, locations?: string): Promise<void> {
    // The book's own choice (offline-control.ts), then the device's (prefs.ts, retention.ts).
    if (!keeps || !shouldKeep(uuid, buf.length)) return;
    await book.ready;
    const meta = book.packaging?.metadata;
    await storeBook({
        uuid,
        format: "epub",
        user: currentUser(),
        bytes: new Blob([buf as BlobPart], { type: "application/epub+zip" }),
        lastModified,
        size: buf.length,
        title: meta?.title || document.title,
        authors: meta?.creator ?? "",
        locations,
        lastOpened: Date.now(),
        statusSeen: cached?.statusSeen,
        savedAt: cached?.savedAt,
    });
    void keepCover(uuid).catch(() => {}); // for the offline Reading page (offline-books.ts)
}

/**
 * The rest of the book in the background (when it was read in parts), then
 * kept for next time. Resolves when that is over, whether it worked or not:
 * page numbers and search wait for it, and otherwise read chapter by chapter.
 */
let wholeDone: Promise<void> = Promise.resolve();
function fetchRest(): void {
    const src = source;
    if (!src || src.whole) return;
    syncButton?.downloading(null);
    wholeDone = downloadBook((_, f) => syncButton?.downloading(f))
        .then(async ({ buf, lastModified }) => {
            // The file changed since the first page was read: keep the new one for next time only.
            if (lastModified === src.lastModified) src.useBuffer(buf);
            await keepCopy(buf, lastModified);
        })
        .catch((err: unknown) => console.warn("downloading the rest of the book failed:", err))
        .finally(() => syncButton?.downloading(false));
}
if (source) syncButton?.setSize(source.size);
if (source && !source.whole && source.size < SMALL_BOOK) fetchRest();

if (cached) {
    void updateBook(uuid, "epub", { lastOpened: Date.now() }).catch(() => {});
    // The copy kept here may be out of date: check, and fetch the new one for next time.
    void fetch(downloadUrl, { method: "HEAD", credentials: "same-origin" })
        .then(async (res) => {
            if (res.ok) void sweep(undefined, uuid).catch(() => {}); // online: tidy up other kept books
            const lm = res.headers.get("last-modified") ?? "";
            if (!res.ok || !lm || lm === cached?.lastModified) return;
            const { buf, lastModified } = await downloadBook();
            await keepCopy(buf, lastModified);
            chrome.toast("A newer copy of this book was downloaded. Reopen it to see it.");
        })
        .catch(() => {}); // offline
} else if (source && !preview) {
    void sweep(undefined, uuid).catch(() => {}); // the server answered: tidy up other kept books
}
if (!cached && source?.whole) {
    // Downloaded whole (small book, or no Range support): keep it.
    syncButton?.downloading(null);
    void source.complete
        .then((buf) => keepCopy(buf, source?.lastModified ?? ""))
        .catch(() => {})
        .finally(() => syncButton?.downloading(false));
}

const epubFlow = (s: ReaderSettings): string => (s.flow === "scrolled" ? "scrolled-doc" : "paginated");
const rendition = book.renderTo(container, { width: "100%", height: "100%", flow: epubFlow(settings()) });

let bookLang = "";
/** Right-to-left books turn the other way: "forward" is to the left. */
let rtl = false;
void book.ready.then(() => {
    const meta = book.packaging?.metadata;
    bookLang = meta?.language ?? "";
    rtl = meta?.direction === "rtl";
    const title = byId("book-title");
    if (!title.textContent && meta?.title) title.textContent = meta.title;
    if (meta?.title) document.title = meta.title;
});

// ---- typography and colours -------------------------------------------------
// A stylesheet of ours in every chapter, after the book's own.

const FONTS = {
    serif: '"Iowan Old Style", "Palatino Linotype", Palatino, Georgia, serif',
    sans: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif',
    mono: 'ui-monospace, "SF Mono", Menlo, Consolas, monospace',
};
const STYLE_ID = "cf-reader-style";

function contentCss(): string {
    const s = settings();
    const c = pageColors();
    const text = "body p, body li, body dd, body blockquote";
    const rules = [
        `html { font-size: ${s.fontSize}% !important; }`,
        // The page colour is ours (the window's), so the margins match. An
        // iframe is only see-through when its colour scheme matches ours.
        `:root { color-scheme: ${c.dark ? "dark" : "light"}; }`,
        "html, body { background: transparent !important; }",
        `body { color: ${c.fg}; }`,
        "img, svg, video { max-width: 100% !important; max-height: 100vh !important; object-fit: contain; break-inside: avoid; }",
    ];
    // Light keeps the book's own colours. Every other theme replaces text
    // and background colours (images are left alone).
    const lightNow = s.theme === "light" || (s.theme === "auto" && !c.dark);
    if (!lightNow) {
        rules.push(
            `body { color: ${c.fg} !important; }`,
            "body * { color: inherit !important; background-color: transparent !important; border-color: currentColor !important; }",
            `a:link, a:visited, a:link *, a:visited * { color: ${c.link} !important; }`,
        );
    }
    if (s.font !== "publisher") {
        rules.push(`body, body *:not(pre):not(code):not(kbd):not(samp) { font-family: ${FONTS[s.font]} !important; }`);
    }
    if (s.lineHeight) rules.push(`body, ${text}, body div { line-height: ${s.lineHeight} !important; }`);
    if (s.align !== "publisher") rules.push(`${text} { text-align: ${s.align} !important; }`);
    if (s.hyphens) rules.push(`body, ${text} { hyphens: auto !important; -webkit-hyphens: auto !important; }`);
    return rules.join("\n");
}

function styleDocument(doc: Document): void {
    let el = doc.getElementById(STYLE_ID);
    if (!el) {
        el = doc.createElement("style");
        el.id = STYLE_ID;
        (doc.head ?? doc.documentElement).appendChild(el);
    }
    el.textContent = contentCss();
    // Hyphenation needs to know the language.
    if (bookLang && !doc.documentElement.getAttribute("lang")) doc.documentElement.setAttribute("lang", bookLang);
}

/** Applies the current settings to the chapters on screen. */
function restyle(): void {
    for (const c of rendition.getContents()) styleDocument(c.document);
}
rendition.hooks.content.register((contents) => {
    relativeFontSizes(contents.document);
    styleDocument(contents.document);
});

// ---- the book's font sizes --------------------------------------------------
// The reader's text size is the root size (html, above). A book that sizes its
// text in absolute units (p { font-size: 12pt }, or px, or "small") ignores
// it, so those sizes become the same size in rem: what the publisher meant at
// the default text size, scaled by the reader's. Relative sizes (em, %, rem)
// already follow the root and are left alone, so headings, small print and
// the like keep their proportions.

/** One rem is the default 16px (12pt). */
const REM_PER_UNIT: Record<string, number> = { px: 1 / 16, pt: 1 / 12, pc: 1, in: 6, cm: 6 / 2.54, mm: 0.6 / 2.54, q: 0.15 / 2.54 };
/** The CSS absolute-size keywords, as browsers size them from a 16px medium. */
const KEYWORD_REM: Record<string, number> = {
    "xx-small": 9 / 16, "x-small": 10 / 16, small: 13 / 16, medium: 1, large: 18 / 16, "x-large": 1.5, "xx-large": 2, "xxx-large": 3,
};

/** An absolute font size as rem, or "" for a relative one (or anything else). */
function remFontSize(value: string): string {
    const v = value.trim().toLowerCase();
    const kw = KEYWORD_REM[v];
    if (kw !== undefined) return `${+kw.toFixed(4)}rem`;
    const m = /^(\d*\.?\d+)(px|pt|pc|in|cm|mm|q)$/.exec(v);
    if (!m) return "";
    return `${+(parseFloat(m[1]) * REM_PER_UNIT[m[2]]).toFixed(4)}rem`;
}

function relativeStyle(style: CSSStyleDeclaration): void {
    const rem = style.fontSize ? remFontSize(style.fontSize) : "";
    if (rem) style.setProperty("font-size", rem, style.getPropertyPriority("font-size"));
}

// Duck-typed: a chapter's frame has CSSOM classes of its own, so instanceof
// with ours would never match.
function relativeRules(rules: CSSRuleList, depth: number): void {
    for (const rule of Array.from(rules)) {
        const r = rule as Partial<CSSImportRule & CSSStyleRule & CSSGroupingRule>;
        if (r.styleSheet) {
            if (depth < 8) relativeSheet(r.styleSheet, depth + 1);
            continue;
        }
        // Style rules, and the ones inside @media, @supports and the like.
        if (r.style) relativeStyle(r.style);
        if (r.cssRules) relativeRules(r.cssRules, depth);
    }
}

function relativeSheet(sheet: CSSStyleSheet, depth: number): void {
    try {
        relativeRules(sheet.cssRules, depth);
    } catch {
        // A sheet we may not read (another origin): it keeps its sizes.
    }
}

/** Turns the chapter's absolute font sizes, in its stylesheets and style attributes, into rem. */
function relativeFontSizes(doc: Document): void {
    const done = new WeakSet<StyleSheet>();
    const sweep = (): void => {
        for (const sheet of Array.from(doc.styleSheets)) {
            if (done.has(sheet) || (sheet.ownerNode as Element | null)?.id === STYLE_ID) continue;
            done.add(sheet);
            relativeSheet(sheet, 0);
        }
    };
    sweep();
    // Sheets still loading are done when they arrive.
    for (const link of Array.from(doc.querySelectorAll<HTMLLinkElement>('link[rel~="stylesheet"]'))) {
        if (!link.sheet) link.addEventListener("load", sweep, { once: true });
    }
    for (const el of Array.from(doc.querySelectorAll<HTMLElement>("[style]"))) relativeStyle(el.style);
}

// Settings that change how much fits on a page: lay out again, once the
// taps on a stepper have stopped.
let relayoutTimer: ReturnType<typeof setTimeout> | undefined;
// epub.js places highlights again only when a chapter's frame changes size,
// and resize() does nothing when the window hasn't. Text that moved inside
// the same frame (line spacing, font, alignment) would leave them where the
// words were, so they're placed again after every relayout, and once it has
// settled on a page (a resize that does take shows the chapter again, later).
const placeMarks = (): void => {
    requestAnimationFrame(() => {
        const views = rendition.views();
        for (const v of Array.isArray(views) ? views : views.all()) v.pane?.render();
    });
};
const relayout = (): void => {
    rendition.resize();
    placeMarks();
};
rendition.on("relocated", placeMarks);
// A font that arrives after the text was laid out moves it again.
rendition.hooks.content.register((contents) => contents.document.fonts.addEventListener("loadingdone", placeMarks));
onSettingsChange((s, changed) => {
    if (changed.includes("margin")) document.body.dataset.margin = s.margin;
    if (changed.includes("flow")) {
        rendition.flow(epubFlow(s));
        if (currentCfi) void rendition.display(currentCfi);
        return;
    }
    const layout: (keyof ReaderSettings)[] = ["font", "fontSize", "lineHeight", "margin", "align", "hyphens"];
    if (!changed.some((k) => layout.includes(k))) return;
    restyle();
    clearTimeout(relayoutTimer);
    relayoutTimer = setTimeout(relayout, 150);
});
// The page's box moves with the bars' measured heights, which epub.js
// (watching only the window) wouldn't notice.
new ResizeObserver(() => {
    clearTimeout(relayoutTimer);
    relayoutTimer = setTimeout(relayout, 150);
}).observe(container);

// ---- position: resume (local or synced), save as the reader moves ----------

/** The book's title as the page shows it, for the Reading list kept for offline (offline-books.ts). */
const bookTitle = (): string => document.getElementById("book-title")?.textContent?.trim() ?? "";

const sync = preview
    ? previewSync(chrome.setStatus)
    : await createReadingSync(uuid, format, {
          legacyKey: format === "epub" ? `reader-cfi-${uuid}` : undefined,
          onStatus: chrome.setStatus,
          title: bookTitle,
      });
if (keeps) noteFormatOpened(sync.user, uuid, "epub"); // the book's cover opens this format next time (last-format.ts)
// Just signed in from this page: what was read here signed out goes to the
// account first, so the place below is the synced one.
if (sync.signedIn && !preview) await syncGuestBook(uuid, chrome.setStatus);
const saved = await sync.loadProgress();
if (!sync.signedIn && !preview) {
    chrome.setStatus("Position saved on this device");
    offerSignIn();
}

let locationsReady = false;
let currentCfi: string | null = null;
/** The last place on screen: with currentCfi, the pages the screen shows. */
let currentEndCfi: string | null = null;
/** This screen within its chapter ("3 of 12"); "" when scrolled or the chapter is one screen. */
let currentScreen = "";
let currentPercent = 0;
// The first "relocated" is just the book opening (epub.js may normalise the
// saved CFI to the page start). Only moving away from it is saved — and that
// is what marks the book as being read.
let openingCfi: string | null = null;

function percentFor(loc: EpubJS.Location): number {
    if (loc.atEnd) return 1;
    if (!locationsReady) return 0;
    return book.locations.percentageFromCfi(loc.start.cfi) || 0;
}

/**
 * "p. N of M": the print edition's page numbers when the book has a page
 * list, otherwise synthetic pages (~1600 characters, roughly a printed page;
 * the same on every device and font size). Given where the screen ends too,
 * "pp. N–E of M": a wide screen or small font shows several pages at once,
 * and a range says so instead of seeming to skip pages on every turn.
 */
const CHARS_PER_PAGE = 1600;
function pageText(cfi: string, endCfi?: string | null): string {
    const page = pageAt(cfi);
    if (!page) return "";
    const end = endCfi ? pageAt(endCfi) : null;
    if (end && end.of === page.of && end.n > page.n) return `pp. ${page.n}–${end.n} of ${page.of}`;
    return `p. ${page.n} of ${page.of}`;
}
/** The page (1-based) cfi is on, of how many: as pageText shows it; null until locations are ready. */
function pageAt(cfi: string): { n: number; of: number | string } | null {
    const pl = book.pageList;
    const printed = pl?.pages.length ? pl.pageFromCfi(cfi) : -1;
    if (printed > 0) return { n: printed, of: pl?.lastPage ?? "" };
    if (!locationsReady) return null;
    return { n: book.locations.locationFromCfi(cfi) + 1, of: book.locations.length() };
}
function positionText(cfi: string, percent: number, endCfi?: string | null): string {
    const page = pageText(cfi, endCfi);
    if (!page) return "—";
    return locationsReady ? `${page} · ${Math.round(percent * 100)}%` : page;
}
function showPosition(): void {
    if (!currentCfi) return;
    chrome.setPosition(positionText(currentCfi, currentPercent, currentEndCfi), currentPercent);
    const label = currentTocEntry()?.label ?? "";
    chrome.setChapter(currentScreen ? (label ? `${label} · ${currentScreen}` : `${currentScreen} in this chapter`) : label);
}

/**
 * The screen within its chapter, from epub.js: "3 of 12", moving on by one
 * each turn whatever the screen or font size. epub.js counts columns, two to
 * a screen in a spread.
 */
function screenInChapter(loc: EpubJS.Location): string {
    const d = loc.start.displayed;
    if (!d || settings().flow === "scrolled") return "";
    const per = Math.max(1, rendition._layout?.divisor ?? 1);
    const total = Math.ceil(d.total / per);
    if (total <= 1) return "";
    return `${Math.min(total, Math.ceil(d.page / per))} of ${total}`;
}

// ---- reading stats (activity.ts): pages read forward, not jumped to -------

const activity = preview
    ? null
    : createActivityTracker(sync.user, uuid, format, (n) => chrome.toast(`Daily goal reached: ${n} pages read today 🎉`));
/** The page the reader was last on (pageAt); null: unknown, or after a jump. */
let lastPage: number | null = null;
/** How many pages that screen showed: a turn moves on about that many. */
let lastSpan = 1;
/** The next "relocated" comes from going somewhere (display), not from reading on. */
let jumped = true;
const display = rendition.display.bind(rendition);
rendition.display = (target?: string): Promise<void> => {
    jumped = true;
    return display(target);
};
// Using the chapter itself (its own document) is reading too.
rendition.on("rendered", (_section, view) => {
    for (const ev of ["keydown", "pointerdown", "wheel", "touchstart"]) {
        view.document.addEventListener(ev, () => activity?.touch(), { passive: true });
    }
});

function countPages(cfi: string, endCfi: string | null, atEnd: boolean): void {
    const n = pageAt(cfi)?.n ?? null;
    const e = endCfi ? (pageAt(endCfi)?.n ?? null) : null;
    // Reading on: a screen's worth at most (a wide screen shows several
    // pages), or a few for scrolling. More than that skipped pages unread.
    const most = Math.max(5, lastSpan + 1);
    if (!jumped && n !== null && lastPage !== null && n > lastPage && n - lastPage <= most) {
        // The pages left behind were read; the one now at the top isn't done yet,
        // unless this is the end of the book.
        activity?.pagesRead(lastPage, atEnd && e !== null && e >= n ? e : n - 1);
    } else activity?.touch();
    lastSpan = n !== null && e !== null && e >= n ? Math.min(20, e - n + 1) : 1;
    lastPage = n;
    jumped = false;
}

rendition.on("relocated", (loc) => {
    if (!loc?.start?.cfi) return;
    currentCfi = loc.start.cfi;
    currentEndCfi = loc.end?.cfi ?? null;
    currentScreen = screenInChapter(loc);
    currentPercent = percentFor(loc);
    countPages(currentCfi, currentEndCfi, !!loc.atEnd);
    showPosition();
    markCurrentChapter();
    if (openingCfi === null) {
        openingCfi = currentCfi;
        return;
    }
    if (currentCfi === openingCfi) return;
    openingCfi = ""; // moved: from now on every change is saved
    sync.saveProgress(currentCfi, currentPercent);
});

/** The scrubber: preview while dragging, jump on release. */
function scrub(fraction: number, done: boolean): string | void {
    if (!locationsReady) return;
    const cfi = book.locations.cfiFromPercentage(fraction);
    if (!cfi) return;
    if (!done) return positionText(cfi, fraction);
    pushJump();
    void rendition.display(cfi);
}

// ---- history: in-book jumps (links, contents) can be undone with Back ------

/** history.state on reader entries; depth = jumps since the reader opened. */
interface ReaderState {
    cfi?: string;
    depth?: number;
}
const state = (): ReaderState => (history.state as ReaderState | null) ?? {};

// Keep the current entry pointing at where the reader is now.
rendition.on("relocated", (loc) => {
    if (loc?.start?.cfi) history.replaceState({ depth: state().depth ?? 0, cfi: loc.start.cfi } satisfies ReaderState, "");
});

/** Call before an in-book jump: the current spot stays behind as a Back entry. */
function pushJump(): void {
    if (!keepsHistory(target)) return;
    history.pushState({ depth: (state().depth ?? 0) + 1 } satisfies ReaderState, "");
}

window.addEventListener("popstate", () => {
    // Back out of the note editor lands on the entry for where we already are.
    const cfi = state().cfi;
    if (cfi && cfi !== currentCfi) void rendition.display(cfi);
});

// The top-bar Back leaves the reader, skipping the in-book jumps.
document.querySelector(".btn-back")?.addEventListener("click", () => leaveReader(target, state().depth ?? 0, chrome.setStatus));

// ---- turning pages ----------------------------------------------------------

const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");

/**
 * Forward (1) or back (-1). In the scrolling layout that is most of a
 * screen, then the next chapter. fromPage: the reader did it on the page
 * (tap, swipe, key), so the bars get out of the way.
 */
function turn(dir: 1 | -1, fromPage = true): void {
    if (fromPage) chrome.hideBars();
    if (settings().flow === "scrolled") {
        const sc = container.querySelector<HTMLElement>(".epub-container");
        if (sc) {
            const max = sc.scrollHeight - sc.clientHeight;
            const atEdge = dir > 0 ? sc.scrollTop >= max - 2 : sc.scrollTop <= 2;
            if (!atEdge) {
                sc.scrollBy({ top: dir * sc.clientHeight * 0.9, behavior: reducedMotion.matches ? "auto" : "smooth" });
                return;
            }
        }
    }
    (dir > 0 ? rendition.next() : rendition.prev()).catch((e: unknown) => {
        // The first and last pages have nowhere to go.
        if (!errorMessage(e).includes("No Section Found")) console.error(e);
    });
}
/** A turn towards the right (1) or left (-1) side of the screen. */
const turnSide = (side: 1 | -1, fromPage = true): void => turn(rtl ? (-side as 1 | -1) : side, fromPage);

byId("prev-btn").addEventListener("click", () => turn(-1, false));
byId("next-btn").addEventListener("click", () => turn(1, false));

function onKey(e: KeyboardEvent): void {
    if (modalOpen()) return; // typing a note
    if (e.ctrlKey || e.metaKey || e.altKey) {
        if ((e.ctrlKey || e.metaKey) && e.key === "f") {
            e.preventDefault();
            openSearch();
        }
        return;
    }
    // Keys belong to the control that has focus (the scrubber, a tab, the search box).
    const t = e.target as Element | null;
    if (t?.ownerDocument === document && t.closest?.(".r-bar, .r-sidebar, .r-sheet")) return;
    switch (e.key) {
        case "ArrowRight":
            turnSide(1);
            break;
        case "ArrowLeft":
            turnSide(-1);
            break;
        case "PageDown":
            turn(1);
            break;
        case "PageUp":
            turn(-1);
            break;
        case " ":
            turn(e.shiftKey ? -1 : 1);
            break;
        default:
            return;
    }
    e.preventDefault();
}
window.addEventListener("keydown", onKey);

/**
 * Each chapter is its own document (an iframe), so presses, taps, swipes,
 * the wheel and keys are listened for there. A tap on the left or right
 * third turns the page; in the middle it shows or hides the bars.
 */
rendition.on("rendered", (section, view) => {
    const doc = view.document;
    const frameTop = (): number => doc.defaultView?.frameElement?.getBoundingClientRect().top ?? 0;
    doc.addEventListener("mousemove", (e) => chrome.pointerAt(frameTop() + e.clientY));
    // Out of the chapter: to the page around it (whose own mousemove follows) or the window.
    doc.addEventListener("mouseout", (e) => {
        if (!e.relatedTarget) chrome.pointerAt(frameTop() + e.clientY, true);
    });
    // A press that closed a panel does nothing else.
    let swallow = false;
    doc.addEventListener("pointerdown", () => {
        const closed = chrome.closePanels();
        swallow = notesUI.dismiss() || closed;
    });

    // Links are handled here (capture phase, before epub.js): in-book links
    // become a history entry plus a jump; external links open in a new tab
    // from this page (the chapter iframe is sandboxed and can't open windows).
    doc.addEventListener(
        "click",
        (e) => {
            // The target lives in the iframe's window: `instanceof Element` (ours) is false.
            const t = e.target as Element | null;
            const a = typeof t?.closest === "function" ? t.closest("a[href]") : null;
            const href = a?.getAttribute("href");
            if (href) {
                e.preventDefault();
                e.stopImmediatePropagation();
            }
            if (swallow) {
                swallow = false;
                return;
            }
            if (href) {
                followLink(href, section);
                return;
            }
            const hit = preview ? null : highlightAt(doc, e.clientX, e.clientY);
            if (hit) {
                notesUI.offerExisting(toScreen(doc, hit.rect), hit.annotation);
                return;
            }
            if (doc.getSelection()?.toString() || t?.closest?.("input, button, select, textarea, audio, video, summary")) return;
            const frame = doc.defaultView?.frameElement;
            const x = (frame?.getBoundingClientRect().left ?? 0) + e.clientX;
            if (x < window.innerWidth * 0.3) turnSide(-1);
            else if (x > window.innerWidth * 0.7) turnSide(1);
            else chrome.toggleBars();
        },
        true,
    );

    // Swipe sideways to turn the page (pages layout only: the scrolling
    // layout leaves touches to the browser).
    let start: { x: number; y: number; t: number } | null = null;
    doc.addEventListener(
        "touchstart",
        (e) => {
            const p = e.touches.length === 1 ? e.touches[0] : undefined;
            start = p ? { x: p.screenX, y: p.screenY, t: e.timeStamp } : null;
        },
        { passive: true },
    );
    doc.addEventListener(
        "touchend",
        (e) => {
            const p = e.changedTouches[0];
            if (!start || !p || settings().flow === "scrolled" || doc.getSelection()?.toString()) return;
            const dx = p.screenX - start.x;
            const dy = p.screenY - start.y;
            if (e.timeStamp - start.t < 700 && Math.abs(dx) > 40 && Math.abs(dx) > 1.5 * Math.abs(dy)) {
                turnSide(dx < 0 ? 1 : -1);
            }
            start = null;
        },
        { passive: true },
    );

    // The wheel turns pages, one per gesture however long a trackpad keeps
    // sending events.
    let wheelAt = 0;
    doc.addEventListener(
        "wheel",
        (e) => {
            if (settings().flow === "scrolled" || e.ctrlKey) return;
            e.preventDefault();
            const d = Math.abs(e.deltaY) >= Math.abs(e.deltaX) ? e.deltaY : e.deltaX;
            if (Math.abs(d) < 4 || e.timeStamp - wheelAt < 350) return;
            wheelAt = e.timeStamp;
            turn(d > 0 ? 1 : -1);
        },
        { passive: false },
    );

    doc.addEventListener("keydown", onKey);
});

function followLink(href: string, section: EpubJS.Section): void {
    const url = new URL(href, `epub:/${pathOf(section.url)}`);
    if (url.protocol !== "epub:") {
        if (/^(https?|mailto):$/.test(url.protocol)) window.open(url.href, "_blank", "noopener,noreferrer");
        return;
    }
    // Same-book target: find its spine item (by path), keep the #anchor.
    const path = pathOf(url.pathname);
    for (let i = 0; i < (book.spine.length ?? 0); i++) {
        const item = book.spine.get(i);
        if (item && pathOf(item.url) === path) {
            pushJump();
            void rendition.display(item.href + url.hash);
            return;
        }
    }
}

// Taps in the margins around the chapter count too.
container.addEventListener("click", (e) => {
    if (e.target !== container && !(e.target as Element).classList?.contains("epub-container")) return;
    if (e.clientX < window.innerWidth * 0.3) turnSide(-1);
    else if (e.clientX > window.innerWidth * 0.7) turnSide(1);
    else chrome.toggleBars();
});

// ---- open -------------------------------------------------------------------

// Handlers are attached first so the opening position is seen as such. A
// reload returns to where this tab was (history.state), which may be newer
// than what was saved.
let startCfi = state().cfi || saved?.position || null;
try {
    await rendition.display(startCfi || undefined);
} catch {
    startCfi = null; // stale/invalid CFI (e.g. the book file changed)
    openingCfi = null;
    await rendition.display().catch((err: unknown) => {
        releaseDownloads(); // the book won't show: background downloads needn't wait
        throw err;
    });
}

// Larger books read in parts: the rest comes once the first page is up.
fetchRest();
void wholeDone.finally(releaseDownloads);

// Page numbers need every chapter: from the copy kept here, or generated
// once the whole book has arrived (then kept with it).
void book.ready
    .then(async () => {
        if (cached?.locations) {
            book.locations.load(cached.locations);
            return;
        }
        await wholeDone;
        await book.locations.generate(CHARS_PER_PAGE);
        const saved = book.locations.save();
        if (keeps) void updateBook(uuid, "epub", { locations: saved }).catch(() => {});
    })
    .then(() => {
        locationsReady = true;
        if (currentCfi) currentPercent = book.locations.percentageFromCfi(currentCfi) || 0;
        chrome.setScrubbable(true);
        showPosition();
    });

// ---- contents --------------------------------------------------------------

/** Every contents entry, in order, with its link. */
const tocEntries: { href: string; label: string; link: HTMLAnchorElement }[] = [];

function renderToc(items: EpubJS.NavItem[], el: HTMLElement): void {
    for (const item of items) {
        const li = document.createElement("li");
        const a = document.createElement("a");
        const label = item.label.trim();
        a.textContent = label;
        a.href = "#";
        a.addEventListener("click", (e) => {
            e.preventDefault();
            pushJump();
            void rendition.display(item.href);
            chrome.sidebar.close();
        });
        tocEntries.push({ href: item.href, label, link: a });
        li.appendChild(a);
        if (item.subitems?.length) {
            const sub = document.createElement("ul");
            renderToc(item.subitems, sub);
            li.appendChild(sub);
        }
        el.appendChild(li);
    }
}

/** The contents entry for a place in the book (the first one in its file). */
function tocEntryFor(cfi: string | null): (typeof tocEntries)[number] | undefined {
    try {
        const href = (cfi ? book.spine.get(cfi) : null)?.href || "";
        if (!href) return undefined;
        return tocEntries.find((t) => {
            const file = t.href.split("#")[0];
            return !!file && href.endsWith(file);
        });
    } catch {
        return undefined;
    }
}
const currentTocEntry = (): (typeof tocEntries)[number] | undefined => tocEntryFor(currentCfi);

/** Highlight the current chapter in the contents. */
function markCurrentChapter(): void {
    const current = currentTocEntry()?.link;
    for (const { link } of tocEntries) {
        if (link === current) link.setAttribute("aria-current", "true");
        else link.removeAttribute("aria-current");
    }
}

void book.ready.then(() => {
    renderToc(book.navigation.toc, byId("toc-list"));
    if (!tocEntries.length) byId("toc-list").replaceChildren(el("li", { className: "toc-empty" }, "This book has no contents."));
    markCurrentChapter();
    showPosition();
});

// ---- bookmarks -------------------------------------------------------------

const bookmarkHandlers: BookmarkHandlers = {
    onGo: (b) => {
        pushJump();
        void rendition.display(b.position);
        chrome.sidebar.close();
    },
    onDelete: (b) => {
        sync.removeBookmark(b.id);
        showBookmarks(sync.bookmarks());
    },
};
const showBookmarks = (items: Bookmark[]): void => renderBookmarks(byId("bookmark-list"), items, bookmarkHandlers);
void sync.loadBookmarks().then(showBookmarks);

byId("bookmark-btn").addEventListener("click", () => {
    if (!currentCfi || preview) return;
    const page = pageText(currentCfi);
    sync.addBookmark(currentCfi, `${currentTocEntry()?.label || "Bookmark"}${page ? ` — ${page.split(" of ")[0]}` : ""}`);
    showBookmarks(sync.bookmarks());
    chrome.toast("Bookmark added");
});

// ---- highlights and notes ---------------------------------------------------
// An annotation's anchor is a CFI range, which epub.js turns back into a
// DOM range whatever the font size or layout. epub.js draws the highlights
// (an SVG layer over the chapter) and puts them back when a chapter is
// rendered again.

/** A rectangle inside a chapter's iframe, in this window's coordinates. */
function toScreen(doc: Document, r: DOMRect): DOMRect {
    const frame = doc.defaultView?.frameElement?.getBoundingClientRect();
    return new DOMRect(r.left + (frame?.left ?? 0), r.top + (frame?.top ?? 0), r.width, r.height);
}

function pageNumber(cfi: string): number {
    try {
        return locationsReady ? book.locations.locationFromCfi(cfi) + 1 : 0;
    } catch {
        return 0;
    }
}

/** Highlights on the page now: id -> what was drawn. */
const drawn = new Map<string, { anchor: string; color: string }>();
function drawHighlights(): void {
    const want = new Map(store.list().map((a) => [a.id, a]));
    for (const [id, d] of drawn) {
        if (want.get(id)?.color === d.color) continue;
        rendition.annotations.remove(d.anchor, "highlight");
        drawn.delete(id);
    }
    for (const a of want.values()) {
        if (drawn.has(a.id)) continue;
        try {
            rendition.annotations.highlight(a.anchor, {}, undefined, "cf-hl", { fill: colorValue(a.color), "fill-opacity": "0.38" });
            drawn.set(a.id, { anchor: a.anchor, color: a.color });
        } catch {
            /* an anchor this copy of the book doesn't have */
        }
    }
}

/** The highlight under a point of a chapter's document, if any. */
function highlightAt(doc: Document, x: number, y: number): { annotation: Annotation; rect: DOMRect } | null {
    for (const a of store.list()) {
        let range: Range | undefined;
        try {
            range = rendition.getRange(a.anchor);
        } catch {
            continue;
        }
        if (!range || range.startContainer.ownerDocument !== doc) continue;
        for (const r of range.getClientRects()) {
            if (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) return { annotation: a, rect: range.getBoundingClientRect() };
        }
    }
    return null;
}

const store = createAnnotationStore(uuid, format, sync.user, () => {
    drawHighlights();
    notesUI.renderList();
});
const notesUI = setupAnnotationUI({
    store,
    onGo(a) {
        pushJump();
        void rendition.display(a.anchor);
        if (window.innerWidth < 700) chrome.sidebar.close();
    },
    compare: (a, b) => a.page - b.page || a.created_at - b.created_at,
    toast: chrome.toast,
    // KOReader's places in a Kindle file don't map onto the converted EPUB (convert_read.go).
    koreader: sync.signedIn && format === "epub" ? { uuid, format: "epub", sync: () => store.load() } : undefined,
});
// A preview shows no highlights and makes none (selecting text still copies).
if (!preview) void store.load();

rendition.on("selected", (cfiRange, contents) => {
    if (preview) return;
    const selection = contents.window.getSelection();
    const text = selection?.toString().trim() ?? "";
    if (!selection?.rangeCount || !text) return;
    const at = toScreen(contents.document, selection.getRangeAt(0).getBoundingClientRect());
    notesUI.offerNew(at, text, (color) => {
        const made = store.add({ anchor: cfiRange, text, color, chapter: tocEntryFor(cfiRange)?.label ?? "", page: pageNumber(cfiRange) });
        contents.window.getSelection()?.removeAllRanges();
        return made;
    });
});
rendition.on("relocated", () => notesUI.hide());

// ---- search ----------------------------------------------------------------
// Chapters are searched one at a time, in reading order, so results appear
// as they are found and a new search stops the old one.

const searchInput = maybeId("search-input", HTMLInputElement);
const searchStatus = byId("search-status");
const searchResults = byId("search-results");
const MAX_HITS = 300;
let searchRun = 0;
let shownHit: string | null = null;

function openSearch(): void {
    chrome.sidebar.show("search");
    searchInput?.focus();
    searchInput?.select();
}
maybeId("search-btn")?.addEventListener("click", openSearch);

function clearHit(): void {
    if (shownHit) rendition.annotations.remove(shownHit, "highlight");
    shownHit = null;
}

async function search(query: string): Promise<void> {
    const run = ++searchRun;
    clearHit();
    searchResults.replaceChildren();
    if (query.length < 2) {
        searchStatus.textContent = query ? "Type at least 2 characters." : "";
        return;
    }
    searchStatus.textContent = "Searching…";
    await book.ready;
    await wholeDone; // every chapter: from memory once the whole book is here
    let hits = 0;
    const count = book.spine.length ?? 0;
    for (let i = 0; i < count && hits < MAX_HITS; i++) {
        const item = book.spine.get(i);
        if (!item) continue;
        let found: { cfi: string; excerpt: string }[] = [];
        try {
            await item.load(book.load.bind(book));
            found = item.find(query);
        } catch {
            /* a chapter that can't be read has no matches */
        } finally {
            item.unload();
        }
        if (run !== searchRun) return; // a newer search took over
        for (const hit of found.slice(0, MAX_HITS - hits)) searchResults.append(hitItem(hit, query));
        hits += found.length;
        searchStatus.textContent = `Searching… ${Math.round(((i + 1) / count) * 100)}%`;
    }
    searchStatus.textContent = hits === 0 ? "No matches." : hits >= MAX_HITS ? `First ${MAX_HITS} matches.` : `${hits} match${hits === 1 ? "" : "es"}.`;
}

function hitItem(hit: { cfi: string; excerpt: string }, query: string): HTMLLIElement {
    const li = document.createElement("li");
    const a = document.createElement("a");
    a.href = "#";
    const where = document.createElement("span");
    where.className = "hit-where";
    where.textContent = [tocEntryFor(hit.cfi)?.label, pageText(hit.cfi).split(" of ")[0]].filter(Boolean).join(" · ");
    a.append(where);
    // The excerpt, with the match marked.
    const text = hit.excerpt.replace(/\s+/g, " ").trim();
    const at = text.toLowerCase().indexOf(query.toLowerCase());
    if (at < 0) a.append(text);
    else {
        const mark = document.createElement("mark");
        mark.textContent = text.slice(at, at + query.length);
        a.append(text.slice(0, at), mark, text.slice(at + query.length));
    }
    a.addEventListener("click", (e) => {
        e.preventDefault();
        pushJump();
        clearHit();
        void rendition.display(hit.cfi).then(() => {
            rendition.annotations.highlight(hit.cfi, {}, undefined, "cf-search-hit", { fill: "#f5c400", "fill-opacity": "0.35" });
            shownHit = hit.cfi;
        });
        // On a phone the sidebar covers the page.
        if (window.innerWidth < 700) chrome.sidebar.close();
    });
    li.append(a);
    return li;
}

maybeId("search-form")?.addEventListener("submit", (e) => {
    e.preventDefault();
    void search(searchInput?.value.trim() ?? "");
});
searchInput?.addEventListener("input", () => {
    if (!searchInput.value) void search("");
});
