// Bookshelves page: create, rename, describe and delete shelves.
import { api, errorMessage, statusLine } from "./dom.js";
const say = statusLine("shelf-status");
for (const form of document.querySelectorAll(".shelf-create")) {
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        const name = form.querySelector("input[name=name]")?.value ?? "";
        try {
            await api("POST", "/api/shelves", { name, global: form.dataset["global"] === "true" });
            location.reload();
        }
        catch (err) {
            say(errorMessage(err), true);
        }
    });
}
document.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest(".shelf-row button[data-action]") : null;
    if (!btn)
        return;
    const row = btn.closest(".shelf-row");
    if (!row)
        return;
    const { id = "", name = "", description = "" } = row.dataset;
    try {
        if (btn.dataset["action"] === "rename") {
            const next = prompt("Shelf name", name);
            if (!next || next === name)
                return;
            await api("PATCH", `/api/shelves/${id}`, { name: next });
            location.reload();
        }
        else if (btn.dataset["action"] === "describe") {
            const next = prompt("Description (optional)", description);
            if (next === null)
                return;
            await api("PATCH", `/api/shelves/${id}`, { description: next });
            location.reload();
        }
        else if (btn.dataset["action"] === "delete") {
            if (!confirm(`Delete the shelf “${name}”? The books themselves aren't affected.`))
                return;
            await api("DELETE", `/api/shelves/${id}`);
            row.remove();
            say(`Deleted “${name}”.`);
        }
    }
    catch (err) {
        say(errorMessage(err), true);
    }
});
