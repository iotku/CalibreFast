// The Manage sidebar of an aggregate page (templates/filtered.html), opened
// from "Manage Series" (or Author, Tag, …) at the top of the bar's Manage
// menu. It stays open while you work in it (no click outside closes it);
// it closes with its ×, Esc, or the menu entry again. Its parts save on
// their own: the series' book counts (series-verify.ts, editors) and the
// group editor (groups.ts, admins), which also handles the Feature button.
import { byId, maybeId } from "./dom.js";
import "./series-verify.js";
import "./groups.js";

const panel = byId("manage-panel");
const toggle = maybeId("manage-toggle", HTMLButtonElement);

function setOpen(open: boolean): void {
    if (open === !panel.hidden) return;
    panel.hidden = !open;
    toggle?.setAttribute("aria-expanded", String(open));
    if (open) {
        panel.dispatchEvent(new CustomEvent("cf:manage-open"));
        panel.querySelector<HTMLElement>(".manage-panel-body input:not([type=file]):not([disabled]), .manage-panel-body textarea")?.focus();
    } else if (panel.contains(document.activeElement)) {
        toggle?.closest(".tb-actions")?.querySelector<HTMLElement>(".tb-manage-btn")?.focus();
    }
}

toggle?.addEventListener("click", () => setOpen(panel.hidden !== false));
panel.querySelector(".manage-panel-close")?.addEventListener("click", () => setOpen(false));
// A section's ⓘ: its explanation, shown or hidden.
for (const btn of panel.querySelectorAll<HTMLButtonElement>(".manage-info-btn")) {
    const info = document.getElementById(btn.getAttribute("aria-controls") ?? "");
    btn.addEventListener("click", () => {
        if (!info) return;
        info.hidden = !info.hidden;
        btn.setAttribute("aria-expanded", String(!info.hidden));
    });
}
// The group editor's Cancel.
panel.addEventListener("cf:manage-close", () => setOpen(false));
document.addEventListener("keydown", (e) => {
    // Not while a dialog (the book modal, a picker) has it.
    if (e.key !== "Escape" || panel.hidden || e.defaultPrevented || document.querySelector("dialog[open]")) return;
    setOpen(false);
});
