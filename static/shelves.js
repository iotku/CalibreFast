// Bookshelves page: create, rename, describe and delete shelves.
const status = document.getElementById("shelf-status");
const say = (text, error = false) => {
    status.hidden = false;
    status.textContent = text;
    status.className = error ? "auth-error" : "auth-notice";
};

async function api(method, url, body) {
    const res = await fetch(url, {
        method,
        credentials: "same-origin",
        headers: body ? { "content-type": "application/json" } : {},
        body: body ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    return res.status === 204 ? null : res.json();
}

for (const form of document.querySelectorAll(".shelf-create")) {
    form.addEventListener("submit", async (e) => {
        e.preventDefault();
        try {
            await api("POST", "/api/shelves", { name: form.name.value, global: form.dataset.global === "true" });
            location.reload();
        } catch (err) {
            say(err.message, true);
        }
    });
}

document.addEventListener("click", async (e) => {
    const btn = e.target.closest(".shelf-row button[data-action]");
    if (!btn) return;
    const row = btn.closest(".shelf-row");
    const { id, name, description } = row.dataset;
    try {
        if (btn.dataset.action === "rename") {
            const next = prompt("Shelf name", name);
            if (!next || next === name) return;
            await api("PATCH", `/api/shelves/${id}`, { name: next });
            location.reload();
        } else if (btn.dataset.action === "describe") {
            const next = prompt("Description (optional)", description);
            if (next === null) return;
            await api("PATCH", `/api/shelves/${id}`, { description: next });
            location.reload();
        } else if (btn.dataset.action === "delete") {
            if (!confirm(`Delete the shelf “${name}”? The books themselves aren't affected.`)) return;
            await api("DELETE", `/api/shelves/${id}`);
            row.remove();
            say(`Deleted “${name}”.`);
        }
    } catch (err) {
        say(err.message, true);
    }
});
