// An author's page (templates/filtered.html): the first time it's opened,
// ask the server to look the author up on Hardcover (author_profile.go).
// The server answers the top of the page rendered again once Hardcover has
// (200), or nothing while the lookup waits in its queue (202): it is kept
// either way, and shows the next time the page is opened. Editors can have
// it looked up again.
import { errorMessage } from "./dom.js";
import { serverHtml } from "./trusted.js";

const slot = document.getElementById("agg-header");

/** POST url; replace the header with the answer when there is one. */
async function ask(url: string): Promise<void> {
    if (!slot || !url.startsWith("/api/authors/profile")) return;
    const res = await fetch(url, { method: "POST", credentials: "same-origin" });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    if (res.status !== 200) return; // still queued
    const html = await res.text();
    if (slot.isConnected) serverHtml(slot, html); // html/template output from our server
}

if (slot) {
    const lookup = slot.dataset["profileLookup"];
    if (lookup) {
        delete slot.dataset["profileLookup"];
        ask(lookup).catch((err: unknown) => console.warn("author profile:", errorMessage(err)));
    }
    slot.addEventListener("click", (e) => {
        const btn = (e.target as Element).closest<HTMLButtonElement>("button[data-profile-refresh]");
        const url = btn?.dataset["profileRefresh"];
        if (!btn || !url) return;
        btn.disabled = true;
        btn.textContent = "Looking up…";
        ask(url)
            .then(() => {
                if (btn.isConnected) btn.textContent = "Queued: shows when it's done";
            })
            .catch((err: unknown) => {
                btn.disabled = false;
                btn.textContent = `Look up again (${errorMessage(err)})`;
            });
    });
}
