// Bookshelves page: create, rename, describe and delete shelves; pin them
// to the home page, feature global ones (admins), and order your own
// shelves and the featured ones (web/reorder.ts).
import { api, errorMessage, statusLine } from "./dom.js";
import { makeReorderable } from "./reorder.js";

const say = statusLine("shelf-status");

for (const form of document.querySelectorAll<HTMLFormElement>(".shelf-create")) {
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        const name = form.querySelector<HTMLInputElement>("input[name=name]")?.value ?? "";
        try {
            await api("POST", "/api/shelves", { name, global: form.dataset["global"] === "true" });
            location.reload();
        } catch (err) {
            say(errorMessage(err), true);
        }
    });
}

/** Pin to home, or take off it. In place: the button says which. */
async function togglePin(btn: HTMLButtonElement, id: string): Promise<void> {
    const on = btn.getAttribute("aria-pressed") !== "true";
    await api(on ? "PUT" : "DELETE", `/api/me/pins/${id}`);
    btn.setAttribute("aria-pressed", String(on));
    btn.textContent = on ? "On home" : "Pin to home";
}

document.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".shelf-row button[data-action]") : null;
    if (!btn) return;
    const row = btn.closest<HTMLElement>(".shelf-row");
    if (!row) return;
    const { id = "", name = "", description = "" } = row.dataset;
    try {
        switch (btn.dataset["action"]) {
            case "pin":
                await togglePin(btn, id);
                break;
            case "feature":
                await api("PATCH", `/api/shelves/${id}`, { featured: btn.getAttribute("aria-pressed") !== "true" });
                location.reload(); // the Featured list changes too
                break;
            case "rename": {
                const next = prompt("Shelf name", name);
                if (!next || next === name) return;
                await api("PATCH", `/api/shelves/${id}`, { name: next });
                location.reload();
                break;
            }
            case "describe": {
                const next = prompt("Description (optional)", description);
                if (next === null) return;
                await api("PATCH", `/api/shelves/${id}`, { description: next });
                location.reload();
                break;
            }
            case "delete":
                if (!confirm(`Delete the shelf “${name}”? The books themselves aren't affected.`)) return;
                await api("DELETE", `/api/shelves/${id}`);
                row.remove();
                say(`Deleted “${name}”.`);
                break;
        }
    } catch (err) {
        say(errorMessage(err), true);
    }
});

// Your shelves, and (admins) the featured ones: drag or Move up/down.
for (const list of document.querySelectorAll<HTMLElement>(".shelf-order")) {
    const url = list.dataset["order"] ?? "";
    if (!url || list.querySelectorAll(":scope > .shelf-row").length < 2) continue;
    makeReorderable({
        list,
        items: ".shelf-row",
        label: (el) => el.dataset["name"] ?? "",
        onChange: (ids) => {
            api("PUT", url, { ids: ids.map(Number) }).then(
                () => say("Order saved."),
                (err: unknown) => say(errorMessage(err), true),
            );
        },
    });
}
