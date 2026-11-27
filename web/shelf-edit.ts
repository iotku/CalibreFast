// A shelf's Edit dialog, on /shelves and the shelf's own page: its name,
// description, whether it's shared with everyone (a personal shelf), the
// people who help with it (invite, withdraw, remove: shelf_members.go), and
// Delete. Saves with PATCH / DELETE /api/shelves/{id} (shelves.go); the
// People part acts at once.
//
// Built with the DOM (el, textContent): a shelf's name and description are
// whatever its owner typed, and are never parsed as HTML.

import { api, closeOnBackdropClick, el, errorMessage } from "./dom.js";

export interface ShelfInfo {
    id: string;
    name: string;
    description: string;
    /** A global shelf (admins'): it can't be shared, it's everyone's. */
    global: boolean;
    shared: boolean;
}

/** What the dialog did: the shelf as saved, or deleted. Null: cancelled. */
export type ShelfEditResult = { deleted: true } | { deleted: false; shelf: ShelfInfo } | null;

let open = false;

/** Opens the dialog; resolves once it's closed. */
export function editShelf(shelf: ShelfInfo): Promise<ShelfEditResult> {
    if (open) return Promise.resolve(null);
    open = true;
    return new Promise((resolve) => {
        let result: ShelfEditResult = null;

        const title = el("h2", { id: "shelf-edit-title" }, "Edit shelf");
        const name = el("input", { type: "text", name: "name", maxLength: 100, required: true, autocomplete: "off", value: shelf.name });
        const desc = el("textarea", { name: "description", maxLength: 1000, rows: 4, value: shelf.description });
        const fields: Node[] = [
            el("label", { className: "shelf-edit-field" }, el("span", {}, "Name"), name),
            el("label", { className: "shelf-edit-field" }, el("span", {}, "Description (optional)"), desc),
        ];
        const shared = el("input", { type: "checkbox", name: "shared", checked: shelf.shared });
        if (!shelf.global) {
            fields.push(
                el(
                    "label",
                    { className: "shelf-edit-check" },
                    shared,
                    el("span", {}, "Share with everyone", el("small", { className: "auth-muted" }, "Anyone who can browse this library can see it, with your username. Only you can change it.")),
                ),
            );
        }
        if (!shelf.global) fields.push(peopleSection(shelf));
        const status = el("p", { className: "auth-error", role: "alert", hidden: true });
        const del = el("button", { type: "button", className: "btn-action btn-danger shelf-edit-delete" }, "Delete shelf…");
        const cancel = el("button", { type: "button", className: "btn-action" }, "Cancel");
        const save = el("button", { type: "submit", className: "btn-action btn-action-read" }, "Save");
        const actions = el("div", { className: "dialog-actions shelf-edit-actions" }, del, el("span", { className: "shelf-edit-spacer" }), cancel, save);
        const form = el("form", { method: "dialog" }, title, ...fields, status, actions);
        const dialog = el("dialog", { className: "dialog" }, form);
        dialog.setAttribute("aria-labelledby", "shelf-edit-title");
        closeOnBackdropClick(dialog);

        const fail = (err: unknown): void => {
            status.textContent = errorMessage(err);
            status.hidden = false;
        };
        const busy = (on: boolean): void => {
            for (const b of [del, cancel, save]) b.disabled = on;
        };

        cancel.addEventListener("click", () => dialog.close());
        form.addEventListener("submit", async (e) => {
            e.preventDefault();
            const next = { name: name.value.trim(), description: desc.value.trim(), shared: !shelf.global && shared.checked };
            if (!next.name) {
                fail("A shelf needs a name.");
                return;
            }
            busy(true);
            try {
                await api("PATCH", `/api/shelves/${encodeURIComponent(shelf.id)}`, shelf.global ? { name: next.name, description: next.description } : next);
                result = { deleted: false, shelf: { ...shelf, ...next } };
                dialog.close();
            } catch (err) {
                fail(err);
            } finally {
                busy(false);
            }
        });

        // Delete asks again in the same box: the books themselves stay.
        let confirming = false;
        del.addEventListener("click", async () => {
            if (!confirming) {
                confirming = true;
                del.textContent = "Delete for good";
                status.textContent = `Delete “${shelf.name}”? The books themselves aren't affected.`;
                status.hidden = false;
                return;
            }
            busy(true);
            try {
                await api("DELETE", `/api/shelves/${encodeURIComponent(shelf.id)}`);
                result = { deleted: true };
                dialog.close();
            } catch (err) {
                fail(err);
            } finally {
                busy(false);
            }
        });

        dialog.addEventListener("close", () => {
            dialog.remove();
            open = false;
            resolve(result);
        });
        document.body.append(dialog);
        dialog.showModal();
        name.focus();
    });
}

interface Member {
    id: number;
    username: string;
}
interface Invite {
    id: number;
    to: string;
}
interface MembersResponse {
    members: Member[];
    invites?: Invite[];
    can_invite?: boolean;
}

/**
 * People: who helps with the shelf (they add, remove and arrange its books),
 * and the invites not yet answered. Invites are by username, to a shared
 * shelf only. Each change is made at once, apart from Save.
 */
function peopleSection(shelf: ShelfInfo): HTMLElement {
    const id = encodeURIComponent(shelf.id);
    const list = el("ul", { className: "shelf-people" });
    const note = el("p", { className: "auth-muted shelf-people-note" });
    const status = el("p", { className: "shelf-people-status", role: "status" });
    const name = el("input", { type: "text", maxLength: 100, placeholder: "Username", autocomplete: "off" });
    name.setAttribute("aria-label", "Username to invite");
    const invite = el("button", { type: "button", className: "btn-action" }, "Invite");
    const row = el("div", { className: "shelf-people-invite" }, name, invite);
    const box = el(
        "fieldset",
        { className: "shelf-people-box" },
        el("legend", {}, "People who help"),
        el("p", { className: "auth-muted" }, "They can add, remove and arrange this shelf's books. Only you can rename, share or delete it."),
        list,
        row,
        note,
        status,
    );
    const say = (text: string, isError = false): void => {
        status.textContent = text;
        status.classList.toggle("auth-error", isError);
    };

    function person(label: string, sub: string, action: string, act: () => Promise<void>): HTMLLIElement {
        const b = el("button", { type: "button", className: "btn-action" }, action);
        b.setAttribute("aria-label", `${action} ${label}`);
        b.addEventListener("click", async () => {
            b.disabled = true;
            try {
                await act();
                await load();
            } catch (err) {
                b.disabled = false;
                say(errorMessage(err), true);
            }
        });
        return el("li", {}, el("span", { className: "shelf-people-name" }, label), el("span", { className: "auth-muted" }, sub), b);
    }

    async function load(): Promise<void> {
        const res = await api<MembersResponse>("GET", `/api/shelves/${id}/members`);
        const items = [
            ...res.members.map((m) =>
                person(m.username, "helps", "Remove", () => api("DELETE", `/api/shelves/${id}/members/${encodeURIComponent(String(m.id))}`).then(() => say(`Removed ${m.username}.`))),
            ),
            ...(res.invites ?? []).map((v) =>
                person(v.to, "invited", "Withdraw", () => api("DELETE", `/api/shelves/${id}/invites/${encodeURIComponent(String(v.id))}`).then(() => say(`Withdrew the invite to ${v.to}.`))),
            ),
        ];
        list.replaceChildren(...(items.length ? items : [el("li", { className: "auth-muted" }, "Nobody yet.")]));
        const can = !!res.can_invite;
        name.disabled = invite.disabled = !can;
        note.textContent = can ? "" : "Share the shelf with everyone (above, then Save) to invite people.";
        note.hidden = can;
    }

    const send = async (): Promise<void> => {
        const who = name.value.trim();
        if (!who) return;
        invite.disabled = true;
        try {
            await api("POST", `/api/shelves/${id}/invites`, { username: who });
            name.value = "";
            say(`Invited ${who}. They'll be asked to accept.`);
            await load();
        } catch (err) {
            say(errorMessage(err), true);
        } finally {
            invite.disabled = name.disabled;
        }
    };
    invite.addEventListener("click", () => void send());
    name.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
            e.preventDefault(); // not the dialog's Save
            void send();
        }
    });
    load().catch((err: unknown) => say(errorMessage(err), true));
    return box;
}
