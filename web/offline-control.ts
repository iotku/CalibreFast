/**
 * The "on this device" line on a book's page and popup (templates/book.html,
 * .book-offline): whether the book (its EPUB, its PDF, or both) is kept in
 * this browser for reading with no connection (book-cache.ts), and a choice of
 *   - Keep while reading: kept once opened, dropped after a week unopened
 *     (the default);
 *   - Always keep on this device: downloaded now (the format last read,
 *     last-format.ts, else the EPUB), never dropped for age;
 *   - Don't keep on this device: the reader keeps nothing, and a kept copy
 *     goes.
 * Loaded with user.ts, which runs wherever a book page or popup can show.
 */

import { downloadForOffline, getBookFiles, offlinePref, setOfflinePref, type KeptFormat, type OfflinePref } from "./book-cache.js";
import { preferredFormat } from "./last-format.js";
import { keepCover } from "./offline-books.js";

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
        state.textContent = `On this device (${which}${megabytes(size)})${pref === "keep" ? ", kept for good" : ""}`;
    } else {
        state.textContent = pref === "never" ? "Not kept on this device" : "Not on this device";
    }
    box.hidden = false;
}

async function choose(box: HTMLElement, pref: OfflinePref): Promise<void> {
    const uuid = box.dataset["uuid"] ?? "";
    await setOfflinePref(uuid, pref);
    if (pref === "keep" && !(await getBookFiles(uuid).catch(() => [])).length && !downloading.has(uuid)) {
        downloading.set(uuid, 0);
        void show(box);
        try {
            await downloadForOffline(uuid, formatToKeep(box, uuid), box.dataset["title"] ?? "", box.dataset["authors"] ?? "", (f) => {
                downloading.set(uuid, f);
                for (const b of boxesFor(uuid)) void show(b);
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
