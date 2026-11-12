// The Library repairs page (Go: library_repairs.go): check on request, show
// what would change, repair on request. Only the database is modified.
import { api, byId, el, errorMessage, statusLine } from "./dom.js";
import type { FormatRepair, RepairCheck } from "./types.js";

const say = statusLine("repair-status");
const check = byId("repair-check", HTMLButtonElement);
const misnamed = byId("misnamed", HTMLElement);
const misnamedList = byId("misnamed-list", HTMLUListElement);
const misnamedFix = byId("misnamed-fix", HTMLButtonElement);
const unlisted = byId("unlisted", HTMLElement);
const unlistedList = byId("unlisted-list", HTMLUListElement);
const unlistedFix = byId("unlisted-fix", HTMLButtonElement);

/** Rows shown per list; the rest are counted. */
const SHOWN = 200;

function bookLink(uuid: string, title: string): HTMLAnchorElement {
    return el("a", { href: `/book/get/${encodeURIComponent(uuid)}`, textContent: title || uuid, target: "_blank" });
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
        misnamed.hidden = res.misnamed.length === 0;
        misnamedFix.disabled = false;

        const fixable = res.unlisted.filter((b) => b.formats.length > 0).length;
        fill(unlistedList, res.unlisted, (b) => [
            bookLink(b.uuid, b.title),
            b.formats.length ? ` adds ${b.formats.join(", ")}` : "",
            b.ambiguous.length ? el("span", { className: "auth-muted", textContent: ` (several ${b.ambiguous.join(", ")} files: left alone)` }) : "",
        ]);
        unlisted.hidden = res.unlisted.length === 0;
        unlistedFix.disabled = fixable === 0;

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
        misnamed.hidden = true;
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
        unlisted.hidden = unlistedList.children.length === 0;
    } catch (err) {
        unlistedFix.disabled = false;
        say(errorMessage(err), true);
    }
});
