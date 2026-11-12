/**
 * /account/import (templates/account-import.html): review the reading done
 * on this device while signed out, and add it to the account
 * (guest_import.go; docs/notes/Discover.md section 5b).
 *
 *   1. The guest records (guest-data.ts) go to /api/me/import/preview, which
 *      says per book: new, the same as the account has, a position that
 *      differs from the account's, or no longer in the library.
 *   2. Books the account already has, and books gone from the library, are
 *      cleared from this device at once: there's nothing to decide.
 *   3. The rest are listed, ticked. Where the place differs, both are shown
 *      with the newer chosen; the user can pick the other.
 *   4. "Add to my account" sends the ticked ones; what was added is cleared
 *      from this device. "Discard" clears them without adding.
 */

import { byId } from "./dom.js";
import type { Book } from "./types.js";
import { coverUrl, escapeHtml } from "./utils.js";
import { collectGuestData, forgetGuestBooks, type GuestBook } from "./guest-data.js";

interface Position {
    format: string;
    percent: number;
    updated_at: number;
    device?: string;
}

interface Preview {
    uuid: string;
    book?: Book;
    state: "new" | "same" | "conflict" | "missing";
    device?: Position;
    account?: Position;
    new_annotations: number;
    merged_notes: number;
    new_bookmarks: number;
    status: string;
}

const statusEl = byId("import-status");
const listEl = byId("import-list");
const actionsEl = byId("import-actions");
const applyBtn = byId("import-apply", HTMLButtonElement);
const discardBtn = byId("import-discard", HTMLButtonElement);

let guest = new Map<string, GuestBook>();
let previews: Preview[] = [];

async function post<T>(url: string, body: unknown): Promise<T> {
    const res = await fetch(url, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`${res.status}`);
    return (await res.json()) as T;
}

function ago(ms: number): string {
    if (!ms) return "";
    const s = Math.max(0, (Date.now() - ms) / 1000);
    if (s < 90) return "just now";
    if (s < 3600) return `${plural(Math.round(s / 60), "minute", "minutes")} ago`;
    if (s < 86400) return `${plural(Math.round(s / 3600), "hour", "hours")} ago`;
    if (s < 2 * 86400) return "yesterday";
    if (s < 30 * 86400) return `${plural(Math.round(s / 86400), "day", "days")} ago`;
    return new Date(ms).toLocaleDateString();
}

function place(p: Position): string {
    const pct = `${Math.round(p.percent * 100)}%`;
    const where = p.device ? ` on ${p.device}` : "";
    return `${pct}${where}${p.updated_at ? ` · ${ago(p.updated_at)}` : ""}`;
}

function plural(n: number, one: string, many: string): string {
    return `${n} ${n === 1 ? one : many}`;
}

function done(text: string): void {
    statusEl.textContent = text;
    const home = document.createElement("a");
    home.href = "/";
    home.textContent = "Back to the library";
    statusEl.append(" ", home);
    actionsEl.hidden = true;
}

function render(): void {
    listEl.replaceChildren();
    for (const p of previews) {
        const b = p.book!;
        const li = document.createElement("li");
        li.className = "import-book";
        li.dataset["uuid"] = p.uuid;
        const extras: string[] = [];
        if (p.new_annotations) extras.push(plural(p.new_annotations, "highlight", "highlights"));
        if (p.new_bookmarks) extras.push(plural(p.new_bookmarks, "bookmark", "bookmarks"));
        let html = `
            <label class="import-include">
                <input type="checkbox" checked aria-label="Add ${escapeHtml(b.title)}" />
            </label>
            <img class="import-cover" src="${escapeHtml(coverUrl(b))}" alt="" loading="lazy" />
            <div class="import-info">
                <div class="import-title">${escapeHtml(b.title)}</div>
                <div class="import-author">${escapeHtml(b.authors)}</div>`;
        if (p.state === "conflict" && p.device && p.account) {
            const deviceNewer = p.device.updated_at >= p.account.updated_at;
            const name = `place-${p.uuid}`;
            html += `
                <fieldset class="import-choice">
                    <legend>Your place differs. Keep:</legend>
                    <label><input type="radio" name="${escapeHtml(name)}" value="device"${deviceNewer ? " checked" : ""} /> This device: ${escapeHtml(place(p.device))}</label>
                    <label><input type="radio" name="${escapeHtml(name)}" value="account"${deviceNewer ? "" : " checked"} /> Your account: ${escapeHtml(place(p.account))}</label>
                </fieldset>`;
        } else if (p.device) {
            html += `<div class="import-line">Place: ${escapeHtml(place(p.device))}${p.device.percent >= 0.97 ? " (finished)" : ""}</div>`;
        }
        if (extras.length) html += `<div class="import-line">${escapeHtml(extras.join(", "))} to add</div>`;
        if (p.merged_notes) {
            html += `<div class="import-line">${escapeHtml(plural(p.merged_notes, "note", "notes"))} added to highlights you already have</div>`;
        }
        html += `</div>`;
        li.innerHTML = html;
        // A cover that isn't there: the box stays, empty.
        li.querySelector("img")?.addEventListener("error", (e) => (e.target as HTMLImageElement).classList.add("missing"));
        listEl.append(li);
    }
}

async function start(): Promise<void> {
    const books = collectGuestData();
    guest = new Map(books.map((b) => [b.uuid, b]));
    if (!books.length) return done("There's no reading from this device to add.");
    let all: Preview[];
    try {
        all = (await post<{ books: Preview[] }>("/api/me/import/preview", { books })).books;
    } catch {
        statusEl.textContent = "Couldn't compare with your account. Try again in a moment.";
        return;
    }
    // Nothing to decide: already in the account, or gone from the library.
    forgetGuestBooks(all.filter((p) => p.state === "same" || p.state === "missing").map((p) => p.uuid));
    previews = all.filter((p) => (p.state === "new" || p.state === "conflict") && p.book);
    if (!previews.length) return done("Your account already has everything read on this device.");

    const conflicts = previews.filter((p) => p.state === "conflict").length;
    statusEl.textContent =
        `${plural(previews.length, "book", "books")} from this device` +
        (conflicts ? `; for ${plural(conflicts, "book", "books")} your place differs from your account's.` : ".");
    render();
    actionsEl.hidden = false;
}

applyBtn.addEventListener("click", async () => {
    const chosen = [...listEl.querySelectorAll<HTMLElement>(".import-book")].filter(
        (li) => li.querySelector<HTMLInputElement>(".import-include input")?.checked,
    );
    if (!chosen.length) {
        statusEl.textContent = "Tick the books to add.";
        return;
    }
    const books = chosen.map((li) => {
        const uuid = li.dataset["uuid"] ?? "";
        const p = previews.find((x) => x.uuid === uuid);
        const pick = li.querySelector<HTMLInputElement>(".import-choice input:checked")?.value;
        return { ...guest.get(uuid)!, use_position: p?.state === "new" || pick === "device" };
    });
    applyBtn.disabled = discardBtn.disabled = true;
    statusEl.textContent = "Adding…";
    try {
        const { results } = await post<{ results: { uuid: string; error?: string }[] }>("/api/me/import", { books });
        const ok = results.filter((r) => !r.error).map((r) => r.uuid);
        const failed = results.filter((r) => r.error);
        forgetGuestBooks(ok);
        for (const uuid of ok) listEl.querySelector(`.import-book[data-uuid="${CSS.escape(uuid)}"]`)?.remove();
        previews = previews.filter((p) => !ok.includes(p.uuid));
        if (failed.length) {
            statusEl.textContent = `Added ${plural(ok.length, "book", "books")}; ${plural(failed.length, "book", "books")} couldn't be added. Try again.`;
        } else if (previews.length) {
            statusEl.textContent = `Added ${plural(ok.length, "book", "books")}. The unticked ones are still on this device.`;
        } else {
            done(`Added ${plural(ok.length, "book", "books")} to your account.`);
        }
    } catch {
        statusEl.textContent = "Couldn't add them. Nothing was lost; try again in a moment.";
    } finally {
        applyBtn.disabled = discardBtn.disabled = false;
    }
});

discardBtn.addEventListener("click", () => {
    const n = previews.length;
    if (!confirm(`Remove this device's reading for ${plural(n, "book", "books")}? Your account isn't changed, and this can't be undone.`)) return;
    forgetGuestBooks(previews.map((p) => p.uuid));
    previews = [];
    listEl.replaceChildren();
    done("This device's reading was removed.");
});

void start();
