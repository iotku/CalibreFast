// The metadata editor's ISBN tools: find ISBNs in the book's own files, and
// look the book up on Open Library and Google Books. Both only offer
// choices: a found ISBN goes into the form when the admin clicks Use, and a
// lookup's answers become sources like "current", picked field by field.
// Nothing is saved until Apply Changes.

import { api, el, errorMessage, maybeId } from "./dom.js";
import type { CoverCandidate, IsbnCandidate, IsbnResponse, LookupResult, MetaSource } from "./types.js";
import { openPdf } from "./vendor.js";

/** What the tools need from the editor. */
export interface IsbnToolsHost {
    uuid: string;
    form: HTMLFormElement;
    /** The ISBN field's value. */
    currentIsbn(): string;
    /** Title and first author in the form, to look up a book without an ISBN. */
    titleAndAuthor(): { title: string; author: string };
    /** Put an ISBN in the ISBN field. */
    useIsbn(isbn: string): void;
    /** Offer a lookup's answer as a source under every field. */
    addSource(meta: MetaSource): void;
}

/** Where an ISBN was found, in words. */
const FOUND_IN: Record<IsbnCandidate["source"], string> = {
    opf: "EPUB metadata",
    exth: "MOBI header",
    xmp: "PDF metadata",
    text: "text",
};

/** Pages of a PDF read for ISBNs: copyright pages are near either end. */
const PDF_FIRST_PAGES = 5;
const PDF_LAST_PAGES = 3;

/** An ISBN as 13 digits, so "0-13-419044-0" and "9780134190440" compare equal. */
export function isbnDigits(value: string): string {
    const d = value.replace(/[^0-9Xx]/g, "").toUpperCase();
    if (d.length !== 10) return d;
    const core = "978" + d.slice(0, 9);
    let sum = 0;
    for (let i = 0; i < 12; i++) sum += Number(core[i]) * (i % 2 ? 3 : 1);
    return core + ((10 - (sum % 10)) % 10);
}

/**
 * The PDF's XMP packet and the text of its first and last few pages, read
 * with pdf.js (Go has no reliable PDF text extraction). Only those pages are
 * fetched, with range requests.
 */
async function readPdf(uuid: string): Promise<{ xmp: string; pages: string[] }> {
    const pdf = await openPdf(`/download/${encodeURIComponent(uuid)}/pdf`, { disableAutoFetch: true, disableStream: true });
    try {
        let xmp = "";
        try {
            xmp = (await pdf.getMetadata()).metadata?.getRaw() ?? "";
        } catch {
            /* no usable XMP: the pages may still have an ISBN */
        }
        const pages: string[] = [];
        for (let n = 1; n <= pdf.numPages; n++) {
            if (n > PDF_FIRST_PAGES && n <= pdf.numPages - PDF_LAST_PAGES) continue;
            const content = await (await pdf.getPage(n)).getTextContent();
            let text = "";
            for (const item of content.items) {
                if ("str" in item) text += item.str + (item.hasEOL ? "\n" : "");
            }
            pages.push(text);
        }
        return { xmp, pages };
    } finally {
        void pdf.destroy();
    }
}

export function mountIsbnTools(host: IsbnToolsHost): void {
    const findEl = maybeId("btn-find-isbns", HTMLButtonElement);
    const panelEl = maybeId("isbn-panel");
    const statusEl = maybeId("isbn-status");
    const listEl = maybeId("isbn-candidates");
    if (!findEl || !panelEl || !statusEl || !listEl) return;
    // Non-null from here on (also inside the functions below).
    const findBtn: HTMLButtonElement = findEl;
    const panel: HTMLElement = panelEl;
    const status: HTMLElement = statusEl;
    const list: HTMLElement = listEl;
    const lookupBtn = maybeId("btn-lookup", HTMLButtonElement); // absent with -no-lookups
    const lookupStatus = maybeId("lookup-status");

    const say = (target: HTMLElement, text: string, isError = false): void => {
        target.hidden = false;
        target.textContent = text;
        target.classList.toggle("error", isError);
    };

    // ---- Finding ISBNs in the book's files --------------------------------

    let candidates: IsbnCandidate[] = [];

    const render = (): void => {
        const inField = isbnDigits(host.currentIsbn());
        list.replaceChildren(
            ...candidates.map((c) => {
                const actions = el("span", { className: "isbn-actions" });
                if (inField === c.isbn13) {
                    actions.append(el("span", { className: "isbn-in-use" }, "in the ISBN field"));
                } else {
                    const use = el("button", { type: "button", className: "meta-source-btn", title: "Put this ISBN in the ISBN field" }, "Use");
                    use.addEventListener("click", () => {
                        host.useIsbn(c.isbn13);
                        render();
                    });
                    actions.append(use);
                }
                if (lookupBtn) {
                    const look = el("button", { type: "button", className: "meta-source-btn", title: "Look this ISBN up on Open Library and Google Books" }, "Look up");
                    look.addEventListener("click", () => void lookUp({ isbn: c.isbn13 }));
                    actions.append(look);
                }
                return el(
                    "li",
                    { className: "isbn-candidate" },
                    el("code", { className: "isbn-value" }, c.isbn13),
                    c.label ? el("span", { className: "isbn-label" }, c.label) : "",
                    el("span", { className: "isbn-where", title: c.file }, `${FOUND_IN[c.source] ?? c.source} · ${c.file}`),
                    actions,
                );
            }),
        );
    };

    // "in the ISBN field" follows the field, however it changes.
    let frame = 0;
    const scheduleRender = (): void => {
        if (frame || !candidates.length) return;
        frame = requestAnimationFrame(() => {
            frame = 0;
            render();
        });
    };
    host.form.addEventListener("input", scheduleRender);
    host.form.addEventListener("click", scheduleRender);

    const show = (res: IsbnResponse, note = ""): void => {
        candidates = res.candidates;
        render();
        const n = candidates.length;
        const found = n === 0
            ? "No ISBNs found in the book's files."
            : `Found ${n} ISBN${n === 1 ? "" : "s"} in the book's files${n > 1 ? ", ebook editions first" : ""}.`;
        say(status, note ? `${found} ${note}` : found);
    };

    let finding = false;
    async function find(): Promise<void> {
        if (finding) return;
        finding = true;
        findBtn.disabled = true;
        panel.hidden = false;
        say(status, "Looking through the book's files…");
        const url = `/metadata/${encodeURIComponent(host.uuid)}/isbns`;
        try {
            let res = await api<IsbnResponse>("GET", url);
            if (res.pdf) {
                show(res, `Reading ${res.pdf}…`);
                try {
                    res = await api<IsbnResponse>("POST", url, await readPdf(host.uuid));
                    show(res);
                } catch (err) {
                    show(res, `Couldn't read ${res.pdf}: ${errorMessage(err)}`);
                }
            } else {
                show(res);
            }
        } catch (err) {
            say(status, `Couldn't search the book's files: ${errorMessage(err)}`, true);
        } finally {
            finding = false;
            findBtn.disabled = false;
        }
    }
    findBtn.addEventListener("click", () => void find());

    // ---- Looking the book up online ---------------------------------------

    async function lookUp(q: { isbn: string } | { title: string; author: string }): Promise<void> {
        if (!lookupBtn || !lookupStatus) return;
        say(lookupStatus, "isbn" in q
            ? `Looking up ${q.isbn} on Open Library and Google Books…`
            : `Searching Open Library and Google Books for “${q.title}”…`);
        lookupBtn.disabled = true;
        try {
            const results = await api<LookupResult[]>("GET", `/metadata/lookup?${new URLSearchParams(q)}`);
            const added: string[] = [];
            const notes: string[] = [];
            const covers: CoverCandidate[] = [];
            for (const r of results) {
                if (r.option) {
                    host.addSource({ ...r.option, External: true });
                    added.push(r.source);
                } else {
                    notes.push(`${r.source}: ${r.error || "no match"}`);
                }
                if (r.cover) covers.push({ source: r.source, url: r.cover });
            }
            let msg = added.length
                ? `Added ${added.join(" and ")} as ${added.length > 1 ? "sources" : "a source"}: pick values with the pills under each field.`
                : "No match.";
            if (covers.length) msg += " Covers are in the cover picker.";
            if (notes.length) msg += ` (${notes.join("; ")})`;
            say(lookupStatus, msg);
            if (covers.length) document.dispatchEvent(new CustomEvent<CoverCandidate[]>("cf:cover-candidates", { detail: covers }));
        } catch (err) {
            say(lookupStatus, `Lookup failed: ${errorMessage(err)}`, true);
        } finally {
            lookupBtn.disabled = false;
        }
    }

    lookupBtn?.addEventListener("click", () => {
        const isbn = host.currentIsbn();
        if (isbn) return void lookUp({ isbn });
        const { title, author } = host.titleAndAuthor();
        if (!title) {
            if (lookupStatus) say(lookupStatus, "Enter an ISBN or a title to look up.", true);
            return;
        }
        void lookUp({ title, author });
    });

    // A book without an ISBN gets a search right away (its files only:
    // nothing leaves the server without a click).
    if (!host.currentIsbn()) void find();
}
