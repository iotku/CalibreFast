// The admin's Activity page (templates/admin-activity.html): undo one
// change, or all of one user's since a moment (POST /api/admin/activity/revert,
// Go: activityRevertHandler in editlog.go).
import { api, byId, el, errorMessage, maybeId, statusLine } from "./dom.js";

/** One entry's outcome (Go: revertResult). */
interface RevertResult {
    id: number;
    action: string;
    title: string;
    done: boolean;
    error?: string;
}

const say = statusLine("activity-status");
const table = byId("activity-table", HTMLTableElement);
const resultList = byId("activity-results", HTMLUListElement);

async function revert(body: { ids: number[] } | { user_id: number; since: number }): Promise<void> {
    try {
        const res = await api<{ results: RevertResult[] }>("POST", "/api/admin/activity/revert", body);
        show(res.results);
    } catch (err) {
        say(errorMessage(err), true);
    }
}

function show(results: RevertResult[]): void {
    const done = results.filter((r) => r.done);
    const failed = results.filter((r) => !r.done);
    for (const r of done) {
        const row = table.querySelector<HTMLTableRowElement>(`tr[data-id="${r.id}"]`);
        if (!row) continue;
        row.classList.add("is-reverted");
        row.lastElementChild?.replaceChildren(el("span", { className: "auth-muted", textContent: "Undone" }));
    }
    const what = (n: number): string => `${n} change${n === 1 ? "" : "s"}`;
    say(
        results.length === 0
            ? "Nothing to undo."
            : `Undid ${what(done.length)}${failed.length ? `; ${what(failed.length)} left as they are` : ""}. Reload to see the undoing listed.`,
        failed.length > 0 && done.length === 0,
    );
    resultList.hidden = failed.length === 0;
    resultList.replaceChildren(
        ...failed.map((r) => el("li", { textContent: `#${r.id} ${r.action} ${r.title ? `“${r.title}”` : ""}: ${r.error ?? "not undone"}` })),
    );
}

table.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>("button[data-undo]") : null;
    if (!btn) return;
    btn.disabled = true;
    void revert({ ids: [Number(btn.dataset["undo"])] }).finally(() => {
        btn.disabled = false;
    });
});

const since = maybeId("undo-since");
if (since instanceof HTMLFormElement) {
    since.addEventListener("submit", (e) => {
        e.preventDefault();
        const input = since.elements.namedItem("since");
        if (!(input instanceof HTMLInputElement) || !input.value) return;
        const when = new Date(input.value);
        if (Number.isNaN(when.getTime())) return;
        if (!confirm(`Undo every change this user made since ${when.toLocaleString()}? Changes someone else has built on since are left alone.`)) return;
        void revert({ user_id: Number(since.dataset["user"]), since: Math.floor(when.getTime() / 1000) });
    });
}
