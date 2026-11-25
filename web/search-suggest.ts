/**
 * Suggestions under the top bar's search box while typing
 * (templates/topbar.html, set up by nav.ts): the best few books, then the
 * authors, series and tags that match, from /api/search/suggest (the
 * search /search runs, cut short: search.go).
 *
 * An ARIA combobox and listbox, keyed as suggest.ts's lists are: ↓/↑ move
 * through the options (aria-activedescendant), Enter opens the one picked,
 * Escape closes the list. Enter with nothing picked submits the form to
 * /search, as without suggestions. Each option is a link, so a click (or a
 * middle click) goes there as any link would.
 *
 * Every name is set as text (el(), textContent): titles and names come from
 * book metadata, which anyone who can add a book controls.
 */

import { el } from "./dom.js";
import { debounce } from "./utils.js";

interface SuggestBook {
    uuid: string;
    title: string;
    authors: string;
    series: string;
    series_index: number;
}

interface SuggestLink {
    kind: string; // author, series, tag
    name: string;
    href: string;
    count: number;
}

interface SuggestResponse {
    books: SuggestBook[];
    links: SuggestLink[];
}

/** Queries shorter than this aren't looked up. */
const MIN_CHARS = 2;
const DEBOUNCE_MS = 150;
const KIND_LABEL: Record<string, string> = { author: "Author", series: "Series", tag: "Tag" };

export interface SearchSuggest {
    close(): void;
}

/** A path on this site (not //host), or nothing: links come from book data via the server. */
function sitePath(href: string): string | null {
    return href.startsWith("/") && !href.startsWith("//") && !href.startsWith("/\\") ? href : null;
}

function seriesText(b: SuggestBook): string {
    if (!b.series) return "";
    const n = Number(b.series_index);
    return Number.isFinite(n) && n > 0 ? `${b.series} #${n}` : b.series;
}

/**
 * Hangs the suggestions under input, in form (the .tb-search form).
 * onOpen runs when the list opens, so the bar can close its other menus.
 */
export function attachSearchSuggest(form: HTMLFormElement, input: HTMLInputElement, onOpen: () => void): SearchSuggest {
    const listId = "tb-suggest";
    const panel = el("div", { id: listId, className: "tb-menu-list tb-suggest", role: "listbox", ariaLabel: "Suggestions", hidden: true });
    form.append(panel);

    input.setAttribute("role", "combobox");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-controls", listId);
    input.setAttribute("aria-expanded", "false");
    input.autocomplete = "off";

    let options: HTMLAnchorElement[] = [];
    let active = -1;
    let request: AbortController | null = null;
    let shownFor = "";

    function close(): void {
        request?.abort();
        request = null;
        lookUp.cancel();
        panel.hidden = true;
        panel.replaceChildren();
        options = [];
        active = -1;
        shownFor = "";
        input.setAttribute("aria-expanded", "false");
        input.removeAttribute("aria-activedescendant");
    }

    function setActive(i: number): void {
        active = i;
        options.forEach((a, j) => {
            const on = j === i;
            a.setAttribute("aria-selected", String(on));
            if (on) {
                a.scrollIntoView({ block: "nearest" });
                input.setAttribute("aria-activedescendant", a.id);
            }
        });
        if (i < 0) input.removeAttribute("aria-activedescendant");
    }

    function option(href: string, main: string, sub: string, className = ""): HTMLAnchorElement {
        const a = el("a", { id: `${listId}-${options.length}`, href, className, tabIndex: -1 }, el("span", { className: "tb-suggest-main", textContent: main }));
        if (sub) a.append(el("span", { className: "tb-suggest-sub", textContent: sub }));
        a.setAttribute("role", "option");
        a.setAttribute("aria-selected", "false");
        options.push(a);
        return a;
    }

    function group(label: string, items: HTMLElement[]): HTMLElement {
        const id = `${listId}-h-${label.toLowerCase().replace(/\W+/g, "-")}`;
        const g = el("div", { className: "tb-suggest-group", role: "group" }, el("div", { id, className: "tb-suggest-head", role: "presentation", textContent: label }), ...items);
        g.setAttribute("aria-labelledby", id);
        return g;
    }

    function show(q: string, res: SuggestResponse): void {
        options = [];
        active = -1;
        input.removeAttribute("aria-activedescendant");
        const groups: HTMLElement[] = [];
        const books = res.books.map((b) =>
            option(`/book/get/${encodeURIComponent(b.uuid)}`, b.title, [b.authors, seriesText(b)].filter(Boolean).join(" · ")),
        );
        if (books.length) groups.push(group("Books", books));
        const links: HTMLElement[] = [];
        for (const l of res.links) {
            const href = sitePath(l.href);
            if (!href) continue;
            const kind = KIND_LABEL[l.kind] ?? "";
            links.push(option(href, l.name, `${kind}${kind ? " · " : ""}${l.count} ${l.count === 1 ? "book" : "books"}`));
        }
        if (links.length) groups.push(group("Authors, series and tags", links));
        if (!groups.length) {
            close();
            return;
        }
        groups.push(el("div", { className: "tb-suggest-group" }, option(`/search?q=${encodeURIComponent(q)}`, `All results for “${q}”`, "", "tb-suggest-all")));
        panel.replaceChildren(...groups);
        shownFor = q;
        if (panel.hidden) {
            panel.hidden = false;
            input.setAttribute("aria-expanded", "true");
            onOpen();
        }
    }

    async function fetchSuggestions(q: string): Promise<void> {
        request?.abort();
        const ctl = new AbortController();
        request = ctl;
        try {
            const r = await fetch(`/api/search/suggest?q=${encodeURIComponent(q)}`, { credentials: "same-origin", signal: ctl.signal });
            if (!r.ok) throw new Error(`HTTP ${r.status}`);
            const res = (await r.json()) as Partial<SuggestResponse>;
            if (ctl !== request || input.value.trim() !== q || document.activeElement !== input) return; // typed on, or left
            show(q, { books: Array.isArray(res.books) ? res.books : [], links: Array.isArray(res.links) ? res.links : [] });
        } catch (err) {
            if (ctl.signal.aborted) return;
            console.warn("search suggestions:", err);
            if (ctl === request) close();
        } finally {
            if (ctl === request) request = null;
        }
    }

    const lookUp = debounce((q: string) => void fetchSuggestions(q), DEBOUNCE_MS);

    input.addEventListener("input", () => {
        const q = input.value.trim();
        if (q.length < MIN_CHARS) {
            close();
            return;
        }
        if (q === shownFor && !panel.hidden) return;
        lookUp(q);
    });

    // Keep the focus in the box while an option is pressed: a click on a
    // link doesn't focus it in every browser, and leaving the box closes
    // the list (below) before the click lands.
    panel.addEventListener("mousedown", (e) => e.preventDefault());
    input.addEventListener("focusout", (e) => {
        if (e.relatedTarget instanceof Node && form.contains(e.relatedTarget)) return;
        close();
    });

    input.addEventListener("keydown", (e) => {
        if (e.isComposing) return;
        const open = !panel.hidden && options.length > 0;
        if (!open) return;
        if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault();
            const n = options.length;
            setActive(e.key === "ArrowDown" ? (active + 1) % n : active < 0 ? n - 1 : (active - 1 + n) % n);
        } else if (e.key === "Enter" && active >= 0) {
            e.preventDefault();
            options[active]?.click();
        } else if (e.key === "Escape") {
            // Only the list: a second Escape closes the box itself (nav.ts).
            e.preventDefault();
            e.stopPropagation();
            close();
        }
    });

    return { close };
}
