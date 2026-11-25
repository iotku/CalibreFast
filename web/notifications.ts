// The notifications page (templates/notifications.html, notifications.go):
// once shown they're seen, so the bar's bell (the sync button) goes; each can be dismissed.
// A dismissed one moves to "Earlier", read only (an account one, such as
// "Link Hardcover again", is deleted instead).

import { api, errorMessage, maybeId, statusLine } from "./dom.js";

const say = statusLine("notifications-status");
const list = maybeId("notifications-active");
const earlier = maybeId("notifications-earlier");

function emptied(): void {
    if (list?.querySelector(".notification")) return;
    list?.remove();
    maybeId("notifications-clear")?.remove();
    const empty = document.querySelector<HTMLElement>(".notifications-empty");
    if (empty) empty.hidden = false;
}

/** item, dismissed: to the top of Earlier, without its Dismiss button; an account one goes. */
function toEarlier(item: HTMLElement): void {
    const into = earlier?.querySelector("ul");
    if (item.dataset["account"] || !earlier || !into) {
        item.remove();
        return;
    }
    item.querySelector(".notification-dismiss")?.remove();
    item.classList.remove("is-new");
    into.prepend(item);
    earlier.hidden = false;
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
            toEarlier(item);
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
            // Oldest first, so the newest ends up on top of Earlier.
            const items = [...(list?.querySelectorAll<HTMLElement>(".notification") ?? [])].reverse();
            for (const item of items) toEarlier(item);
            emptied();
        })
        .catch((err: unknown) => say(errorMessage(err), true));
});
