// Admin user management (calls /api/admin/users).
const status = document.getElementById("admin-status");
const say = (text, error = false) => {
    status.hidden = false;
    status.textContent = text;
    status.className = error ? "auth-error" : "auth-notice";
};

async function api(method, url, body) {
    const res = await fetch(url, {
        method,
        headers: { "content-type": "application/json" },
        body: body ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    return res.status === 204 ? null : res.json();
}

document.querySelector(".admin-users").addEventListener("change", async (e) => {
    const box = e.target.closest("input[data-field]");
    if (!box) return;
    const row = box.closest("tr");
    try {
        await api("PATCH", `/api/admin/users/${row.dataset.id}`, { [box.dataset.field]: box.checked });
        say(`Updated ${row.dataset.username}.`);
    } catch (err) {
        box.checked = !box.checked;
        say(err.message, true);
    }
});

document.querySelector(".admin-users").addEventListener("click", async (e) => {
    const btn = e.target.closest("button[data-action]");
    if (!btn) return;
    const row = btn.closest("tr");
    const name = row.dataset.username;
    try {
        if (btn.dataset.action === "signin-link") {
            const res = await api("POST", "/api/admin/links", { kind: "login", user_id: Number(row.dataset.id), expires: expiry() });
            showLink(`Sign-in link for ${name} — works once, expires ${when(res.expires_at)}:`, res.url);
            loadLinks();
        } else if (btn.dataset.action === "password") {
            const pw = prompt(`New password for ${name} (min 10 characters). Their sessions will be signed out.`);
            if (!pw) return;
            await api("PATCH", `/api/admin/users/${row.dataset.id}`, { password: pw });
            say(`Password reset for ${name}.`);
        } else if (btn.dataset.action === "delete") {
            if (!confirm(`Delete ${name}? Their favorites, progress, bookmarks and shelves are deleted too. This can't be undone.`)) return;
            await api("DELETE", `/api/admin/users/${row.dataset.id}`);
            row.remove();
            say(`Deleted ${name}.`);
        }
    } catch (err) {
        say(err.message, true);
    }
});

document.getElementById("add-user").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
        await api("POST", "/api/admin/users", {
            username: f.username.value,
            password: f.password.value,
            is_admin: f.is_admin.checked,
        });
        location.reload();
    } catch (err) {
        say(err.message, true);
    }
});

// ---- one-time links --------------------------------------------------------

const expiry = () => document.getElementById("link-expiry").value;
const when = (unix) => new Date(unix * 1000).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });

function showLink(label, url) {
    document.getElementById("link-output").hidden = false;
    document.getElementById("link-output-label").textContent = label;
    const input = document.getElementById("link-url");
    input.value = url;
    input.focus();
    input.select();
    document.getElementById("links").scrollIntoView({ behavior: "smooth", block: "start" });
}

document.getElementById("link-copy").addEventListener("click", async () => {
    const input = document.getElementById("link-url");
    try {
        await navigator.clipboard.writeText(input.value);
    } catch {
        input.select();
        document.execCommand("copy");
    }
    say("Link copied.");
});

document.getElementById("invite-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
        const res = await api("POST", "/api/admin/links", {
            kind: "invite",
            is_admin: f.is_admin.checked,
            note: f.note.value,
            expires: expiry(),
        });
        showLink(`Invite${f.is_admin.checked ? " (admin)" : ""} — works once, expires ${when(res.expires_at)}:`, res.url);
        f.reset();
        loadLinks();
    } catch (err) {
        say(err.message, true);
    }
});

async function loadLinks() {
    const list = document.getElementById("link-list");
    try {
        const { links } = await api("GET", "/api/admin/links");
        list.replaceChildren(
            ...(links.length
                ? links.map((l) => {
                      const li = document.createElement("li");
                      const text = document.createElement("span");
                      text.textContent =
                          (l.kind === "login" ? `Sign-in as ${l.username}` : `Invite${l.invite_admin ? " (admin)" : ""}`) +
                          (l.note ? ` — “${l.note}”` : "") +
                          ` · by ${l.created_by || "?"} · expires ${when(l.expires_at)}`;
                      const revoke = document.createElement("button");
                      revoke.type = "button";
                      revoke.className = "btn-standard";
                      revoke.textContent = "Revoke";
                      revoke.addEventListener("click", async () => {
                          try {
                              await api("DELETE", `/api/admin/links/${l.id}`);
                              li.remove();
                              say("Link revoked.");
                          } catch (err) {
                              say(err.message, true);
                          }
                      });
                      li.append(text, revoke);
                      return li;
                  })
                : [Object.assign(document.createElement("li"), { className: "auth-muted", textContent: "No active links." })]),
        );
    } catch (err) {
        list.replaceChildren(Object.assign(document.createElement("li"), { className: "auth-error", textContent: err.message }));
    }
}
loadLinks();
