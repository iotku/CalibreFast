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
        if (btn.dataset.action === "password") {
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
