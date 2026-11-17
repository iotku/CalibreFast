/**
 * The search box of the aggregate pages (/authors, /tags, …,
 * templates/aggregate.html). The page lists what has more than one book;
 * the box finds every entry, single-book ones too, from /api/<plural>.
 * Matching is fuzzy (see fuzzy.ts): names that start with the query come
 * first, then those containing it, then typo and initials matches; ties go
 * to the entry with more books. An empty box shows the page as it was.
 */

import { byId, el } from "./dom.js";
import { bestScore, type Hay, prepare, prepareQuery } from "./fuzzy.js";
import type { AggregateEntry } from "./types.js";

const MAX_RESULTS = 300;

interface Entry {
    item: AggregateEntry;
    hay: Hay[]; // name, display name and members
}

const page = byId("aggregate-page");
const input = byId("agg-search", HTMLInputElement);
const results = byId("agg-results");
const grid = byId("aggregate-grid");
const featured = document.getElementById("agg-featured");
const src = page.dataset["src"] ?? "";
const prefix = page.dataset["prefix"] ?? "/";

let entries: Entry[] | null = null;
let loading: Promise<Entry[]> | null = null;
let maxCount = 1;

function load(): Promise<Entry[]> {
    loading ??= fetch(src, { credentials: "same-origin" })
        .then((res): Promise<AggregateEntry[]> => {
            if (!res.ok) throw new Error(`HTTP ${res.status}`);
            return res.json() as Promise<AggregateEntry[]>;
        })
        .then((items) => {
            maxCount = Math.max(1, ...items.map((i) => i.count));
            entries = items.map((item) => ({
                item,
                hay: [item.name, item.display ?? "", ...(item.members ?? [])].filter(Boolean).map(prepare),
            }));
            return entries;
        })
        .catch((err: unknown): Entry[] => {
            console.error("aggregate search:", err);
            loading = null; // try again on the next key
            return [];
        });
    return loading;
}

/** Entries matching q, best match first, then by book count. */
function search(all: Entry[], q: string): AggregateEntry[] {
    const query = prepareQuery(q);
    const found: { item: AggregateEntry; score: number }[] = [];
    for (const e of all) {
        const score = bestScore(query, e.hay);
        if (score) found.push({ item: e.item, score });
    }
    found.sort((a, b) => b.score - a.score || b.item.count - a.item.count);
    return found.map((f) => f.item);
}

function link(item: AggregateEntry): HTMLAnchorElement {
    const ratio = item.count / maxCount;
    const a = el("a", { className: "agg-item", href: prefix + encodeURIComponent(item.name) });
    a.style.fontSize = `${(12 + ratio * 24).toFixed(1)}px`;
    a.style.setProperty("--o", (0.4 + ratio * 0.6).toFixed(2));
    if (item.members?.length) {
        a.classList.add("agg-group");
        a.title = `Group: ${item.members.join(", ")}`;
    }
    a.append(item.display || item.name);
    if (item.display && item.display !== item.name) a.append(" ", el("span", { className: "agg-sub" }, item.name));
    a.append(` (${item.count})`);
    return a;
}

function show(searching: boolean): void {
    results.hidden = !searching;
    grid.hidden = searching;
    if (featured) featured.hidden = searching;
}

async function update(): Promise<void> {
    const q = input.value.trim();
    if (!q) {
        show(false);
        results.replaceChildren();
        return;
    }
    const all = entries ?? (await load());
    if (input.value.trim() !== q) return; // typed on meanwhile
    const found = search(all, q);
    const shown = found.slice(0, MAX_RESULTS);
    results.replaceChildren(...shown.map(link));
    if (found.length > shown.length) {
        results.append(el("p", { className: "agg-more" }, `${found.length - shown.length} more: keep typing to narrow it down.`));
    } else if (found.length === 0) {
        results.append(el("p", { className: "status-message" }, entries ? "Nothing found." : "Couldn't load the list."));
    }
    show(true);
}

input.addEventListener("input", () => void update());
input.addEventListener("focus", () => void load(), { once: true });
input.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && input.value) {
        input.value = "";
        void update();
    }
});
// The browser may restore the box's text on back/forward.
if (input.value) void update();
