/**
 * Marks the library's cards (.book[data-uuid]) of books kept on this device
 * for reading offline (book-cache.ts): .is-offline, a small badge on the
 * cover (style.css). Cards come and go as the grid scrolls, so any card
 * added to the page is marked as it arrives.
 */

import { keptUuids } from "./book-cache.js";

const canKeep = isSecureContext && "indexedDB" in window;
let kept = new Set<string>();

function mark(card: HTMLElement): void {
    const on = kept.has(card.dataset["uuid"] ?? "");
    if (card.classList.contains("is-offline") === on) return;
    card.classList.toggle("is-offline", on);
    if (on) card.title = "On this device: reads with no connection";
    else card.removeAttribute("title");
}

function markAll(root: ParentNode = document): void {
    for (const card of root.querySelectorAll<HTMLElement>(".book[data-uuid]")) mark(card);
}

async function refresh(): Promise<void> {
    kept = await keptUuids().catch(() => new Set<string>());
    markAll();
}

if (canKeep) {
    new MutationObserver((records) => {
        if (!kept.size) return;
        for (const r of records) {
            for (const n of r.addedNodes) {
                if (!(n instanceof HTMLElement)) continue;
                if (n.matches(".book[data-uuid]")) mark(n);
                else if (n.firstElementChild) markAll(n);
            }
        }
    }).observe(document.documentElement, { childList: true, subtree: true });
    // Kept or dropped here (the popup's choice, a sweep), or in the reader
    // in another tab (seen when this one is shown again).
    window.addEventListener("cf:offline-changed", () => void refresh());
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "visible") void refresh();
    });
    void refresh();
}
