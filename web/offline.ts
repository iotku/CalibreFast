/**
 * The offline Reading page (templates/offline.html), shown by the service
 * worker (sw.ts) for any page when the server can't be reached:
 *
 *   - the Reading list as last seen online (offline-books.ts), each book
 *     with its cover and how far along: those kept on this device
 *     (book-cache.ts) open in the reader for the format kept (the one last
 *     read, when both are); the others are placeholders,
 *     until the connection is back;
 *   - then the books kept here that aren't on that list.
 *
 * It keeps asking the server; once it answers, the page asked for is
 * loaded (the full page, with everything that needs the server).
 */

import { listBooks, readerUrl, type CachedBook, type KeptFormat } from "./book-cache.js";
import { byId, el } from "./dom.js";
import { preferredFormat } from "./last-format.js";
import { keptCover, readingSnapshot, type ListedBook } from "./offline-books.js";
import { coverHue } from "./utils.js";

/** How often to ask whether the server is back. */
const PROBE_MS = 15_000;

/** How far along each book is, as the library's marks last had it (reading.ts). */
function progress(): Record<string, number | null> {
    try {
        return (JSON.parse(localStorage.getItem("cf-reading") ?? "null") as { reading?: Record<string, number | null> } | null)?.reading ?? {};
    } catch {
        return {};
    }
}

interface Card {
    uuid: string;
    title: string;
    authors: string;
    /** The reader for the copy kept here; null when there's none. */
    href: string | null;
    percent: number | null | undefined;
}

/** A card like the library's (.book): a link to the reader when the book is kept, else a placeholder. */
function card(c: Card): HTMLElement {
    const wrapper = el("div", { className: "cover-wrapper" });
    wrapper.style.setProperty("--cover-hue", String(coverHue(c)));
    const info = el(
        "div",
        { className: "book-info" },
        el("h2", { textContent: c.title || "Untitled" }),
        el("div", { className: "book-author", textContent: c.authors || "Unknown Author" }),
    );
    const inner = c.href
        ? el("a", { className: "book-link", href: c.href, ariaLabel: `Read ${c.title}` }, wrapper)
        : el("div", { className: "book-link" }, wrapper);
    const book = el("div", { className: `book${c.href ? "" : " offline-unavailable"}` }, inner, info);
    book.dataset["uuid"] = c.uuid;
    if (!c.href) {
        book.setAttribute("aria-disabled", "true");
        book.append(el("span", { className: "offline-badge", textContent: "Not on this device" }));
    }
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
    const listed: ListedBook[] = readingSnapshot()?.books ?? [];
    const onList = new Set(listed.map((b) => b.uuid));

    const reading = listed.map((b) => card({ ...b, href: hrefOf(b.uuid), percent: marks[b.uuid] }));
    // Books kept here but not on the list: most recently opened first.
    const others = kept
        .filter((b) => !onList.has(b.uuid))
        .sort((a, b) => b.lastOpened - a.lastOpened)
        .map((b) => card({ uuid: b.uuid, title: b.title, authors: b.authors, href: hrefOf(b.uuid), percent: marks[b.uuid] }));

    // Kept books first on the Reading list: they're the ones that open.
    const unavailable = (e: HTMLElement): number => Number(e.classList.contains("offline-unavailable"));
    reading.sort((a, b) => unavailable(a) - unavailable(b)); // stable: the list's order otherwise
    byId("offline-reading-books").replaceChildren(...reading);
    byId("offline-kept-books").replaceChildren(...others);
    byId("offline-reading").hidden = reading.length === 0;
    byId("offline-kept").hidden = others.length === 0;
    byId("offline-none").hidden = kept.length > 0;
}

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
        // /offline itself is a real page: go to the Reading page instead.
        if (location.pathname === "/offline") location.replace("/me/reading");
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
