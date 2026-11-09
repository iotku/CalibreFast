// The metadata editor's ISBN tools: find ISBNs in the book's own files, and
// look the book up online. Both only offer choices: a found ISBN goes into
// the form when the admin clicks Use, and a lookup's answer becomes a source
// like "current", picked field by field or all at once. Nothing is saved
// until Apply.
//
// Lookups are kept to a minimum: one source per request, only on a click.
// Open Library first; Wikidata only if Open Library doesn't know the ISBN;
// other sources only when the admin clicks "Try …".

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
    /** What taking all of meta would change, for the lookup dialog. */
    preview(meta: MetaSource): HTMLElement[];
    /** Take every value meta has, then apply. */
    acceptAll(meta: MetaSource): void;
}

/** Where an ISBN was found, in words. */
const FOUND_IN: Record<IsbnCandidate["source"], string> = {
    opf: "EPUB metadata",
    exth: "MOBI header",
    xmp: "PDF metadata",
    text: "text",
};

/** Lookup sources (Go: lookupSources in lookup.go). */
type Source = "openlibrary" | "wikidata" | "google";
const SOURCE_NAMES: Record<Source, string> = { openlibrary: "Open Library", wikidata: "Wikidata", google: "Google Books" };

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
    const lookups = mountLookup(host); // null with -no-lookups

    const say = (text: string, isError = false): void => {
        status.textContent = text;
        status.classList.toggle("error", isError);
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
                if (lookups) {
                    const look = el("button", { type: "button", className: "meta-source-btn", title: "Look this ISBN up online" }, "Look up");
                    look.addEventListener("click", () => lookups.lookUpIsbn(c.isbn13));
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
        say(note ? `${found} ${note}` : found);
        if (candidates[0]) lookups?.suggest(candidates[0].isbn13);
    };

    let finding = false;
    async function find(): Promise<void> {
        if (finding) return;
        finding = true;
        findBtn.disabled = true;
        panel.hidden = false;
        say("Looking through the book's files…");
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
            say(`Couldn't search the book's files: ${errorMessage(err)}`, true);
        } finally {
            finding = false;
            findBtn.disabled = false;
        }
    }
    findBtn.addEventListener("click", () => void find());

    // A book without an ISBN gets a search of its own files right away.
    // That never contacts another site: only the Look up buttons do.
    if (!host.currentIsbn()) void find();
}

/**
 * The top bar's ISBN lookup, the status bar under the banner and the lookup
 * dialog. Returns null when lookups are off (-no-lookups).
 */
function mountLookup(host: IsbnToolsHost): { lookUpIsbn(isbn: string): void; suggest(isbn: string): void } | null {
    const formEl = maybeId("isbn-lookup", HTMLFormElement);
    const inputEl = maybeId("isbn-lookup-input", HTMLInputElement);
    const barEl = maybeId("lookup-bar");
    const barStatusEl = maybeId("lookup-status");
    const barActionsEl = maybeId("lookup-actions");
    const dialogEl = maybeId("lookup-dialog", HTMLDialogElement);
    if (!formEl || !inputEl || !barEl || !barStatusEl || !barActionsEl || !dialogEl) return null;
    const lookupForm: HTMLFormElement = formEl;
    const input: HTMLInputElement = inputEl;
    const bar: HTMLElement = barEl;
    const barStatus: HTMLElement = barStatusEl;
    const barActions: HTMLElement = barActionsEl;
    const dialog: HTMLDialogElement = dialogEl;
    const submitBtn = lookupForm.querySelector<HTMLButtonElement>("button[type=submit]");
    const googleAllowed = lookupForm.dataset["google"] === "1";

    input.value = host.currentIsbn();

    // ---- The dialog: a source vs the current metadata ----------------------

    let shown: MetaSource | null = null;
    function openDialog(meta: MetaSource): void {
        shown = meta;
        const title = maybeId("lookup-title");
        if (title) title.textContent = `${meta.Format} vs current metadata`;
        maybeId("lookup-body")?.replaceChildren(...host.preview(meta));
        if (!dialog.open) dialog.showModal();
    }
    maybeId("lookup-accept", HTMLButtonElement)?.addEventListener("click", () => {
        dialog.close();
        if (shown) host.acceptAll(shown);
    });
    // Click outside the dialog box (on the backdrop) closes it.
    dialog.addEventListener("click", (e) => {
        const r = dialog.getBoundingClientRect();
        if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) dialog.close();
    });

    // ---- Asking the sources ------------------------------------------------

    type Query = { isbn: string } | { title: string; author: string };
    let query: Query | null = null;
    let tried = new Set<Source>();
    const found = new Map<Source, MetaSource>();
    let notes: string[] = [];
    let hasCover = false;
    let busy = false;

    /** Asks one source; true if it knew the book. */
    async function ask(source: Source): Promise<boolean> {
        if (!query) return false;
        tried.add(source);
        const r = await api<LookupResult>("GET", `/metadata/lookup?${new URLSearchParams({ source, ...query })}`);
        if (!r.option) {
            notes.push(`${SOURCE_NAMES[source]}: ${r.error || "no match"}`);
            return false;
        }
        const meta: MetaSource = { ...r.option, External: true };
        host.addSource(meta);
        found.set(source, meta);
        if (r.cover) {
            document.dispatchEvent(new CustomEvent<CoverCandidate[]>("cf:cover-candidates", { detail: [{ source: r.source, url: r.cover }] }));
            hasCover = true;
        }
        return true;
    }

    /** What's been found so far, in words. */
    function summary(): string {
        if (!found.size) return "No match.";
        const names = [...found.keys()].map((s) => SOURCE_NAMES[s]).join(" and ");
        return `Found on ${names}: the values are under each field` +
            (hasCover ? ", and Open Library's cover is in the cover picker (click the book's cover)." : ".");
    }

    /** Shows text (by default, what's been found), with the sources still to try. */
    function renderBar(text = summary(), isError = false): void {
        bar.hidden = false;
        barStatus.textContent = notes.length ? `${text} (${notes.join("; ")})` : text;
        barStatus.classList.toggle("error", isError);
        const buttons: HTMLButtonElement[] = [];
        for (const [source, meta] of found) {
            const b = el("button", { type: "button", className: "meta-source-btn" }, `Compare ${SOURCE_NAMES[source]}`);
            b.addEventListener("click", () => openDialog(meta));
            buttons.push(b);
        }
        const others: Source[] = [];
        if (query && "isbn" in query) others.push("wikidata");
        if (googleAllowed) others.push("google");
        for (const source of others) {
            if (tried.has(source)) continue;
            const b = el("button", { type: "button", className: "meta-source-btn" }, `Try ${SOURCE_NAMES[source]}`);
            b.title = source === "google"
                ? "Sends the ISBN (or title and author) to Google. Google Books often has a description."
                : "Sends the ISBN to Wikidata";
            b.addEventListener("click", () => void run(async () => {
                renderBar(`Asking ${SOURCE_NAMES[source]}…`);
                const hit = await ask(source);
                renderBar();
                if (hit) openDialog(found.get(source)!);
            }));
            buttons.push(b);
        }
        barActions.replaceChildren(...buttons);
    }

    /** One lookup at a time; errors go to the bar. */
    async function run(work: () => Promise<void>): Promise<void> {
        if (busy) return;
        busy = true;
        if (submitBtn) submitBtn.disabled = true;
        try {
            await work();
        } catch (err) {
            renderBar(`Lookup failed: ${errorMessage(err)}`, true);
        } finally {
            busy = false;
            if (submitBtn) submitBtn.disabled = false;
        }
    }

    /** Open Library, then Wikidata if Open Library doesn't know the ISBN. */
    function lookUp(q: Query): void {
        void run(async () => {
            query = q;
            tried = new Set();
            found.clear();
            notes = [];
            hasCover = false;
            renderBar("isbn" in q ? `Looking up ${q.isbn} on Open Library…` : `Searching Open Library for “${q.title}”…`);
            let hit: Source | null = (await ask("openlibrary")) ? "openlibrary" : null;
            if (!hit && "isbn" in q) {
                renderBar("Not on Open Library; asking Wikidata…");
                if (await ask("wikidata")) hit = "wikidata";
            }
            renderBar();
            if (hit) openDialog(found.get(hit)!);
        });
    }

    lookupForm.addEventListener("submit", (e) => {
        e.preventDefault();
        const isbn = input.value.trim();
        if (isbn) return lookUp({ isbn });
        const { title, author } = host.titleAndAuthor();
        if (title) return lookUp({ title, author });
        renderBar("Enter an ISBN to look up.", true);
    });

    return {
        lookUpIsbn(isbn) {
            input.value = isbn;
            lookUp({ isbn });
        },
        /** A found ISBN fills the box if it's empty. */
        suggest(isbn) {
            if (!input.value.trim()) input.value = isbn;
        },
    };
}
