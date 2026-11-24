// Sync to Hardcover on a placeholder's page (placeholder.html,
// hardcover_placeholder.go): the book page's control (hardcover-book.ts).
// Saved, the placeholder is sent as want to read, so Save and Unsave
// change how its sync stands: it's looked at again after each.

import { wireHardcoverBook } from "./hardcover-book.js";

const box = document.querySelector<HTMLElement>("[data-ph-hardcover]");
const uuid = box?.dataset["phHardcover"];
const refresh = box && uuid ? wireHardcoverBook(box, uuid) : null;

if (refresh && uuid) {
    document.addEventListener("click", (e) => {
        const save = e.target instanceof Element ? e.target.closest<HTMLElement>("[data-placeholder-save]") : null;
        if (save?.dataset["placeholderSave"] !== uuid) return;
        // The status goes to the server, then the worker sends it.
        setTimeout(refresh, 1500);
        setTimeout(refresh, 5000);
    });
}
