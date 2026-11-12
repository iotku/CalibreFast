// The Library repairs page (Go: library_repairs.go): check on request, show
// what would change, repair on request. Only the database is modified.
import { api, byId, el, errorMessage, statusLine } from "./dom.js";
import type { FormatRepair, RepairCheck } from "./types.js";

const say = statusLine("repair-status");
const check = byId("repair-check", HTMLButtonElement);
const misnamedState = byId("misnamed-state", HTMLElement);
const misnamedList = byId("misnamed-list", HTMLUListElement);
const misnamedFix = byId("misnamed-fix", HTMLButtonElement);
const unlistedState = byId("unlisted-state", HTMLElement);
const unlistedList = byId("unlisted-list", HTMLUListElement);
const unlistedFix = byId("unlisted-fix", HTMLButtonElement);

/** Rows shown per list; the rest are counted. */
const SHOWN = 200;

function bookLink(uuid: string, title: string): HTMLAnchorElement {
    return el("a", { href: `/book/get/${encodeURIComponent(uuid)}`, textContent: title || uuid, target: "_blank" });
}

/** Show a section's result: its list and button, or a line when there's nothing. */
function show(state: HTMLElement, list: HTMLUListElement, fix: HTMLButtonElement, count: number, canFix: boolean): void {
    state.hidden = count > 0;
    state.replaceChildren(el("em", { textContent: "Nothing to repair." }));
    list.hidden = count === 0;
    fix.hidden = count === 0;
    fix.disabled = !canFix;
}

function fill<T>(list: HTMLUListElement, items: T[], row: (item: T) => (Node | string)[]): void {
    list.replaceChildren(...items.slice(0, SHOWN).map((it) => el("li", {}, ...row(it))));
    if (items.length > SHOWN) list.append(el("li", { className: "auth-muted", textContent: `…and ${items.length - SHOWN} more` }));
}

async function run(): Promise<void> {
    check.disabled = true;
    say("Checking every book's files…");
    try {
        const res = await api<RepairCheck>("GET", "/api/admin/repairs");
        fill(misnamedList, res.misnamed, (m) => [
            bookLink(m.uuid, m.title),
            ` ${m.format}: `,
            el("code", { textContent: m.from }),
            " → ",
            el("code", { textContent: m.to }),
        ]);
        show(misnamedState, misnamedList, misnamedFix, res.misnamed.length, true);

        const fixable = res.unlisted.filter((b) => b.formats.length > 0).length;
        fill(unlistedList, res.unlisted, (b) => [
            bookLink(b.uuid, b.title),
            b.formats.length ? ` adds ${b.formats.join(", ")}` : "",
            b.ambiguous.length ? el("span", { className: "auth-muted", textContent: ` (several ${b.ambiguous.join(", ")} files: left alone)` }) : "",
        ]);
        show(unlistedState, unlistedList, unlistedFix, res.unlisted.length, fixable > 0);

        const found: string[] = [];
        if (res.misnamed.length) found.push(`${res.misnamed.length} file name${res.misnamed.length === 1 ? "" : "s"} to repair`);
        if (res.unlisted.length) found.push(`${res.unlisted.length} book${res.unlisted.length === 1 ? "" : "s"} with unlisted files`);
        say(found.length ? `Found ${found.join(" and ")}.` : "Nothing to repair.");
    } catch (err) {
        say(errorMessage(err), true);
    } finally {
        check.disabled = false;
    }
}

check.addEventListener("click", () => void run());

misnamedFix.addEventListener("click", async () => {
    misnamedFix.disabled = true;
    try {
        const res = await api<{ repaired: number }>("POST", "/api/admin/repairs/format-names");
        misnamedList.replaceChildren();
        show(misnamedState, misnamedList, misnamedFix, 0, false);
        say(`Repaired ${res.repaired} file name${res.repaired === 1 ? "" : "s"}.`);
    } catch (err) {
        misnamedFix.disabled = false;
        say(errorMessage(err), true);
    }
});

unlistedFix.addEventListener("click", async () => {
    unlistedFix.disabled = true;
    try {
        const res = await api<FormatRepair>("POST", "/api/library/repair-formats", { uuids: [] });
        let msg = `Added ${res.formats} format${res.formats === 1 ? "" : "s"} to ${res.books} book${res.books === 1 ? "" : "s"}.`;
        if (res.skipped.length) msg += ` ${res.skipped.length} left alone: several files of one format.`;
        say(msg);
        // What's left are the ones it couldn't do.
        const left = new Set(res.skipped.map((s) => s.uuid));
        for (const li of [...unlistedList.children]) {
            const a = li.querySelector("a");
            const uuid = a ? decodeURIComponent(a.pathname.split("/").pop() ?? "") : "";
            if (!left.has(uuid)) li.remove();
        }
        // Any left are ones with several files of a format: nothing more to do here.
        show(unlistedState, unlistedList, unlistedFix, unlistedList.children.length, false);
    } catch (err) {
        unlistedFix.disabled = false;
        say(errorMessage(err), true);
    }
});
