// /admin/discover (templates/admin-discover.html): which tags get a row on
// the home page. Every tag is listed with whether it has a row and why not;
// an admin includes or excludes tags and sets the cutoff (tag_rows.go).
import { api, byId, errorMessage, statusLine } from "./dom.js";

interface TagInfo {
    name: string;
    count: number;
    share: number;
    mode: "" | "include" | "deny";
    status: "rows" | "included" | "denied" | "broad" | "few" | "not-top";
}

interface Settings {
    broad_tag_share: number;
    rows_max: number;
    min_books: number;
    tags: TagInfo[];
}

const say = statusLine("discover-status");
const list = byId("tag-list");
const filter = byId("tag-filter", HTMLInputElement);
const summary = byId("tag-summary");
const cutoff = byId("cutoff", HTMLInputElement);

let settings: Settings | null = null;

function why(t: TagInfo, s: Settings): string {
    switch (t.status) {
        case "rows":
            return "Has a row";
        case "included":
            return "Has a row (included)";
        case "denied":
            return "Excluded";
        case "broad":
            return `Too broad (${(t.share * 100).toFixed(t.share < 0.1 ? 1 : 0)}% of the library)`;
        case "few":
            return `Too few books (under ${s.min_books})`;
        case "not-top":
            return `Not among the ${s.rows_max} biggest`;
    }
}

function render(): void {
    const s = settings;
    if (!s) return;
    const q = filter.value.trim().toLowerCase();
    const shown = s.tags.filter((t) => !q || t.name.toLowerCase().includes(q));
    const rows = s.tags.filter((t) => t.status === "rows" || t.status === "included").length;
    summary.textContent = `${rows} of ${s.tags.length} tags have a row.`;
    list.replaceChildren(
        ...shown.slice(0, 500).map((t) => {
            const tr = document.createElement("tr");
            tr.dataset["name"] = t.name;
            tr.className = t.status === "rows" || t.status === "included" ? "" : "discover-off";
            const name = document.createElement("td");
            const link = document.createElement("a");
            link.href = `/tag/${encodeURIComponent(t.name)}`;
            link.textContent = t.name;
            name.append(link);
            const count = document.createElement("td");
            count.textContent = String(t.count);
            const state = document.createElement("td");
            state.textContent = why(t, s);
            const pick = document.createElement("td");
            const sel = document.createElement("select");
            sel.setAttribute("aria-label", `Setting for ${t.name}`);
            for (const [v, label] of [
                ["", "Default"],
                ["include", "Include"],
                ["deny", "Exclude"],
            ] as const) {
                const o = document.createElement("option");
                o.value = v;
                o.textContent = label;
                o.selected = t.mode === v;
                sel.append(o);
            }
            pick.append(sel);
            tr.append(name, count, state, pick);
            return tr;
        }),
    );
    if (shown.length > 500) {
        const tr = document.createElement("tr");
        tr.innerHTML = `<td colspan="4" class="auth-muted"></td>`;
        tr.firstElementChild!.textContent = `${shown.length - 500} more: narrow the list with Find a tag.`;
        list.append(tr);
    }
}

async function load(): Promise<void> {
    settings = await api<Settings>("GET", "/api/admin/discover");
    cutoff.value = String(Math.round(settings.broad_tag_share * 100));
    render();
}

filter.addEventListener("input", render);

list.addEventListener("change", async (e) => {
    const sel = e.target instanceof HTMLSelectElement ? e.target : null;
    const name = sel?.closest<HTMLElement>("tr")?.dataset["name"];
    if (!sel || !name) return;
    const path = `/api/admin/discover/tags/${encodeURIComponent(name)}`;
    try {
        if (sel.value) await api("PUT", path, { mode: sel.value });
        else await api("DELETE", path);
        await load();
        say(`Saved: ${name}.`);
    } catch (err) {
        say(errorMessage(err), true);
    }
});

byId("discover-cutoff", HTMLFormElement).addEventListener("submit", async (e) => {
    e.preventDefault();
    const pct = Number(cutoff.value);
    try {
        await api("PUT", "/api/admin/discover/settings", { broad_tag_share: pct / 100 });
        await load();
        say("Saved the cutoff.");
    } catch (err) {
        say(errorMessage(err), true);
    }
});

load().catch((err: unknown) => say(errorMessage(err), true));
