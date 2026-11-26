// A shelf's own page (/shelf/N, templates/filtered.html): Pin to home, Edit
// shelf (its owner, or admins on a global shelf: shelf-edit.ts), and for
// admins on a global shelf, Feature; a tag's page (/tag/X) and a series'
// (/series/X): Pin to home (shelf_pins.go, series_pins.go).
import { api, errorMessage } from "./dom.js";
import { editShelf } from "./shelf-edit.js";

const LABELS: Record<string, [string, string]> = {
    pin: ["Pin to home", "On home"],
    "pin-tag": ["Pin to home", "On home"],
    "pin-series": ["Pin to home", "On home"],
    feature: ["Feature", "Featured"],
};

for (const btn of document.querySelectorAll<HTMLButtonElement>(".shelf-page-btn")) {
    btn.addEventListener("click", async () => {
        const action = btn.dataset["shelfAction"] ?? "";
        const id = btn.dataset["id"] ?? "";
        if (action === "edit") {
            const res = await editShelf({
                id,
                name: btn.dataset["name"] ?? "",
                description: btn.dataset["description"] ?? "",
                global: btn.dataset["global"] === "true",
                shared: btn.dataset["shared"] === "true",
            });
            if (res?.deleted) location.assign("/shelves");
            else if (res) location.reload();
            return;
        }
        const on = btn.getAttribute("aria-pressed") !== "true";
        btn.disabled = true;
        try {
            if (action === "pin") await api(on ? "PUT" : "DELETE", `/api/me/pins/${id}`);
            else if (action === "pin-tag") await api(on ? "PUT" : "DELETE", `/api/me/tag-pins/${encodeURIComponent(btn.dataset["tag"] ?? "")}`);
            else if (action === "pin-series") await api(on ? "PUT" : "DELETE", `/api/me/series-pins/${encodeURIComponent(btn.dataset["series"] ?? "")}`);
            else await api("PATCH", `/api/shelves/${id}`, { featured: on });
            btn.setAttribute("aria-pressed", String(on));
            const label = btn.querySelector(".tb-label");
            if (label) label.textContent = LABELS[action]?.[on ? 1 : 0] ?? "";
        } catch (err) {
            alert(errorMessage(err));
        } finally {
            btn.disabled = false;
        }
    });
}
