// Hardcover series (hardcover.go, hardcover_http.go): finding a series on
// Hardcover and linking it, fetching its list again, applying the review,
// checking an author's series together, and the queue of requests waiting
// for Hardcover's limits. Editors only. Everything Hardcover sends is
// untrusted: it is shown as text (el, textContent), and only links the
// server built and checked (https://hardcover.app/…) become hrefs.

import { api, el, errorMessage, maybeId, statusLine } from "./dom.js";
import type { HardcoverCandidate, HardcoverJob, HardcoverJobAnswer, HardcoverUsage } from "./types.js";

const say = statusLine("hc-status");

/** Goes to a page the server named on this site. */
function go(url: string | undefined): void {
    if (url && url.startsWith("/") && !url.startsWith("//")) location.assign(url);
    else location.reload();
}

/** A link to Hardcover, only when it is one. */
function hardcoverLink(url: string | undefined, text: string): Node {
    if (url && url.startsWith("https://hardcover.app/")) {
        return el("a", { href: url, target: "_blank", rel: "noopener noreferrer", className: "ph-sub" }, text);
    }
    return el("span", { className: "ph-sub" }, text);
}

function showUsage(u: HardcoverUsage | undefined): void {
    const p = maybeId("hc-usage");
    if (!p || !u) return;
    const parts: (Node | string)[] = [
        "Hardcover today: ",
        el("strong", {}, String(u.used)),
        ` of ${u.limit} requests · resets in ${u.resetsIn}`,
    ];
    if (u.paused) parts.push(" · ", el("span", { className: "hc-held" }, `paused until ${u.paused}`));
    p.replaceChildren(...parts);
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** Polls a job until it is done or failed; tells how it waits. */
async function follow(job: HardcoverJob, until?: string): Promise<HardcoverJob> {
    let delay = 1500;
    for (;;) {
        if (job.state === "done" || job.state === "failed") return job;
        if (job.state === "held") {
            say(`Hardcover's limit is used up for now: the request is queued and runs ${until ? `at ${until}` : "later"}. You can leave this page.`);
            delay = 30_000;
        } else {
            say(job.state === "running" ? "Asking Hardcover…" : "Waiting for earlier requests to Hardcover…");
        }
        await sleep(delay);
        const res = await api<{ jobs: HardcoverJob[]; usage: HardcoverUsage }>("GET", `/api/hardcover/jobs?ids=${job.id}`);
        showUsage(res.usage);
        const next = res.jobs[0];
        if (!next) throw new Error("the request was removed");
        if (next.state === "held" && next.notBefore) until = new Date(next.notBefore * 1000).toUTCString();
        job = next;
    }
}

/** Sends a series request and waits for its answer. */
async function request(kind: "candidates" | "link" | "refresh", body: object): Promise<HardcoverJob> {
    const res = await api<HardcoverJobAnswer>("POST", `/api/hardcover/${kind}`, body);
    showUsage(res.usage);
    const job = await follow(res.job, res.until);
    if (job.state === "failed") throw new Error(job.error || "the request failed");
    return job;
}

// ---------------------------------------------------------------------------
// A series' page
// ---------------------------------------------------------------------------

const page = maybeId("hc-page");
const series = page?.dataset["series"] ?? "";

function candidateItem(c: HardcoverCandidate): HTMLLIElement {
    const pick = el("button", { type: "button", className: "btn-standard" + (c.sure ? " btn-primary" : "") }, "Link");
    pick.addEventListener("click", () => {
        for (const b of document.querySelectorAll<HTMLButtonElement>("#hc-candidates button")) b.disabled = true;
        request("link", { series, hcId: c.id })
            .then((job) => go(job.result?.review))
            .catch((err: unknown) => {
                say(errorMessage(err), true);
                for (const b of document.querySelectorAll<HTMLButtonElement>("#hc-candidates button")) b.disabled = false;
            });
    });
    const details: (Node | string)[] = [el("span", { className: "ph-title" }, c.name)];
    if (c.author) details.push(el("span", { className: "ph-sub" }, c.author));
    details.push(el("span", { className: "ph-sub" }, `${c.booksCount} book${c.booksCount === 1 ? "" : "s"}`));
    if (c.hits) details.push(el("span", { className: "hc-badge" }, `${c.hits} of your books`));
    if (c.sure) details.push(el("span", { className: "hc-badge" }, "likely"));
    details.push(hardcoverLink(c.url, "View on Hardcover"));
    return el("li", {}, ...details, pick);
}

maybeId("hc-find", HTMLButtonElement)?.addEventListener("click", (e) => {
    const btn = e.currentTarget as HTMLButtonElement;
    btn.disabled = true;
    const list = maybeId("hc-candidates");
    request("candidates", { series })
        .then((job) => {
            const cands = job.result?.candidates ?? [];
            say(cands.length ? "Pick the Hardcover series this is:" : "Hardcover has no series like this one.");
            list?.replaceChildren(...cands.map(candidateItem));
            if (list) list.hidden = cands.length === 0;
        })
        .catch((err: unknown) => say(errorMessage(err), true))
        .finally(() => (btn.disabled = false));
});

maybeId("hc-refresh", HTMLButtonElement)?.addEventListener("click", (e) => {
    const btn = e.currentTarget as HTMLButtonElement;
    btn.disabled = true;
    request("refresh", { series })
        .then(() => location.reload())
        .catch((err: unknown) => {
            say(errorMessage(err), true);
            btn.disabled = false;
        });
});

maybeId("hc-unlink")?.addEventListener("click", () => {
    if (!confirm("Unlink this series from Hardcover? Placeholders made from it stay.")) return;
    api("DELETE", `/api/hardcover/link?series=${encodeURIComponent(series)}`)
        .then(() => location.reload())
        .catch((err: unknown) => say(errorMessage(err), true));
});

const review = maybeId("hc-review", HTMLFormElement);
const sayApply = statusLine("hc-apply-status");

function ticked(): HTMLInputElement[] {
    return review ? [...review.querySelectorAll<HTMLInputElement>("input.hc-tick:checked")] : [];
}

const idOf = (box: HTMLInputElement): number => Number(box.dataset["hc"] ?? "0");

review?.addEventListener("submit", (e) => {
    e.preventDefault();
    const boxes = ticked();
    if (!boxes.length) {
        sayApply("Nothing is ticked.", true);
        return;
    }
    const body = {
        series,
        placeholders: boxes.filter((b) => b.dataset["kind"] === "placeholder").map(idOf),
        tag: boxes
            .filter((b) => b.dataset["kind"] === "tag")
            .map((b) => ({ uuid: b.dataset["uuid"] ?? "", hcBookId: idOf(b) })),
        dismiss: [] as number[],
    };
    sayApply("Applying…");
    api<{ placeholders: number; tagged: number; errors?: string[] }>("POST", "/api/hardcover/apply", body)
        .then((res) => {
            const done = `${res.placeholders} placeholder${res.placeholders === 1 ? "" : "s"} made, ${res.tagged} book${res.tagged === 1 ? "" : "s"} queued for tagging.`;
            if (res.errors?.length) sayApply(`${done} Not done: ${res.errors.join("; ")}`, true);
            else {
                sayApply(done);
                setTimeout(() => location.reload(), 1200);
            }
        })
        .catch((err: unknown) => sayApply(errorMessage(err), true));
});

maybeId("hc-dismiss")?.addEventListener("click", () => {
    const ids = ticked().map(idOf).filter((n) => n > 0);
    if (!ids.length) {
        sayApply("Tick the suggestions to dismiss.", true);
        return;
    }
    api("POST", "/api/hardcover/apply", { series, placeholders: [], tag: [], dismiss: ids })
        .then(() => location.reload())
        .catch((err: unknown) => sayApply(errorMessage(err), true));
});

maybeId("hc-undismiss")?.addEventListener("click", () => {
    api("POST", "/api/hardcover/undismiss", { series })
        .then(() => location.reload())
        .catch((err: unknown) => sayApply(errorMessage(err), true));
});

// A request for this series already waiting (from before): follow it.
const waiting = page ? document.querySelector<HTMLElement>(".hc-job[data-job]") : null;
if (waiting && page) {
    const id = Number(waiting.dataset["job"]);
    api<{ jobs: HardcoverJob[] }>("GET", `/api/hardcover/jobs?ids=${id}`)
        .then(async (res) => {
            const job = res.jobs[0];
            if (!job) return;
            const end = await follow(job);
            if (end.state === "done" && end.kind !== "candidates") location.reload();
        })
        .catch((err: unknown) => say(errorMessage(err), true));
}

// ---------------------------------------------------------------------------
// An author's series
// ---------------------------------------------------------------------------

const authorPage = maybeId("hc-author");

function rowState(name: string, text: string): void {
    for (const li of document.querySelectorAll<HTMLElement>(".hc-series li[data-series]")) {
        if (li.dataset["series"] === name) {
            const s = li.querySelector<HTMLElement>("[data-state]");
            if (s) s.textContent = text;
        }
    }
}

function describe(j: HardcoverJob): string {
    switch (j.state) {
        case "queued":
            return "queued";
        case "running":
            return "asking Hardcover…";
        case "held":
            return j.notBefore ? `waiting for Hardcover's limit (${new Date(j.notBefore * 1000).toUTCString()})` : "waiting for Hardcover's limit";
        case "failed":
            return `failed: ${j.error ?? ""}`;
    }
    const r = j.result;
    if (r?.linked) return `linked to ${r.hcName ?? "?"} (${r.books ?? 0} books)`;
    if (r?.needsChoice) return "more than one match: choose on its review page";
    return "done";
}

maybeId("hc-batch", HTMLButtonElement)?.addEventListener("click", (e) => {
    const btn = e.currentTarget as HTMLButtonElement;
    const names = [...document.querySelectorAll<HTMLInputElement>(".hc-series input.hc-tick:checked")].map((b) => b.value);
    const max = Number(authorPage?.dataset["max"] ?? "25");
    if (!names.length) {
        say("Tick the series to check.", true);
        return;
    }
    if (names.length > max) {
        say(`At most ${max} series at once.`, true);
        return;
    }
    btn.disabled = true;
    api<{ ids: number[]; usage: HardcoverUsage }>("POST", "/api/hardcover/batch", {
        author: authorPage?.dataset["author"] ?? "",
        series: names,
    })
        .then(async (res) => {
            showUsage(res.usage);
            say(`${res.ids.length} series queued. You can leave this page: requests that hit Hardcover's limit run when it resets.`);
            let open = res.ids;
            while (open.length) {
                await sleep(2000);
                const st = await api<{ jobs: HardcoverJob[]; usage: HardcoverUsage }>("GET", `/api/hardcover/jobs?ids=${open.join(",")}`);
                showUsage(st.usage);
                for (const j of st.jobs) rowState(j.series, describe(j));
                open = st.jobs.filter((j) => j.state !== "done" && j.state !== "failed").map((j) => j.id);
                if (st.jobs.every((j) => j.state === "held")) await sleep(28_000);
            }
            say("All checked. Open a series' review to apply its suggestions.");
        })
        .catch((err: unknown) => say(errorMessage(err), true))
        .finally(() => (btn.disabled = false));
});

// ---------------------------------------------------------------------------
// Waiting requests (every Hardcover page, and /missing)
// ---------------------------------------------------------------------------

for (const b of document.querySelectorAll<HTMLButtonElement>("button.hc-cancel")) {
    b.addEventListener("click", () => {
        b.disabled = true;
        api("DELETE", `/api/hardcover/jobs/${encodeURIComponent(b.dataset["job"] ?? "")}`)
            .then(() => b.closest("li")?.remove())
            .catch((err: unknown) => {
                b.disabled = false;
                alert(errorMessage(err));
            });
    });
}
