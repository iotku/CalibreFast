// Admin user management (calls /api/admin/users) and one-time links
// (/api/admin/links).
import { api, byId, el, errorMessage, statusLine, withPageScheme } from "./dom.js";

const say = statusLine("admin-status");
const table = byId("users-table", HTMLTableElement);

/** A form field by name, checked to exist (the template defines them). */
function field(form: HTMLFormElement, name: string): HTMLInputElement {
    const input = form.elements.namedItem(name);
    if (!(input instanceof HTMLInputElement)) throw new Error(`form field ${name} missing`);
    return input;
}

table.addEventListener("change", async (e) => {
    const box = e.target instanceof Element ? e.target.closest<HTMLInputElement>("input[data-field]") : null;
    const row = box?.closest<HTMLTableRowElement>("tr");
    if (!box || !row) return;
    try {
        await api("PATCH", `/api/admin/users/${row.dataset["id"]}`, { [box.dataset["field"] ?? ""]: box.checked });
        say(`Updated ${row.dataset["username"]}.`);
    } catch (err) {
        box.checked = !box.checked;
        say(errorMessage(err), true);
    }
});

table.addEventListener("click", async (e) => {
    const btn = e.target instanceof Element ? e.target.closest<HTMLButtonElement>("button[data-action]") : null;
    const row = btn?.closest<HTMLTableRowElement>("tr");
    if (!btn || !row) return;
    const name = row.dataset["username"] ?? "";
    const id = row.dataset["id"] ?? "";
    try {
        switch (btn.dataset["action"]) {
            case "signin-link": {
                const res = await api<LinkCreated>("POST", "/api/admin/links", { kind: "login", user_id: Number(id), expires: expiry() });
                showLink(`Sign-in link for ${name} — works once, expires ${when(res.expires_at)}:`, res.url);
                void loadLinks();
                break;
            }
            case "password": {
                const pw = prompt(`New password for ${name} (min 10 characters). Their sessions will be signed out.`);
                if (!pw) return;
                await api("PATCH", `/api/admin/users/${id}`, { password: pw });
                say(`Password reset for ${name}.`);
                break;
            }
            case "delete":
                if (!confirm(`Delete ${name}? Their favorites, progress, bookmarks and shelves are deleted too. This can't be undone.`)) return;
                await api("DELETE", `/api/admin/users/${id}`);
                row.remove();
                say(`Deleted ${name}.`);
                break;
        }
    } catch (err) {
        say(errorMessage(err), true);
    }
});

byId("add-user", HTMLFormElement).addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.currentTarget as HTMLFormElement;
    try {
        await api("POST", "/api/admin/users", {
            username: field(f, "username").value,
            password: field(f, "password").value,
            is_admin: field(f, "is_admin").checked,
        });
        location.reload();
    } catch (err) {
        say(errorMessage(err), true);
    }
});

// ---- one-time links --------------------------------------------------------

/** POST /api/admin/links response (Go: adminCreateLink). */
interface LinkCreated {
    url: string;
    expires_at: number;
}

/** An active link (Go: loginLink). */
interface ActiveLink {
    id: string;
    kind: "login" | "invite";
    username?: string;
    invite_admin: boolean;
    note: string;
    created_by: string;
    expires_at: number;
}

const expiry = (): string => byId("link-expiry", HTMLSelectElement).value;
const when = (unix: number): string =>
    new Date(unix * 1000).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });

function showLink(label: string, url: string): void {
    byId("link-output").hidden = false;
    byId("link-output-label").textContent = label;
    const input = byId("link-url", HTMLInputElement);
    input.value = withPageScheme(url);
    input.focus();
    input.select();
    byId("links").scrollIntoView({ behavior: "smooth", block: "start" });
}

byId("link-copy").addEventListener("click", async () => {
    const input = byId("link-url", HTMLInputElement);
    try {
        await navigator.clipboard.writeText(input.value);
    } catch {
        input.select();
        document.execCommand("copy");
    }
    say("Link copied.");
});

byId("invite-form", HTMLFormElement).addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.currentTarget as HTMLFormElement;
    const admin = field(f, "is_admin").checked;
    try {
        const res = await api<LinkCreated>("POST", "/api/admin/links", {
            kind: "invite",
            is_admin: admin,
            note: field(f, "note").value,
            expires: expiry(),
        });
        showLink(`Invite${admin ? " (admin)" : ""} — works once, expires ${when(res.expires_at)}:`, res.url);
        f.reset();
        void loadLinks();
    } catch (err) {
        say(errorMessage(err), true);
    }
});

function linkRow(l: ActiveLink): HTMLLIElement {
    const text =
        (l.kind === "login" ? `Sign-in as ${l.username}` : `Invite${l.invite_admin ? " (admin)" : ""}`) +
        (l.note ? ` — “${l.note}”` : "") +
        ` · by ${l.created_by || "?"} · expires ${when(l.expires_at)}`;
    const revoke = el("button", { type: "button", className: "btn-standard", textContent: "Revoke" });
    const li = el("li", {}, el("span", { textContent: text }), revoke);
    revoke.addEventListener("click", async () => {
        try {
            await api("DELETE", `/api/admin/links/${l.id}`);
            li.remove();
            say("Link revoked.");
        } catch (err) {
            say(errorMessage(err), true);
        }
    });
    return li;
}

async function loadLinks(): Promise<void> {
    const list = byId("link-list");
    try {
        const { links } = await api<{ links: ActiveLink[] }>("GET", "/api/admin/links");
        list.replaceChildren(
            ...(links.length ? links.map(linkRow) : [el("li", { className: "auth-muted", textContent: "No active links." })]),
        );
    } catch (err) {
        list.replaceChildren(el("li", { className: "auth-error", textContent: errorMessage(err) }));
    }
}
void loadLinks();
