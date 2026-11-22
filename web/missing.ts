// /missing (templates/missing.html, placeholders.go): restoring a placeholder
// from the list of recently removed ones, say one a library book replaced in
// error. The restore is in the edit log, so it can be undone too.

import { api, errorMessage, statusLine } from "./dom.js";

const say = statusLine("ph-removed-status");

document.querySelector("#ph-removed")?.addEventListener("click", async (e) => {
    const btn = (e.target as Element).closest<HTMLButtonElement>("button.ph-restore");
    const row = btn?.closest<HTMLElement>("li.ph-removed");
    const id = row?.dataset["uuid"];
    if (!btn || !row || !id) return;
    btn.disabled = true;
    try {
        await api("POST", `/api/placeholders/${encodeURIComponent(id)}/restore`);
        const title = row.querySelector(".ph-title")?.textContent ?? "The placeholder";
        row.remove();
        say(`${title} is a placeholder again. Reload to see it in its series.`);
    } catch (err) {
        btn.disabled = false;
        say(`Couldn't restore it: ${errorMessage(err)}`, true);
    }
});
