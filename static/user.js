/**
 * Per-user controls in the book popup / page: favorite star, reading status
 * and shelves. Loaded from /api/me/books/<uuid> so the book page itself stays
 * the same for everyone (and cacheable). Signed-out visitors get a sign-in
 * link instead.
 */

const SELECTOR = ".book-user-actions[data-uuid]";

async function api(method, url, body) {
    const res = await fetch(url, {
        method,
        credentials: "same-origin",
        headers: body ? { "content-type": "application/json" } : {},
        body: body ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) {
        const err = new Error((await res.text()).trim() || `HTTP ${res.status}`);
        err.status = res.status;
        throw err;
    }
    return res.status === 204 ? null : res.json();
}

const el = (tag, props = {}, ...children) => {
    const e = Object.assign(document.createElement(tag), props);
    e.append(...children);
    return e;
};

function renderSignedOut(box) {
    const next = encodeURIComponent(location.pathname + location.search + location.hash);
    box.replaceChildren(
        el("a", { href: `/login?next=${next}`, className: "meta-link user-signin-hint", textContent: "Sign in to favorite, track and shelve this book" }),
    );
}

function render(box, uuid, state) {
    const star = el("button", {
        type: "button",
        className: "btn-action btn-favorite",
        title: state.favorite ? "Remove from favorites" : "Add to favorites",
    });
    const paintStar = () => {
        star.textContent = state.favorite ? "★ Favorite" : "☆ Favorite";
        star.setAttribute("aria-pressed", String(state.favorite));
        star.classList.toggle("is-favorite", state.favorite);
    };
    paintStar();
    star.addEventListener("click", async () => {
        const want = !state.favorite;
        state.favorite = want; // optimistic
        paintStar();
        try {
            await api("PUT", `/api/me/books/${uuid}/favorite`, { favorite: want });
        } catch {
            state.favorite = !want;
            paintStar();
        }
    });

    const status = el("select", { className: "book-status", title: "Reading status" },
        el("option", { value: "", textContent: "Not started" }),
        el("option", { value: "reading", textContent: "Reading" }),
        el("option", { value: "finished", textContent: "Finished" }),
    );
    status.value = state.status || "";
    const autoNote = el("span", { className: "book-status-note" });
    const paintNote = () => {
        autoNote.textContent = state.status && state.status_source === "auto" ? "set by the reader" : "";
    };
    paintNote();
    status.addEventListener("change", async () => {
        const prev = state.status;
        try {
            const res = await api("PUT", `/api/me/books/${uuid}/status`, { status: status.value });
            Object.assign(state, res);
        } catch {
            status.value = prev || "";
        }
        paintNote();
    });

    const row = el("div", { className: "book-user-row" }, star, el("label", { className: "book-status-label" }, status), autoNote);
    box.replaceChildren(row);
    renderShelves(box, uuid, state);
}

function renderShelves(box, uuid, state) {
    const shelves = state.shelves || [];
    const details = el("details", { className: "book-shelves" });
    const summary = el("summary", { textContent: "Shelves" });
    const count = el("span", { className: "book-shelves-count" });
    summary.append(count);
    const list = el("div", { className: "book-shelves-list" });
    const paintCount = () => {
        const n = shelves.filter((s) => s.has).length;
        count.textContent = n ? ` (${n})` : "";
    };

    for (const s of shelves) {
        const box2 = el("input", { type: "checkbox", checked: s.has, disabled: !s.can_edit });
        box2.addEventListener("change", async () => {
            try {
                await api(box2.checked ? "PUT" : "DELETE", `/api/shelves/${s.id}/books/${uuid}`);
                s.has = box2.checked;
            } catch {
                box2.checked = !box2.checked;
            }
            paintCount();
        });
        list.append(el("label", { className: "book-shelf" }, box2, ` ${s.name}`, s.global ? el("span", { className: "book-shelf-global", textContent: "global" }) : ""));
    }

    // Quick-create a personal shelf with this book on it.
    const input = el("input", { type: "text", placeholder: "New shelf…", className: "book-shelf-new", autocomplete: "off" });
    const add = el("button", { type: "button", className: "btn-action", textContent: "Add" });
    const create = async () => {
        const name = input.value.trim();
        if (!name) return;
        try {
            const shelf = await api("POST", "/api/shelves", { name });
            await api("PUT", `/api/shelves/${shelf.id}/books/${uuid}`);
            shelves.push({ id: shelf.id, name: shelf.name, global: false, has: true, can_edit: true });
            renderShelves(box, uuid, state);
            box.querySelector(".book-shelves").open = true;
        } catch (err) {
            input.setCustomValidity(err.message);
            input.reportValidity();
        }
    };
    add.addEventListener("click", create);
    input.addEventListener("input", () => input.setCustomValidity(""));
    input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
            e.preventDefault();
            create();
        }
    });
    list.append(el("div", { className: "book-shelf-create" }, input, add));

    details.append(summary, list);
    paintCount();
    box.querySelector(".book-shelves")?.remove();
    box.append(details);
}

async function mount(box) {
    if (box.dataset.mounted) return;
    box.dataset.mounted = "1";
    const uuid = box.dataset.uuid;
    try {
        const state = await api("GET", `/api/me/books/${encodeURIComponent(uuid)}`);
        render(box, uuid, state);
    } catch (err) {
        if (err.status === 401) renderSignedOut(box);
    }
}

export function mountAll(root = document) {
    if (root.matches?.(SELECTOR)) mount(root);
    root.querySelectorAll?.(SELECTOR).forEach(mount);
}

// Book popups are loaded by htmx; mount whatever appears.
new MutationObserver((records) => {
    for (const r of records) for (const n of r.addedNodes) if (n.nodeType === 1) mountAll(n);
}).observe(document.documentElement, { childList: true, subtree: true });

mountAll();
