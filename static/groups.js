/**
 * Group editor for filtered pages (/author, /publisher, /tag, /lang).
 *
 * Edits are staged locally; nothing is saved until "Save grouping". Removing a
 * value only strikes it through (click again to undo), mirroring the metadata
 * editor. Grouping never touches book metadata.
 */

const $ = (id) => document.getElementById(id);
const panel = $("group-panel");
const toggle = $("group-toggle");

if (panel && toggle) init();

function init() {
    const { category, key, name: initialName } = panel.dataset;
    const list = panel.querySelector(".group-members");
    const addInput = $("group-add");
    const nameInput = $("group-name");
    const status = $("group-status");
    const saveBtn = $("group-save");
    const lower = (s) => s.toLowerCase();

    const initial = [...list.querySelectorAll("li[data-member]")].map(
        (li) => li.dataset.member,
    );
    // { value, removed, added }
    let members = initial.map((value) => ({ value, removed: false, added: false }));
    let valuesLoaded = false;

    function active() {
        return members.filter((m) => !m.removed).map((m) => m.value);
    }

    function dirty() {
        const a = active().map(lower).sort().join("\n");
        const b = initial.map(lower).sort().join("\n");
        return a !== b || nameInput.value.trim() !== initialName;
    }

    function setStatus(text, isError = false) {
        status.textContent = text;
        status.classList.toggle("error", isError);
    }

    function render() {
        list.replaceChildren(
            ...members.map((m) => {
                const li = document.createElement("li");
                li.className = "group-chip";
                li.classList.toggle("removed", m.removed);
                li.classList.toggle("added", m.added);
                const label = document.createElement("span");
                label.textContent = m.value;
                const btn = document.createElement("button");
                btn.type = "button";
                btn.textContent = m.removed ? "Undo" : "×";
                btn.title = m.removed ? `Keep ${m.value}` : `Remove ${m.value} from this group`;
                btn.addEventListener("click", () => {
                    if (m.added) members = members.filter((x) => x !== m); // never saved: just drop it
                    else m.removed = !m.removed;
                    render();
                });
                li.append(label, btn);
                return li;
            }),
        );
        if (!saveBtn.hasAttribute("data-locked")) {
            saveBtn.disabled = !dirty();
        }
        refreshSuggestions();
    }

    let allValues = [];
    function refreshSuggestions() {
        const taken = new Set(members.map((m) => lower(m.value)));
        $("group-values").replaceChildren(
            ...allValues
                .filter((v) => !taken.has(lower(v)))
                .map((v) => Object.assign(document.createElement("option"), { value: v })),
        );
    }

    async function loadValues() {
        if (valuesLoaded) return;
        valuesLoaded = true;
        try {
            const res = await fetch(`/api/groups/${encodeURIComponent(category)}/values`);
            if (res.ok) allValues = await res.json();
            refreshSuggestions();
        } catch (err) {
            console.error("group values:", err);
        }
    }

    function add() {
        const v = addInput.value.trim();
        if (!v) return;
        const existing = members.find((m) => lower(m.value) === lower(v));
        if (existing) existing.removed = false;
        else members.push({ value: v, removed: false, added: true });
        addInput.value = "";
        setStatus("");
        render();
        addInput.focus();
    }

    function open(show) {
        panel.hidden = !show;
        toggle.setAttribute("aria-expanded", String(show));
        if (show) {
            loadValues();
            addInput.focus();
        }
    }

    async function save() {
        const body = { name: nameInput.value.trim(), members: active() };
        saveBtn.disabled = true;
        setStatus("Saving…");
        try {
            const res = await fetch(
                `/api/groups/${encodeURIComponent(category)}/${encodeURIComponent(key)}`,
                {
                    method: "PUT",
                    headers: { "content-type": "application/json" },
                    body: JSON.stringify(body),
                },
            );
            if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
            const data = await res.json();
            location.href = data.url;
        } catch (err) {
            setStatus(err.message, true);
            saveBtn.disabled = false;
        }
    }

    if (saveBtn.disabled) saveBtn.setAttribute("data-locked", ""); // read-only store

    toggle.addEventListener("click", () => open(panel.hidden));
    $("group-add-btn").addEventListener("click", add);
    addInput.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
            e.preventDefault();
            add();
        }
    });
    nameInput.addEventListener("input", render);
    $("group-cancel").addEventListener("click", () => {
        members = initial.map((value) => ({ value, removed: false, added: false }));
        nameInput.value = initialName;
        setStatus("");
        render();
        open(false);
    });
    saveBtn.addEventListener("click", save);
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && !panel.hidden) open(false);
    });

    render();
}
