/**
 * Library-backed autocomplete for plain text inputs (metadata editor).
 *
 * Suggestions come only from values that exist in the library
 * (/api/groups/{category}/values), never from browser form history. Unlike
 * the group editor, new values are allowed here, so nothing is preselected:
 * Enter keeps doing whatever the field normally does with the typed text.
 * Arrow keys pick a suggestion; Enter/Tab/click accept it.
 *
 * multi: the field holds a comma-separated list; only the item after the last
 * comma is completed, and items already present aren't suggested.
 */

import type { ValueSuggestion } from "./types.js";

export interface SuggestOptions {
    /** Category key on the server: "author", "publisher", "tag", "language". */
    category: string;
    /** The field holds a comma-separated list. */
    multi?: boolean;
    /** Values not to suggest (e.g. chips already added). */
    exclude?: () => string[];
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
    { category, multi = false, exclude = () => [], onPick = null }: SuggestOptions,
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

    const token = (): string => {
        const v = input.value;
        return multi ? v.slice(v.lastIndexOf(",") + 1) : v;
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
            const head = v.slice(0, v.lastIndexOf(",") + 1);
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

        const taken = new Set(exclude().map(lower));
        if (multi) {
            for (const t of input.value.split(",").slice(0, -1)) taken.add(lower(t.trim()));
        }
        const starts: ValueSuggestion[] = [];
        const contains: ValueSuggestion[] = [];
        for (const v of values) {
            const val = lower(v.value);
            if (taken.has(val)) continue; // exact matches stay listed
            const hay = [val, lower(v.label || "")];
            if (hay.some((h) => h.startsWith(q))) starts.push(v);
            else if (hay.some((h) => h.includes(q))) contains.push(v);
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
