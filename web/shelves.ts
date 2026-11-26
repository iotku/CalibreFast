// Bookshelves page: create shelves, edit them (name, description, sharing,
// delete: shelf-edit.ts), pin them to the home page, feature global ones
// (admins), find one by name, and arrange your own shelves and the
// featured ones (web/reorder.ts); answer invites to help with someone's
// shelf, leave one, and unblock people (shelf_members.go).
import { startBookSync } from "./book-sync.js";
import { api, el, errorMessage, maybeId, statusLine } from "./dom.js";
import { makeReorderable, type Reorderable } from "./reorder.js";
import { editShelf } from "./shelf-edit.js";

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
    await api(on ? "PUT" : "DELETE", `/api/me/pins/${encodeURIComponent(id)}`);
    btn.setAttribute("aria-pressed", String(on));
    btn.textContent = on ? "On home" : "Pin to home";
}

document.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".shelf-card button[data-action]") : null;
    if (!btn) return;
    const card = btn.closest<HTMLElement>(".shelf-card");
    if (!card) return;
    const { id = "", name = "", description = "" } = card.dataset;
    try {
        switch (btn.dataset["action"]) {
            case "pin":
                await togglePin(btn, id);
                break;
            case "feature":
                await api("PATCH", `/api/shelves/${encodeURIComponent(id)}`, { featured: btn.getAttribute("aria-pressed") !== "true" });
                location.reload(); // the Featured list changes too
                break;
            case "leave":
                if (!confirm(`Stop helping with “${name}”? Its owner can invite you again.`)) return;
                await api("DELETE", `/api/shelves/${encodeURIComponent(id)}/members/${encodeURIComponent(btn.dataset["user"] ?? "")}`);
                card.remove();
                say(`You left “${name}”.`);
                break;
            case "edit": {
                const res = await editShelf({
                    id,
                    name,
                    description,
                    global: card.dataset["global"] === "true",
                    shared: card.dataset["shared"] === "true",
                });
                if (!res) return;
                if (res.deleted) {
                    card.remove();
                    say(`Deleted “${name}”.`);
                } else location.reload();
                break;
            }
        }
    } catch (err) {
        say(errorMessage(err), true);
    }
});

/** Order saving for a list: the whole order, as shown. */
function reorderable(list: HTMLElement, items: string): Reorderable | null {
    const url = list.dataset["order"] ?? "";
    if (!url || list.querySelectorAll(`:scope > ${items}`).length < 2) return null;
    return makeReorderable({
        list,
        items,
        label: (el) => el.dataset["name"] ?? "",
        onChange: (ids) => {
            api("PUT", url, { ids: ids.map(Number) }).then(
                () => say("Order saved."),
                (err: unknown) => say(errorMessage(err), true),
            );
        },
    });
}

// The featured order (admins): always arrangeable, it's a short list.
const featuredList = maybeId("featured-list");
if (featuredList) reorderable(featuredList, ".shelf-row");

// Your shelves: Arrange turns the cards into a list to drag or move.
const arrange = maybeId("arrange-mine", HTMLButtonElement);
const mine = maybeId("mine-list");
if (arrange && mine) {
    let r: Reorderable | null = null;
    arrange.addEventListener("click", () => {
        const on = arrange.getAttribute("aria-pressed") !== "true";
        arrange.setAttribute("aria-pressed", String(on));
        arrange.textContent = on ? "Done" : "Arrange";
        mine.classList.toggle("arranging", on);
        if (on) r = reorderable(mine, ".shelf-card");
        else {
            r?.stop();
            r = null;
        }
    });
}

// Find a shelf: hides the cards whose name and description don't match.
const filter = maybeId("shelves-filter", HTMLInputElement);
if (filter) {
    const none = maybeId("shelves-none");
    filter.addEventListener("input", () => {
        const q = filter.value.trim().toLowerCase();
        let shown = 0;
        for (const card of document.querySelectorAll<HTMLElement>(".shelf-grid .shelf-card")) {
            const hay = `${card.dataset["name"] ?? ""} ${card.dataset["description"] ?? ""}`.toLowerCase();
            const hit = !q || hay.includes(q);
            card.hidden = !hit;
            if (hit) shown++;
        }
        // A section with nothing left showing steps aside.
        for (const sec of document.querySelectorAll<HTMLElement>(".shelves-section")) {
            if (!sec.querySelector(".shelf-grid")) continue;
            sec.classList.toggle("filtered-out", !!q && !sec.querySelector(".shelf-card:not([hidden])"));
        }
        if (none) none.hidden = !q || shown > 0;
    });
}

// Invites: Accept, Decline, Decline and block.
document.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>(".shelf-invite button[data-invite-action]") : null;
    const row = btn?.closest<HTMLElement>(".shelf-invite");
    if (!btn || !row) return;
    const invite = encodeURIComponent(row.dataset["invite"] ?? "");
    const from = row.dataset["from"] ?? "";
    const action = btn.dataset["inviteAction"];
    if (action === "block" && !confirm(`Decline, and take no more invites from ${from}? You can undo this at the bottom of this page.`)) return;
    for (const b of row.querySelectorAll("button")) b.disabled = true;
    try {
        if (action === "accept") {
            const res = await api<{ href: string }>("POST", `/api/me/shelf-invites/${invite}/accept`);
            location.assign(res.href.startsWith("/shelf/") ? res.href : "/shelves");
            return;
        }
        await api("POST", `/api/me/shelf-invites/${invite}/decline`, { block: action === "block" });
        if (action === "block") {
            location.reload(); // their other invites go too, and the blocked list changes
            return;
        }
        row.remove();
        say("Declined.");
    } catch (err) {
        for (const b of row.querySelectorAll("button")) b.disabled = false;
        say(errorMessage(err), true);
    }
});

// Whom the user blocked from inviting them, each to unblock.
const blocks = maybeId("shelf-blocks");
if (blocks) {
    const list = blocks.querySelector<HTMLElement>("ul")!;
    api<{ blocked: { id: number; username: string }[] }>("GET", "/api/me/shelf-invites").then(
        (res) => {
            blocks.hidden = res.blocked.length === 0;
            list.replaceChildren(
                ...res.blocked.map((b) => {
                    const un = el("button", { type: "button", className: "btn-standard" }, "Unblock");
                    un.setAttribute("aria-label", `Unblock ${b.username}`);
                    const li = el("li", {}, el("span", { className: "shelf-people-name" }, b.username), un);
                    un.addEventListener("click", async () => {
                        un.disabled = true;
                        try {
                            await api("DELETE", `/api/me/shelf-blocks/${encodeURIComponent(String(b.id))}`);
                            li.remove();
                            blocks.hidden = list.childElementCount === 0;
                        } catch (err) {
                            un.disabled = false;
                            say(errorMessage(err), true);
                        }
                    });
                    return li;
                }),
            );
        },
        () => {},
    );
}

// A library page: books for offline reading that this device is missing (book-sync.ts).
startBookSync();
