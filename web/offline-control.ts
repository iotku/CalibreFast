/**
 * The "on this device" line on a book's page and popup (templates/book.html,
 * .book-offline): whether the book (its EPUB, its PDF, or both) is kept in
 * this browser for reading with no connection (book-cache.ts), and a choice of
 *   - Keep while reading: kept by this device's rules (retention.ts, set on
 *     /preferences; the default);
 *   - Always keep on this device: downloaded now (the preferred format,
 *     last-format.ts, else the EPUB), never dropped for age, room or size.
 *     Only while the book is on one of the user's lists, so nothing sits
 *     on the device forgotten: a book with no status is added to Saved
 *     first, and taking it off every list (Not Reading) drops it;
 *   - Don't keep on this device: the reader keeps nothing, and a kept copy
 *     goes.
 * Loaded with user.ts, which runs wherever a book page or popup can show.
 */

import { currentUser, getBookFiles, offlinePref, setOfflinePref, updateBook, type KeptFormat, type OfflinePref } from "./book-cache.js";
import { api } from "./dom.js";
import { preferredFormat } from "./last-format.js";
import { keepCover } from "./offline-books.js";
import { devicePrefs } from "./prefs.js";
import { applyChange, cachedMarks, saveMarks, type BookChange } from "./reading.js";
import { downloadForOffline, fitsBookLimit, requestPersist } from "./retention.js";

/** Whether this browser can keep books at all (IndexedDB, and the service worker to read them offline). */
const canKeep = isSecureContext && "indexedDB" in window && "serviceWorker" in navigator;

const megabytes = (n: number): string => `${(n / 1048576).toFixed(n < 10485760 ? 1 : 0)} MB`;

/** The book's formats that the readers open. */
const readable = (box: HTMLElement): KeptFormat[] =>
    (["epub", "pdf"] as const).filter((f) => (box.dataset["formats"] ?? "").split(" ").includes(f));

/** The format to download for "Always keep". */
function formatToKeep(box: HTMLElement, uuid: string): KeptFormat {
    const has = readable(box);
    return preferredFormat(uuid, has) ?? has[0] ?? "epub";
}

/** Downloads in progress, by book, so a popup opened again shows it. */
const downloading = new Map<string, number>();
/** Books just added to Saved to be pinned, to say so. */
const addedToSaved = new Set<string>();

/**
 * A pinned book must be on one of the user's lists: with no status, add it
 * to Saved (and tell the controls and the grid). Its status ("" for a
 * guest, whose books have none); null if that failed.
 */
async function ensureListed(box: HTMLElement, uuid: string): Promise<string | null> {
    if (currentUser() === null) return "";
    const path = `/api/me/books/${encodeURIComponent(uuid)}`;
    try {
        const { status } = await api<{ status: string }>("GET", path);
        if (status) return status;
        await api("PUT", `${path}/status`, { status: "wanted" });
    } catch {
        return null;
    }
    addedToSaved.add(uuid);
    const change: BookChange = { uuid, status: "wanted" };
    const marks = cachedMarks();
    if (marks.signedIn && applyChange(marks, change)) saveMarks(marks);
    document.dispatchEvent(new CustomEvent<BookChange>("cf:status", { detail: change }));
    // The book's own status control, if it's showing: it saves again (the same) and repaints.
    const select = box.closest(".book-page")?.querySelector<HTMLSelectElement>(".book-status");
    if (select && select.value !== "wanted") {
        select.value = "wanted";
        select.dispatchEvent(new Event("change"));
    }
    return "wanted";
}

async function show(box: HTMLElement): Promise<void> {
    const uuid = box.dataset["uuid"] ?? "";
    const state = box.querySelector<HTMLElement>(".book-offline-state");
    const select = box.querySelector<HTMLSelectElement>(".book-offline-pref");
    if (!state || !select) return;
    const pref = offlinePref(uuid);
    const kept = await getBookFiles(uuid).catch(() => []);
    select.value = pref;
    box.classList.toggle("is-kept", kept.length > 0);
    const progress = downloading.get(uuid);
    if (progress !== undefined) {
        state.textContent = `Downloading for offline… ${Math.round(progress * 100)}%`;
    } else if (kept.length) {
        // Which file, when the book has more than one the readers open.
        const which = readable(box).length > 1 ? `${kept.map((b) => (b.format ?? "epub").toUpperCase()).join(" and ")}, ` : "";
        const size = kept.reduce((n, b) => n + b.size, 0);
        const limit = devicePrefs().maxBookBytes;
        const big = pref === "keep" && !fitsBookLimit(Math.max(...kept.map((b) => b.size)));
        state.textContent =
            `On this device (${which}${megabytes(size)})${pref === "keep" ? ", kept for good" : ""}` +
            (addedToSaved.has(uuid) ? ". Added to Saved" : "") +
            (big ? `. Larger than your ${megabytes(limit)} limit; kept because you chose to` : "");
    } else {
        state.textContent = pref === "never" ? "Not kept on this device" : "Not on this device";
    }
    box.hidden = false;
}

async function choose(box: HTMLElement, pref: OfflinePref): Promise<void> {
    const uuid = box.dataset["uuid"] ?? "";
    const status = pref === "keep" ? await ensureListed(box, uuid) : "";
    if (status === null) {
        for (const b of boxesFor(uuid)) {
            void show(b).then(() => {
                const state = b.querySelector<HTMLElement>(".book-offline-state");
                if (state) state.textContent = "Couldn't add it to Saved, so it isn't kept. Try again.";
            });
        }
        return;
    }
    await setOfflinePref(uuid, pref);
    if (pref === "keep") requestPersist();
    // The status it's pinned with, so taking it off every list later drops it (retention.ts).
    if (status) {
        for (const b of await getBookFiles(uuid).catch(() => [])) await updateBook(uuid, b.format ?? "epub", { statusSeen: status });
    }
    if (pref === "keep" && !(await getBookFiles(uuid).catch(() => [])).length && !downloading.has(uuid)) {
        downloading.set(uuid, 0);
        void show(box);
        try {
            await downloadForOffline(uuid, formatToKeep(box, uuid), box.dataset["title"] ?? "", box.dataset["authors"] ?? "", {
                onProgress: (f) => {
                    downloading.set(uuid, f);
                    for (const b of boxesFor(uuid)) void show(b);
                },
                ...(status ? { statusSeen: status } : {}),
            });
            void keepCover(uuid).catch(() => {}); // for the offline Reading page
        } catch {
            downloading.delete(uuid);
            for (const b of boxesFor(uuid)) {
                const state = b.querySelector<HTMLElement>(".book-offline-state");
                if (state) state.textContent = "Couldn't download it. It'll be kept next time it's opened.";
            }
            return;
        }
        downloading.delete(uuid);
    }
    for (const b of boxesFor(uuid)) void show(b);
}

const boxesFor = (uuid: string): HTMLElement[] =>
    [...document.querySelectorAll<HTMLElement>(".book-offline")].filter((b) => b.dataset["uuid"] === uuid);

function setUp(box: HTMLElement): void {
    if (!canKeep || box.dataset["ready"]) return;
    box.dataset["ready"] = "1";
    box.querySelector<HTMLSelectElement>(".book-offline-pref")?.addEventListener("change", (e) => {
        void choose(box, (e.target as HTMLSelectElement).value as OfflinePref);
    });
    void show(box);
}

function scan(root: Node): void {
    if (!(root instanceof HTMLElement)) return;
    if (root.matches(".book-offline")) setUp(root);
    root.querySelectorAll<HTMLElement>(".book-offline").forEach(setUp);
}

// A copy kept or dropped elsewhere (the reader in another tab is another
// page, but a sweep or a download here isn't): show it.
window.addEventListener("cf:offline-changed", (e) => {
    const uuid = (e as CustomEvent<string>).detail;
    if (downloading.has(uuid)) return;
    for (const b of boxesFor(uuid)) void show(b);
});

new MutationObserver((records) => {
    for (const r of records) for (const n of r.addedNodes) scan(n);
}).observe(document.documentElement, { childList: true, subtree: true });
scan(document.body);
