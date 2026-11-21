// The notifications page (templates/notifications.html, notifications.go):
// once shown they're seen, so the bar's bell (the sync button) goes; each can be dismissed.

import { api, errorMessage, maybeId, statusLine } from "./dom.js";

const say = statusLine("notifications-status");
const list = document.querySelector<HTMLElement>(".notification-list");

function emptied(): void {
    if (list?.querySelector(".notification")) return;
    list?.remove();
    maybeId("notifications-clear")?.remove();
    const empty = document.querySelector<HTMLElement>(".notifications-empty");
    if (empty) empty.hidden = false;
}

/** The bar's bell and count (nav.ts) go once they're seen. */
function bellSeen(): void {
    document.dispatchEvent(new CustomEvent("cf:notifications-seen"));
}

// Shown, so seen. nav.ts may draw the bell again from an answer that
// predates this (cf:me): it goes again.
let seen = !document.querySelector(".notification.is-new");
if (!seen) {
    api("POST", "/api/me/notifications/seen")
        .then(() => {
            seen = true;
            bellSeen();
        })
        .catch(() => {});
}
document.addEventListener("cf:me", () => {
    if (seen) setTimeout(bellSeen); // after nav.ts has drawn the bar
});

list?.addEventListener("click", (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".notification-dismiss") : null;
    const item = btn?.closest<HTMLElement>(".notification");
    const id = item?.dataset["id"];
    if (!btn || !item || !id) return;
    btn.disabled = true;
    api("DELETE", `/api/me/notifications/${encodeURIComponent(id)}`)
        .then(() => {
            item.remove();
            emptied();
        })
        .catch((err: unknown) => {
            btn.disabled = false;
            say(errorMessage(err), true);
        });
});

maybeId("notifications-clear")?.addEventListener("click", () => {
    api("DELETE", "/api/me/notifications")
        .then(() => {
            list?.querySelectorAll(".notification").forEach((n) => n.remove());
            emptied();
        })
        .catch((err: unknown) => say(errorMessage(err), true));
});
