// Save on a placeholder's page and in its popup (placeholder.html): a book
// not in the library yet, Saved like any book (its status "wanted"), so its
// saver is told when it arrives (notifications.go). Delegated, as the popup
// arrives later (modal.ts loads it); user.ts imports this for the popup, and
// placeholder.html loads it for the page.

import { api, el } from "./dom.js";
import { lastKnownUser } from "./local-keys.js";
import { ICONS, cachedMarks, saveMarks, type BookChange } from "./reading.js";
import { noteMark } from "./pending-marks.js";
import { sendPending } from "./send-pending.js";

function paint(btn: HTMLButtonElement, on: boolean): void {
    btn.replaceChildren(on ? ICONS.wanted() : ICONS.want(), el("span", {}, on ? "Saved" : "Save"));
    btn.classList.toggle("is-on", on);
    btn.setAttribute("aria-pressed", String(on));
    btn.title = on ? "Remove from Saved" : "Save it, and be told when it's in the library";
}

const buttons = (uuid: string): NodeListOf<HTMLButtonElement> =>
    document.querySelectorAll<HTMLButtonElement>(`button[data-placeholder-save="${CSS.escape(uuid)}"]`);

function paintAll(root: ParentNode): void {
    root.querySelectorAll<HTMLButtonElement>("button[data-placeholder-save]").forEach((b) => {
        if (!b.dataset["painted"]) {
            b.dataset["painted"] = "1";
            paint(b, b.getAttribute("aria-pressed") === "true");
        }
    });
}

document.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>("button[data-placeholder-save]") : null;
    const uuid = btn?.dataset["placeholderSave"];
    if (!btn || !uuid) return;
    const on = btn.getAttribute("aria-pressed") !== "true";
    buttons(uuid).forEach((b) => paint(b, on));
    const marks = cachedMarks();
    const user = marks.user ?? lastKnownUser();
    if (!user) {
        // Nothing known here yet about who's signed in: straight to the server.
        api("PUT", `/api/me/books/${encodeURIComponent(uuid)}/status`, { status: on ? "wanted" : "" }).catch(() =>
            buttons(uuid).forEach((b) => paint(b, !on)),
        );
        return;
    }
    if (marks.user) {
        if (on) marks.wanted.add(uuid);
        else marks.wanted.delete(uuid);
        saveMarks(marks);
    }
    noteMark(user, uuid, { status: on ? "wanted" : "" });
    void sendPending(user);
});

// The server's answer (pending-marks.ts), should it differ.
document.addEventListener("cf:status", (e) => {
    const c = (e as CustomEvent<BookChange>).detail;
    if (c.status !== undefined) buttons(c.uuid).forEach((b) => paint(b, c.status === "wanted"));
});

new MutationObserver((records) => {
    for (const r of records) for (const n of r.addedNodes) if (n instanceof HTMLElement) paintAll(n.parentNode ?? n);
}).observe(document.documentElement, { childList: true, subtree: true });
paintAll(document);
