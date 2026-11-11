/**
 * Library-backed autocomplete for plain text inputs (metadata editor).
 *
 * Suggestions come only from values that exist in the library
 * (/api/groups/{category}/values), never from browser form history. Unlike
 * the group editor, new values are allowed here, so nothing is preselected:
 * Enter keeps doing whatever the field normally does with the typed text.
 * Arrow keys pick a suggestion; Enter/Tab/click accept it.
 *
 * multi: the field holds a list (separated by commas, or by the characters in
 * separators); only the item after the last separator is completed, and
 * items already present aren't suggested.
 *
 * canonical: values that mean the same thing (e.g. languages "en" and "eng")
 * are offered once, in their canonical form, and not at all when the field
 * already holds one of them.
 */

import type { ValueSuggestion } from "./types.js";

export interface SuggestOptions {
    /** Category key on the server: "author", "publisher", "tag", "language". */
    category: string;
    /** The field holds a list. */
    multi?: boolean;
    /** Characters that separate a list's items (default ","). */
    separators?: string;
    /** Values not to suggest (e.g. chips already added). */
    exclude?: () => string[];
    /**
     * Preferred form of a value; values with the same canonical form (ignoring
     * case) count as one. Default: the value itself.
     */
    canonical?: (value: string) => string;
    /** Identity of a value for that comparison. Default: canonical, lowercased. */
    key?: (value: string) => string;
    /** Called after a suggestion is accepted. */
    onPick?: ((value: string) => void) | null;
}

const MAX = 30;
const cache = new Map<string, Promise<ValueSuggestion[]>>();
const lower = (s: string): string => s.toLowerCase();

function loadValues(category: string): Promise<ValueSuggestion[]> {
    if (!cache.has(category)) {
        cache.set(
            category,
            fetch(`/api/groups/${encodeURIComponent(category)}/values`)
                .then((r): Promise<ValueSuggestion[]> | ValueSuggestion[] => (r.ok ? r.json() : []))
                .catch((): ValueSuggestion[] => {
                    cache.delete(category); // retry next time
                    return [];
                }),
        );
    }
    return cache.get(category) as Promise<ValueSuggestion[]>;
}

let uid = 0;

export function attachSuggest(
    input: HTMLInputElement,
    { category, multi = false, separators = ",", exclude = () => [], canonical = (v) => v, key = (v) => lower(canonical(v)), onPick = null }: SuggestOptions,
): void {
    const id = `suggest-${++uid}`;
    const list = document.createElement("ul");
    list.className = "suggest-list";
    list.id = id;
    list.setAttribute("role", "listbox");
    list.hidden = true;

    // Anchor the list to the input itself (a fieldset's legend skews offsets),
    // so it opens directly under the box and covers whatever is below it.
    const wrap = document.createElement("div");
    wrap.className = "suggest-wrap";
    input.replaceWith(wrap);
    wrap.append(input, list);

    input.setAttribute("role", "combobox");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-controls", id);
    input.setAttribute("aria-expanded", "false");
    input.autocomplete = "off";
    input.dataset["1pIgnore"] = "";
    input.dataset["lpignore"] = "true";
    input.dataset["formType"] = "other";

    let shown: ValueSuggestion[] = [];
    let active = -1;
    let picking = false; // our own input event after a pick shouldn't reopen

    /** Where the last item starts (after the last separator). */
    const itemStart = (v: string): number => Math.max(...[...separators].map((c) => v.lastIndexOf(c))) + 1;
    const token = (): string => {
        const v = input.value;
        return multi ? v.slice(itemStart(v)) : v;
    };

    function close(): void {
        list.hidden = true;
        input.setAttribute("aria-expanded", "false");
        input.removeAttribute("aria-activedescendant");
        shown = [];
        active = -1;
    }

    function setActive(i: number): void {
        active = i;
        [...list.children].forEach((li, j) => {
            const on = j === i;
            li.setAttribute("aria-selected", String(on));
            if (on) {
                li.scrollIntoView({ block: "nearest" });
                input.setAttribute("aria-activedescendant", li.id);
            }
        });
    }

    function pick(value: string): void {
        if (multi) {
            const v = input.value;
            const head = v.slice(0, itemStart(v));
            input.value = (head ? `${head.trimEnd()} ` : "") + value;
        } else {
            input.value = value;
        }
        close();
        picking = true;
        input.dispatchEvent(new Event("input", { bubbles: true }));
        picking = false;
        onPick?.(value);
        input.focus();
    }

    async function refresh(): Promise<void> {
        if (picking) return;
        const q = lower(token().trim());
        if (!q || document.activeElement !== input) return close();
        const values = await loadValues(category);
        if (lower(token().trim()) !== q) return; // typed on meanwhile
        if (document.activeElement !== input) return close(); // focus moved on meanwhile

        const taken = new Set(exclude().map(key));
        if (multi) {
            const v = input.value;
            for (const t of v.slice(0, itemStart(v)).split(new RegExp(`[${separators.replace(/[\]\\^-]/g, "\\$&")}]`))) {
                if (t.trim()) taken.add(key(t.trim()));
            }
        }
        const starts: ValueSuggestion[] = [];
        const contains: ValueSuggestion[] = [];
        const offered = new Set<string>();
        for (const v of values) {
            const k = key(v.value);
            if (taken.has(k) || offered.has(k)) continue; // exact matches stay listed
            const hay = [lower(v.value), k, lower(v.label || "")];
            const s: ValueSuggestion = { ...v, value: canonical(v.value) };
            if (hay.some((h) => h.startsWith(q))) starts.push(s);
            else if (hay.some((h) => h.includes(q))) contains.push(s);
            else continue;
            offered.add(k);
            if (starts.length >= MAX) break;
        }
        shown = starts.concat(contains).slice(0, MAX);
        if (!shown.length) return close();

        list.replaceChildren(
            ...shown.map((v, i) => {
                const li = document.createElement("li");
                li.id = `${id}-${i}`;
                li.setAttribute("role", "option");
                li.textContent = v.label || v.value;
                if (v.label && v.label !== v.value) {
                    const sub = document.createElement("span");
                    sub.className = "suggest-sub";
                    sub.textContent = v.value;
                    li.append(sub);
                }
                li.addEventListener("mousedown", (e) => {
                    e.preventDefault(); // keep focus in the input
                    pick(v.value);
                });
                return li;
            }),
        );
        active = -1;
        list.hidden = false;
        input.setAttribute("aria-expanded", "true");
    }

    input.addEventListener("input", () => void refresh());
    input.addEventListener("focus", () => void loadValues(category));
    input.addEventListener("blur", close);

    // Capture phase so this runs before the field's own Enter handling
    // (e.g. chip inputs committing the typed text).
    input.addEventListener(
        "keydown",
        (e) => {
            const open = !list.hidden && shown.length > 0;
            if (!open) return;
            if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                e.preventDefault();
                const n = shown.length;
                setActive(e.key === "ArrowDown" ? (active + 1) % n : (active - 1 + n) % n);
            } else if ((e.key === "Enter" || e.key === "Tab") && (active >= 0 || e.key === "Tab")) {
                e.preventDefault();
                e.stopImmediatePropagation();
                const choice = shown[Math.max(active, 0)];
                if (choice) pick(choice.value);
            } else if (e.key === "Escape") {
                e.preventDefault();
                e.stopImmediatePropagation();
                close();
            }
        },
        true,
    );
}
