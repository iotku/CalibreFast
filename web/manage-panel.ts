// The Manage sidebar of an aggregate page (templates/filtered.html), opened
// from "Manage Series" (or Author, Tag, …) at the top of the bar's Manage
// menu. It sits beside the page, which moves over for it (style.css), and
// stays open until closed (its ×, Esc, the menu entry again), however you
// use the page meanwhile: a book's popup can open over it. Closed, it keeps
// what you'd entered until you leave the page (or Cancel the group's). Its parts save on
// their own: the series' book counts (series-verify.ts, editors) and the
// group editor (groups.ts, admins), which also handles the Feature button.
import { byId, maybeId, modalOpen } from "./dom.js";
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
// Capture phase: seen before the book popup's own Esc closes it.
document.addEventListener(
    "keydown",
    (e) => {
        // Not while the book popup or a dialog has it: that Esc closes that.
        if (e.key !== "Escape" || panel.hidden || e.defaultPrevented || modalOpen()) return;
        // A list open in it (the group's suggestions) closes first.
        if (e.target instanceof HTMLElement && e.target.getAttribute("aria-expanded") === "true") return;
        setOpen(false);
    },
    true,
);
