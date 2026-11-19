/**
 * The offline Reading page (templates/offline.html), shown by the service
 * worker (sw.ts) for any page when the server can't be reached:
 *
 *   - the Reading list as last seen online (offline-books.ts), each book
 *     with its cover and how far along;
 *   - On deck / Saved for later, as last fetched for the books downloaded
 *     ahead of time (retention.ts, savedList), as Discover's row has them;
 *   - then the books kept here on neither list.
 *
 * Books kept on this device (book-cache.ts) open in the reader for the
 * format kept (the one last read, when both are). The others on a list fold
 * away under a summary, so a long list doesn't push the readable books
 * down; opened, one shows a popup that waits for the server.
 *
 * It keeps asking the server; once it answers, the page asked for is
 * loaded (the full page, with everything that needs the server), with the
 * book waited for, if any, in its popup (#book/<uuid>, modal.ts).
 */

import { KEPT_FORMATS, currentUser, listBooks, readerUrl, type CachedBook, type KeptFormat } from "./book-cache.js";
import { byId, el } from "./dom.js";
import { preferredFormat } from "./last-format.js";
import { lastKnownUser, localKey } from "./local-keys.js";
import { keptCover, readingSnapshot, type ListedBook } from "./offline-books.js";
import { savedList } from "./retention.js";
import { coverHue } from "./utils.js";

/** How often to ask whether the server is back; more often while a book waits for it. */
const PROBE_MS = 15_000;
const WAIT_PROBE_MS = 4_000;

/** How far along each book is, as the library's marks last had it (reading.ts). */
function progress(): Record<string, number | null> {
    try {
        return (JSON.parse(localStorage.getItem("cf-reading") ?? "null") as { reading?: Record<string, number | null> } | null)?.reading ?? {};
    } catch {
        return {};
    }
}

/**
 * How far along a book is: the newest position saved on this device (in
 * either format, reading-sync.ts), else the library's marks. Pages read just
 * before going offline count, though the marks predate them.
 */
function percentOf(uuid: string, marks: Record<string, number | null>): number | null | undefined {
    const owner = lastKnownUser();
    let newest: { percent?: number; updatedAt?: number } | null = null;
    for (const f of KEPT_FORMATS) {
        try {
            const p = JSON.parse(localStorage.getItem(localKey(owner, `progress-${uuid}-${f}`)) ?? "null") as { percent?: number; updatedAt?: number } | null;
            if (p && (p.updatedAt ?? 0) > (newest?.updatedAt ?? -1)) newest = p;
        } catch {
            /* unreadable: skip */
        }
    }
    return newest?.percent ?? marks[uuid];
}

interface Card {
    uuid: string;
    title: string;
    authors: string;
    /** The reader for the copy kept here; null when there's none. */
    href: string | null;
    percent: number | null | undefined;
}

/** A card like the library's (.book): a link to the reader when the book is kept, else to the popup that waits for the server. */
function card(c: Card): HTMLElement {
    const wrapper = el("div", { className: "cover-wrapper" });
    wrapper.style.setProperty("--cover-hue", String(coverHue(c)));
    const info = el(
        "div",
        { className: "book-info" },
        el("h2", { textContent: c.title || "Untitled" }),
        el("div", { className: "book-author", textContent: c.authors || "Unknown Author" }),
    );
    const title = c.title || "Untitled";
    const inner = c.href
        ? el("a", { className: "book-link", href: c.href, ariaLabel: `Read ${title}` }, wrapper)
        : el("a", { className: "book-link", href: `#book/${encodeURIComponent(c.uuid)}`, ariaLabel: `${title}: not on this device` }, wrapper);
    if (!c.href)
        inner.addEventListener("click", (e) => {
            e.preventDefault();
            openWait(c.uuid, title, inner);
        });
    const book = el("div", { className: `book${c.href ? "" : " offline-unavailable"}` }, inner, info);
    book.dataset["uuid"] = c.uuid;
    if (!c.href) book.append(el("span", { className: "offline-badge", textContent: "Not on this device" }));
    if (c.percent !== undefined) {
        book.classList.add("is-reading");
        const bar = el("span");
        bar.style.width = `${Math.round((c.percent ?? 0) * 100)}%`;
        book.append(el("div", { className: "book-progress" }, bar));
    }
    void keptCover(c.uuid).then((src) => {
        if (!src) return;
        const img = el("img", { className: "book-cover", alt: "" });
        img.addEventListener("load", () => img.classList.add("loaded"), { once: true });
        img.src = src;
        wrapper.append(img);
    });
    return book;
}

async function render(): Promise<void> {
    const files = await listBooks().catch((): CachedBook[] => []);
    // A book's kept files (an EPUB, a PDF or both), as one book: the one opened last.
    const keptBy = new Map<string, { book: CachedBook; formats: KeptFormat[] }>();
    for (const f of files) {
        const k = keptBy.get(f.uuid);
        if (!k) keptBy.set(f.uuid, { book: f, formats: [f.format ?? "epub"] });
        else {
            k.formats.push(f.format ?? "epub");
            if (f.lastOpened > k.book.lastOpened) k.book = f;
        }
    }
    const hrefOf = (uuid: string): string | null => {
        const k = keptBy.get(uuid);
        if (!k) return null;
        return readerUrl(uuid, preferredFormat(uuid, k.formats) ?? k.book.format ?? "epub");
    };
    const kept = [...keptBy.values()].map((k) => k.book);
    const marks = progress();
    const cardOf = (b: { uuid: string; title: string; authors: string }): HTMLElement => {
        // A book a reader added to the list (offline-books.ts, noteReadingHere)
        // has only its title: the kept copy has the rest.
        const k = keptBy.get(b.uuid)?.book;
        return card({
            uuid: b.uuid,
            title: b.title || k?.title || "",
            authors: b.authors || k?.authors || "",
            href: hrefOf(b.uuid),
            percent: percentOf(b.uuid, marks),
        });
    };

    const snap = readingSnapshot();
    const listed: ListedBook[] = snap?.books ?? [];
    const shown = new Set(listed.map((b) => b.uuid));
    // On deck / Saved as saved for this page (offline-books.ts); until it is, the books downloaded ahead of time.
    const saved = (snap?.saved ?? savedList(currentUser())?.books ?? []).filter((b) => !shown.has(b.uuid));
    for (const b of saved) shown.add(b.uuid);
    const reading = fill("reading", listed, keptBy, cardOf);
    const onDeck = fill("saved", saved, keptBy, cardOf);

    // Books kept here on neither list: most recently opened first.
    const others = kept
        .filter((b) => !shown.has(b.uuid))
        .sort((a, b) => b.lastOpened - a.lastOpened)
        .map(cardOf);
    byId("offline-kept-books").replaceChildren(...others);
    byId("offline-kept").hidden = others.length === 0;
    byId("offline-none").hidden = kept.length > 0 || reading + onDeck > 0;
}

/**
 * Fill a list's section: its books kept here, in the list's order, then the
 * rest folded away under a summary ("3 more not on this device"). Returns
 * how many books it lists.
 */
function fill(
    id: "reading" | "saved",
    books: { uuid: string; title: string; authors: string }[],
    keptBy: Map<string, unknown>,
    cardOf: (b: { uuid: string; title: string; authors: string }) => HTMLElement,
): number {
    const here = books.filter((b) => keptBy.has(b.uuid));
    const away = books.filter((b) => !keptBy.has(b.uuid));
    byId(`offline-${id}-books`).replaceChildren(...here.map(cardOf));
    const more = byId(`offline-${id}-more`, HTMLDetailsElement);
    const n = away.length;
    more.querySelector("summary")!.textContent = `${n} ${here.length ? "more" : n === 1 ? "book" : "books"} not on this device`;
    more.querySelector(".offline-grid")!.replaceChildren(...away.map(cardOf));
    more.hidden = n === 0;
    byId(`offline-${id}`).hidden = books.length === 0;
    return books.length;
}

/* A book not kept here: a popup that waits for the server. */

const modal = byId("book-modal");
const waitTitle = byId("offline-wait-title");
/** The book waited for, and what to focus again once its popup closes. */
let waiting: { uuid: string; back: HTMLElement } | null = null;
let waitTimer: ReturnType<typeof setInterval> | undefined;

/** The page's address, with the book asked for as the popup to open (#book/<uuid>, modal.ts). */
function setHash(uuid: string | null): void {
    const url = new URL(location.href);
    url.hash = uuid ? `book/${encodeURIComponent(uuid)}` : "";
    history.replaceState(history.state, "", url.hash ? url.href : url.pathname + url.search);
}

function openWait(uuid: string, title: string, back: HTMLElement): void {
    waiting = { uuid, back };
    waitTitle.textContent = title;
    setHash(uuid); // a reload once the server answers opens it
    modal.classList.add("open");
    document.body.classList.add("no-scroll");
    byId("modal-close").focus();
    clearInterval(waitTimer);
    waitTimer = setInterval(() => void check(), WAIT_PROBE_MS);
    void check();
}

function closeWait(): void {
    if (!waiting) return;
    const back = waiting.back;
    waiting = null;
    clearInterval(waitTimer);
    setHash(null);
    modal.classList.remove("open");
    document.body.classList.remove("no-scroll");
    back.focus();
}

modal.addEventListener("click", (e) => {
    if (e.target === modal || (e.target instanceof Element && e.target.closest("#modal-close"))) closeWait();
});
document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeWait();
});

/* Back online: load the page asked for (this one is only its stand-in). */

const statusText = byId("offline-status-text");
const retry = byId("offline-retry", HTMLButtonElement);
let checking = false;

async function serverAnswers(): Promise<boolean> {
    try {
        const res = await fetch("/api/me", { cache: "no-store", credentials: "same-origin" });
        return res.ok;
    } catch {
        return false;
    }
}

async function check(byHand = false): Promise<void> {
    if (checking) return;
    checking = true;
    retry.disabled = true;
    if (byHand) statusText.textContent = "Checking the connection…";
    const back = await serverAnswers();
    checking = false;
    retry.disabled = false;
    if (back) {
        statusText.textContent = "Back online. Loading…";
        // /offline itself is a real page: go to the Reading page instead
        // (with the book waited for, if any).
        if (location.pathname === "/offline") location.replace("/me/reading" + location.hash);
        else location.reload();
    } else if (byHand) {
        statusText.textContent = "Still offline. Books on this device can be read.";
    }
}

retry.addEventListener("click", () => void check(true));
window.addEventListener("online", () => void check());
document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void check();
});
setInterval(() => {
    if (document.visibilityState === "visible") void check();
}, PROBE_MS);

await render();
