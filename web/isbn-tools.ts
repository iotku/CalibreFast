// The metadata editor's ISBN tools and online lookup: find ISBNs in the
// book's own files, and look the book up on Open Library. Both only offer
// choices: a found ISBN goes into the form when the admin clicks Use, and
// the matches the admin ticks become sources like
// "current", picked field by field or all at once. Nothing is saved until
// Apply.
//
// Lookups are kept to a minimum: one source per request, only on a click.
// Open Library by the ISBN in the form when there is one (the best match),
// else by title and author; by title and author too when the ISBN finds
// nothing, or when the admin asks. Google Books only on "Try Google Books".

import type { BookTarget } from "./book-target.js";
import { api, closeOnBackdropClick, el, errorMessage, maybeId } from "./dom.js";
import type { CoverCandidate, IsbnCandidate, IsbnResponse, LookupResult, MetaSource } from "./types.js";
import { openPdf } from "./vendor.js";

/** What the tools need from the editor. */
export interface IsbnToolsHost {
    book: BookTarget;
    form: HTMLFormElement;
    /** The ISBN field's value. */
    currentIsbn(): string;
    /** Title and first author in the form, to look up a book without an ISBN. */
    titleAndAuthor(): { title: string; author: string };
    /** Put an ISBN in the ISBN field. */
    useIsbn(isbn: string): void;
    /** Offer a lookup's match as a source under every field (by its Format). */
    addSource(meta: MetaSource): void;
    /** Take that source's pills away again. */
    removeSource(format: string): void;
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
type Source = "openlibrary" | "google";
const SOURCE_NAMES: Record<Source, string> = { openlibrary: "Open Library", google: "Google Books" };

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
async function readPdf(book: BookTarget): Promise<{ xmp: string; pages: string[] }> {
    const pdf = await openPdf(book.file("pdf"), { disableAutoFetch: true, disableStream: true });
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
    const refreshEl = maybeId("btn-refresh-isbns", HTMLButtonElement);
    const panelEl = maybeId("isbn-panel");
    const statusEl = maybeId("isbn-status");
    const listEl = maybeId("isbn-candidates");
    if (!findEl || !refreshEl || !panelEl || !statusEl || !listEl) return;
    // Non-null from here on (also inside the functions below).
    const findBtn: HTMLButtonElement = findEl;
    const refreshBtn: HTMLButtonElement = refreshEl;
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
    };

    let finding = false;
    async function find(): Promise<void> {
        if (finding) return;
        finding = true;
        refreshBtn.disabled = true;
        findBtn.hidden = true; // the results have Refresh
        panel.hidden = false;
        say("Looking through the book's files…");
        const url = host.book.isbns;
        try {
            let res = await api<IsbnResponse>("GET", url);
            if (res.pdf) {
                show(res, `Reading ${res.pdf}…`);
                try {
                    res = await api<IsbnResponse>("POST", url, await readPdf(host.book));
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
            refreshBtn.disabled = false;
        }
    }
    findBtn.addEventListener("click", () => void find());
    refreshBtn.addEventListener("click", () => void find());

    // The ISBN field's own Look up button, shown while the field has an ISBN.
    const lookupIsbnBtn = maybeId("btn-lookup-isbn", HTMLButtonElement);
    if (lookupIsbnBtn && lookups) {
        const sync = (): void => {
            lookupIsbnBtn.hidden = !host.currentIsbn();
        };
        sync();
        // The field changes by typing, and by clicks anywhere: pills, Use,
        // Undo, the lookup dialog's Accept all.
        document.addEventListener("input", sync);
        document.addEventListener("click", () => requestAnimationFrame(sync));
        lookupIsbnBtn.addEventListener("click", () => {
            const isbn = host.currentIsbn();
            if (isbn) lookups.lookUpIsbn(isbn);
        });
    }

    // A book without an ISBN gets a search of its own files right away;
    // one with an ISBN only on a click. Neither contacts another site: only
    // the Look up buttons do.
    if (!host.currentIsbn()) void find();
}

/**
 * The lookup bar at the top of the editor, its list of matches and the
 * lookup dialog. Returns null when lookups are off (-no-lookups).
 */
function mountLookup(host: IsbnToolsHost): { lookUpIsbn(isbn: string): void } | null {
    const barEl = maybeId("lookup-bar");
    const statusEl = maybeId("lookup-status");
    const searchEl = maybeId("lookup-search", HTMLButtonElement);
    const moreEl = maybeId("lookup-more");
    const listEl = maybeId("lookup-results");
    const dialogEl = maybeId("lookup-dialog", HTMLDialogElement);
    if (!barEl || !statusEl || !searchEl || !moreEl || !listEl || !dialogEl) return null;
    const bar: HTMLElement = barEl;
    const status: HTMLElement = statusEl;
    const searchBtn: HTMLButtonElement = searchEl;
    const more: HTMLElement = moreEl;
    const list: HTMLElement = listEl;
    const dialog: HTMLDialogElement = dialogEl;
    const googleAllowed = bar.dataset["google"] === "1";

    // ---- Queries -----------------------------------------------------------

    type Query = { isbn: string } | { title: string; author: string };

    /** The ISBN in the form, if it's a whole one. */
    const formIsbn = (): string => {
        const isbn = host.currentIsbn();
        return isbnDigits(isbn).length === 13 ? isbn : "";
    };
    const byTitle = (): Query | null => {
        const { title, author } = host.titleAndAuthor();
        return title ? { title, author } : null;
    };
    /** The best query: the ISBN (one edition, the likeliest match), else title and author. */
    const bestQuery = (): Query | null => {
        const isbn = formIsbn();
        return isbn ? { isbn } : byTitle();
    };
    const describe = (q: Query): string =>
        "isbn" in q ? `ISBN ${q.isbn}` : `“${q.title}”${q.author ? ` by ${q.author}` : ""}`;

    // ---- Matches -------------------------------------------------------------

    interface Match {
        key: string;
        source: Source;
        meta: MetaSource;
        cover: string;
        /** Its pills' name while it's a source in the form ("Open Library 2"); null when not. */
        label: string | null;
    }

    /** What the list shows: the ticked matches first (they outlive a new search), then the last search's. */
    let matches: Match[] = [];
    const picked = (): Match[] => matches.filter((m) => m.label);

    const matchKey = (source: Source, meta: MetaSource): string => {
        const id = meta.Identifiers?.find((i) => ["olid", "google", "isbn"].includes(i.Scheme.toLowerCase()));
        return `${source}:${id ? `${id.Scheme}:${id.Value}` : `${meta.Title}\0${meta.Creators?.join(",") ?? ""}`}`.toLowerCase();
    };

    /** The lowest free name for a source: "Open Library", then "Open Library 2", … */
    function freeLabel(source: Source): string {
        const taken = new Set(picked().map((m) => m.label));
        for (let n = 1; ; n++) {
            const label = n === 1 ? SOURCE_NAMES[source] : `${SOURCE_NAMES[source]} ${n}`;
            if (!taken.has(label)) return label;
        }
    }

    const asSource = (m: Match): MetaSource => ({ ...m.meta, Format: m.label ?? SOURCE_NAMES[m.source], External: true });

    /** Tells the cover dialog about the ticked matches' covers. */
    function announceCovers(): void {
        const covers = picked().flatMap((m) => (m.cover ? [{ source: m.label ?? "", url: m.cover }] : []));
        document.dispatchEvent(new CustomEvent<CoverCandidate[]>("cf:cover-candidates", { detail: covers }));
    }

    /** Ticks a match: its values become pills under each field. */
    function pick(m: Match): void {
        if (m.label) return;
        m.label = freeLabel(m.source);
        host.addSource(asSource(m));
        announceCovers();
        renderList();
    }

    function unpick(m: Match): void {
        if (!m.label) return;
        host.removeSource(m.label);
        m.label = null;
        announceCovers();
        renderList();
    }

    // ---- The bar -----------------------------------------------------------

    let busy = false;
    let lastQuery: Query | null = null;
    /** Sources asked for the last query, so "Try …" isn't offered twice. */
    let asked = new Set<Source>();
    /** What the last search found, in words; empty before the first. */
    let found = "";

    const say = (text: string, isError = false): void => {
        status.textContent = text;
        status.classList.toggle("error", isError);
    };

    /** Before a search: what Search would send. After: what was found. */
    function renderStatus(): void {
        if (busy) return;
        if (found) return say(found);
        const q = bestQuery();
        say(q
            ? `Fill in this book's details from Open Library: search by ${describe(q)}.`
            : "Fill in this book's details from Open Library: add an ISBN or a title, then search.");
    }

    function renderActions(): void {
        searchBtn.disabled = busy;
        const q = bestQuery();
        searchBtn.title = q ? `Sends ${describe(q)} to Open Library` : "Needs an ISBN or a title";
        const buttons: HTMLButtonElement[] = [];
        const add = (label: string, title: string, run: () => void): void => {
            const b = el("button", { type: "button", className: "meta-source-btn", title, disabled: busy }, label);
            b.addEventListener("click", run);
            buttons.push(b);
        };
        // Found by ISBN: other editions and books may be found by title.
        const t = byTitle();
        if (lastQuery && "isbn" in lastQuery && t) {
            add("Search by title and author", `Sends ${describe(t)} to Open Library, for other editions`, () => search(t));
        }
        if (googleAllowed && lastQuery && !asked.has("google")) {
            const g = lastQuery;
            add("Try Google Books", `Sends ${describe(g)} to Google. Google Books often has a description.`, () => void askGoogle(g));
        }
        more.replaceChildren(...buttons);
    }

    function renderList(): void {
        list.hidden = matches.length === 0;
        list.replaceChildren(
            ...matches.map((m) => {
                const meta = m.meta;
                const box = el("input", { type: "checkbox", checked: !!m.label, title: "Offer its values under each field" });
                box.addEventListener("change", () => {
                    if (box.checked) pick(m);
                    else unpick(m);
                });
                const ids = (meta.Identifiers ?? []).map((i) => `${i.Scheme}: ${i.Value}`);
                const details = [meta.Date.slice(0, 4), meta.Publisher, (meta.Languages ?? []).join(", "), ...ids].filter(Boolean).join(" · ");
                const compare = el("button", { type: "button", className: "meta-source-btn", title: "Compare with the current metadata" }, "Compare");
                compare.addEventListener("click", () => openDialog(m));
                return el(
                    "li",
                    { className: `lookup-result${m.label ? " picked" : ""}` },
                    el(
                        "label",
                        { className: "lookup-pick" },
                        box,
                        el(
                            "span",
                            { className: "lookup-result-text" },
                            el("strong", {}, meta.Title || "(no title)"),
                            meta.Creators?.length ? el("span", { className: "lookup-result-by" }, ` by ${meta.Creators.join(", ")}`) : "",
                            details ? el("span", { className: "lookup-result-meta" }, details) : "",
                        ),
                    ),
                    el("span", { className: "lookup-result-label" }, m.label ? `→ ${m.label}` : SOURCE_NAMES[m.source]),
                    compare,
                );
            }),
        );
    }

    const render = (): void => {
        renderStatus();
        renderActions();
    };
    // Before a search, the bar says what Search would send, as the form changes.
    host.form.addEventListener("input", render);
    host.form.addEventListener("click", () => requestAnimationFrame(render));

    // ---- Asking ------------------------------------------------------------

    async function ask(source: Source, q: Query): Promise<LookupResult> {
        asked.add(source);
        return api<LookupResult>("GET", `/metadata/lookup?${new URLSearchParams({ source, ...q })}`);
    }

    /** Puts a source's matches in the list, after the ticked ones; returns how many are new. */
    function show(source: Source, r: LookupResult, { replace }: { replace: boolean }): number {
        const keep = replace ? picked() : matches;
        const have = new Set(keep.map((m) => m.key));
        const fresh: Match[] = [];
        for (const { option, cover } of r.matches ?? []) {
            const key = matchKey(source, option);
            if (have.has(key)) continue;
            have.add(key);
            fresh.push({ key, source, meta: option, cover: cover ?? "", label: null });
        }
        matches = [...keep, ...fresh];
        renderList();
        return (r.matches ?? []).length;
    }

    /** One lookup at a time; errors go to the bar. */
    async function run(work: () => Promise<void>): Promise<void> {
        if (busy) return;
        busy = true;
        renderActions();
        try {
            await work();
        } catch (err) {
            found = `Lookup failed: ${errorMessage(err)}`;
            say(found, true);
        } finally {
            busy = false;
            renderActions();
        }
    }

    const tickHint = (n: number): string =>
        n > 1 ? " Tick the right ones to offer their values under each field (the same value is one pill), or Compare one." : " Tick it to offer its values under each field, or Compare it.";

    /** Open Library by q; by title and author when an ISBN finds nothing. */
    function search(q: Query): void {
        void run(async () => {
            lastQuery = q;
            asked = new Set();
            say(`Searching Open Library for ${describe(q)}…`);
            let r = await ask("openlibrary", q);
            let notes = "";
            if (!r.matches?.length && "isbn" in q) {
                const t = byTitle();
                notes = `Open Library doesn't know ISBN ${q.isbn}${r.error ? ` (${r.error})` : ""}.`;
                if (t) {
                    say(`${notes} Searching by ${describe(t)}…`);
                    lastQuery = q = t;
                    r = await ask("openlibrary", t);
                    notes += " ";
                }
            }
            const n = show("openlibrary", r, { replace: true });
            if (n) {
                found = `${notes}Open Library found ${n === 1 ? "a book" : `${n} books`} for ${describe(q)}.${tickHint(n)}`;
            } else {
                found = notes && !("isbn" in q) ? `${notes}No match for ${describe(q)} either.` : notes || `Open Library: ${r.error || `no match for ${describe(q)}`}.`;
            }
            say(found, !!r.error && !n);
        });
    }

    async function askGoogle(q: Query): Promise<void> {
        await run(async () => {
            say(`Asking Google Books for ${describe(q)}…`);
            const r = await ask("google", q);
            const n = show("google", r, { replace: false });
            found = n ? `Google Books found ${describe(q)}; it's at the end of the list.` : `Google Books: ${r.error || "no match"}.`;
            say(found, !!r.error && !n);
        });
    }

    searchBtn.addEventListener("click", () => {
        const q = bestQuery();
        if (q) search(q);
        else say("Add an ISBN or a title to search Open Library.", true);
    });

    // ---- The dialog: one match vs the current metadata ----------------------

    const keepBtn = maybeId("lookup-keep", HTMLButtonElement);
    const acceptBtn = maybeId("lookup-accept", HTMLButtonElement);
    let shown: Match | null = null;

    function openDialog(m: Match): void {
        shown = m;
        if (keepBtn) keepBtn.hidden = !!m.label;
        const title = maybeId("lookup-title");
        if (title) title.textContent = `${m.label ?? SOURCE_NAMES[m.source]} vs current metadata`;
        maybeId("lookup-body")?.replaceChildren(...host.preview(asSource(m)));
        if (!dialog.open) dialog.showModal();
    }

    keepBtn?.addEventListener("click", () => {
        if (shown) pick(shown);
        dialog.close();
    });
    acceptBtn?.addEventListener("click", () => {
        if (!shown) return;
        pick(shown);
        dialog.close();
        host.acceptAll(asSource(shown));
    });
    closeOnBackdropClick(dialog);

    render();
    return {
        lookUpIsbn(isbn) {
            bar.scrollIntoView({ behavior: "smooth", block: "nearest" });
            search({ isbn });
        },
    };
}
