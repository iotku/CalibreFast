/**
 * Group editor for filtered pages (/author, /publisher, /tag, /lang,
 * /series), and their Feature button.
 *
 * Edits are staged locally; nothing is saved until "Save", which writes the
 * grouping and the entry's description and picture together. Removing a
 * value only strikes it through (click again to undo), mirroring the
 * metadata editor. Grouping never touches book metadata.
 */

import { api, byId, errorMessage, maybeId } from "./dom.js";
import type { FeaturedResponse, GroupEdit, GroupSaved, ValueSuggestion } from "./types.js";

interface Member {
    value: string;
    removed: boolean;
    added: boolean; // not saved yet
}

const panel = maybeId("group-panel");
const toggle = maybeId("group-toggle");

if (panel && toggle) init(panel, toggle);

// Feature: show this entry at the top of its aggregate page, for everyone.
const featureBtn = maybeId("feature-toggle", HTMLButtonElement);
featureBtn?.addEventListener("click", async () => {
    const btn = featureBtn;
    const on = btn.getAttribute("aria-pressed") !== "true";
    const url = `/api/aggregates/${encodeURIComponent(btn.dataset["category"] ?? "")}/featured/${encodeURIComponent(btn.dataset["key"] ?? "")}`;
    btn.disabled = true;
    try {
        const res = await api<FeaturedResponse>(on ? "PUT" : "DELETE", url);
        btn.setAttribute("aria-pressed", String(res.featured));
        const label = btn.querySelector(".tb-label");
        if (label) label.textContent = res.featured ? "Featured" : "Feature";
    } catch (err) {
        alert(errorMessage(err));
    } finally {
        btn.disabled = false;
    }
});

function init(panel: HTMLElement, toggle: HTMLElement): void {
    const category = panel.dataset["category"] ?? "";
    const key = panel.dataset["key"] ?? "";
    const initialName = panel.dataset["name"] ?? "";
    const listEl = panel.querySelector<HTMLElement>(".group-members");
    if (!listEl) return;
    const list: HTMLElement = listEl;
    const addInput = byId("group-add", HTMLInputElement);
    const nameInput = byId("group-name", HTMLInputElement);
    const descInput = byId("group-description", HTMLTextAreaElement);
    const status = byId("group-status");
    const saveBtn = byId("group-save", HTMLButtonElement);
    const lower = (s: string): string => s.toLowerCase();

    // The picture: a new file, or the saved one removed.
    const imageFile = byId("group-image-file", HTMLInputElement);
    const imagePreview = byId("group-image-preview", HTMLImageElement);
    const imageCanvas = byId("group-image-canvas", HTMLCanvasElement);
    const imageEmpty = byId("group-image-empty");
    const imageRemove = byId("group-image-remove", HTMLButtonElement);
    const initialImage = panel.dataset["image"] ?? "";
    const initialDesc = descInput.value;
    let newImage: File | null = null;
    let removeImage = false;
    let drawn = 0; // the latest pick's draw; older ones give up

    const initial = [...list.querySelectorAll<HTMLElement>("li[data-member]")].map((li) => li.dataset["member"] ?? "");
    let members: Member[] = initial.map((value) => ({ value, removed: false, added: false }));

    function active(): string[] {
        return members.filter((m) => !m.removed).map((m) => m.value);
    }

    function groupDirty(): boolean {
        const a = active().map(lower).sort().join("\n");
        const b = initial.map(lower).sort().join("\n");
        return a !== b || nameInput.value.trim() !== initialName;
    }

    function descDirty(): boolean {
        return descInput.value.trim() !== initialDesc.trim();
    }

    function dirty(): boolean {
        return groupDirty() || descDirty() || newImage !== null || removeImage;
    }

    // The saved picture stays in the <img> the server rendered; a new pick
    // is drawn on the canvas, so no script ever sets an image URL.
    function showImage(): void {
        const pick = newImage !== null;
        const saved = !pick && !removeImage && initialImage !== "";
        imageCanvas.hidden = !pick;
        imagePreview.hidden = !saved;
        imageEmpty.hidden = pick || saved;
        imageRemove.hidden = !(pick || saved);
        render();
    }

    async function drawPick(file: File): Promise<void> {
        const n = ++drawn;
        let bmp: ImageBitmap;
        try {
            bmp = await createImageBitmap(file);
        } catch {
            if (n !== drawn) return;
            newImage = null;
            setStatus("That file isn't a picture this browser can show.", true);
            showImage();
            return;
        }
        if (n === drawn) {
            const scale = Math.min(1, 264 / Math.max(bmp.width, bmp.height));
            imageCanvas.width = Math.max(1, Math.round(bmp.width * scale));
            imageCanvas.height = Math.max(1, Math.round(bmp.height * scale));
            imageCanvas.getContext("2d")?.drawImage(bmp, 0, 0, imageCanvas.width, imageCanvas.height);
        }
        bmp.close();
    }

    function setStatus(text: string, isError = false): void {
        status.textContent = text;
        status.classList.toggle("error", isError);
    }

    function render(): void {
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
        saveBtn.disabled = !dirty();
        if (!suggestEl.hidden) void refreshSuggestions();
    }

    // Suggestions: only values that exist in the library ({value, label}).
    const suggestEl = byId("group-suggest");
    const MAX_SUGGESTIONS = 50;
    let allValues: ValueSuggestion[] = [];
    let valuesPromise: Promise<ValueSuggestion[]> | null = null;
    let shown: ValueSuggestion[] = []; // values currently listed
    let activeIdx = -1;

    function loadValues(): Promise<ValueSuggestion[]> {
        valuesPromise ??= fetch(`/api/groups/${encodeURIComponent(category)}/values`)
            .then((res): Promise<ValueSuggestion[]> | ValueSuggestion[] => (res.ok ? res.json() : []))
            .then((vals) => (allValues = vals))
            .catch((err: unknown): ValueSuggestion[] => {
                console.error("group values:", err);
                valuesPromise = null; // allow a retry
                return [];
            });
        return valuesPromise;
    }

    function matches(q: string): ValueSuggestion[] {
        const taken = new Set(members.filter((m) => !m.removed).map((m) => lower(m.value)));
        const starts: ValueSuggestion[] = [];
        const contains: ValueSuggestion[] = [];
        for (const v of allValues) {
            if (taken.has(lower(v.value))) continue;
            const hay = [lower(v.value), lower(v.label || "")];
            if (hay.some((h) => h.startsWith(q))) starts.push(v);
            else if (hay.some((h) => h.includes(q))) contains.push(v);
            if (starts.length >= MAX_SUGGESTIONS) break;
        }
        return starts.concat(contains).slice(0, MAX_SUGGESTIONS);
    }

    function closeSuggestions(): void {
        suggestEl.hidden = true;
        addInput.setAttribute("aria-expanded", "false");
        addInput.removeAttribute("aria-activedescendant");
        shown = [];
        activeIdx = -1;
    }

    function setActive(i: number): void {
        activeIdx = i;
        [...suggestEl.children].forEach((li, j) => {
            li.setAttribute("aria-selected", String(j === i));
            if (j === i) {
                li.scrollIntoView({ block: "nearest" });
                addInput.setAttribute("aria-activedescendant", li.id);
            }
        });
    }

    async function refreshSuggestions(): Promise<void> {
        const q = lower(addInput.value.trim());
        if (!q || panel.hidden) return closeSuggestions();
        await loadValues();
        if (lower(addInput.value.trim()) !== q) return; // typed on meanwhile
        shown = matches(q);
        if (shown.length === 0) return closeSuggestions();
        suggestEl.replaceChildren(
            ...shown.map((v, i) => {
                const li = document.createElement("li");
                li.id = `group-suggest-${i}`;
                li.setAttribute("role", "option");
                li.textContent = v.label || v.value;
                if (v.label && v.label !== v.value) {
                    const sub = document.createElement("span");
                    sub.className = "suggest-sub";
                    sub.textContent = v.value;
                    li.append(sub);
                }
                // mousedown so the input doesn't blur (and close the list) first
                li.addEventListener("mousedown", (e) => {
                    e.preventDefault();
                    addValue(v.value);
                });
                return li;
            }),
        );
        suggestEl.hidden = false;
        addInput.setAttribute("aria-expanded", "true");
        setActive(0);
    }

    function addValue(value: string): void {
        const existing = members.find((m) => lower(m.value) === lower(value));
        if (existing) existing.removed = false;
        else members.push({ value, removed: false, added: true });
        addInput.value = "";
        closeSuggestions();
        setStatus("");
        render();
        addInput.focus();
    }

    /** Add the highlighted suggestion, or an exact (case-insensitive) match. */
    async function add(): Promise<void> {
        const typed = addInput.value.trim();
        const highlighted = activeIdx >= 0 ? shown[activeIdx] : undefined;
        if (highlighted) return addValue(highlighted.value);
        if (!typed) return;
        await loadValues();
        const t = lower(typed);
        const hit = allValues.find((v) => lower(v.value) === t || lower(v.label || "") === t);
        if (hit) return addValue(hit.value);
        setStatus(`No ${panel.dataset["label"]} “${typed}” in the library.`, true);
    }

    function open(show: boolean): void {
        panel.hidden = !show;
        toggle.setAttribute("aria-expanded", String(show));
        if (show) {
            void loadValues();
            addInput.focus();
        }
    }

    /** Saves the grouping, description and picture in one request. */
    async function save(): Promise<void> {
        const edit: GroupEdit = {};
        if (groupDirty()) {
            edit.name = nameInput.value.trim();
            edit.members = active();
        }
        if (descDirty()) edit.description = descInput.value.trim();
        if (removeImage && !newImage) edit.removeImage = true;
        let init: RequestInit;
        if (newImage) {
            const form = new FormData();
            form.append("data", JSON.stringify(edit));
            form.append("image", newImage);
            init = { method: "PUT", body: form };
        } else {
            init = { method: "PUT", headers: { "content-type": "application/json" }, body: JSON.stringify(edit) };
        }
        saveBtn.disabled = true;
        setStatus("Saving…");
        try {
            const res = await fetch(`/api/groups/${encodeURIComponent(category)}/${encodeURIComponent(key)}`, init);
            if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
            const data = (await res.json()) as GroupSaved;
            location.href = data.url;
        } catch (err) {
            setStatus(errorMessage(err), true);
            saveBtn.disabled = false;
        }
    }

    toggle.addEventListener("click", () => open(panel.hidden !== false)); // hidden → show
    byId("group-add-btn").addEventListener("click", () => void add());
    addInput.addEventListener("input", () => {
        setStatus("");
        void refreshSuggestions();
    });
    addInput.addEventListener("keydown", (e) => {
        const open = !suggestEl.hidden && shown.length > 0;
        if (e.key === "ArrowDown" && open) {
            e.preventDefault();
            setActive((activeIdx + 1) % shown.length);
        } else if (e.key === "ArrowUp" && open) {
            e.preventDefault();
            setActive((activeIdx - 1 + shown.length) % shown.length);
        } else if (e.key === "Enter") {
            e.preventDefault();
            void add();
        } else if (e.key === "Escape" && open) {
            e.stopPropagation(); // close the list, not the whole panel
            closeSuggestions();
        }
    });
    addInput.addEventListener("blur", closeSuggestions);
    nameInput.addEventListener("input", render);
    descInput.addEventListener("input", render);
    byId("group-image-pick").addEventListener("click", () => imageFile.click());
    imageFile.addEventListener("change", () => {
        const file = imageFile.files?.[0];
        if (!file) return;
        newImage = file;
        removeImage = false;
        setStatus("");
        showImage();
        void drawPick(file);
    });
    imageRemove.addEventListener("click", () => {
        newImage = null;
        removeImage = initialImage !== "";
        imageFile.value = "";
        showImage();
    });
    byId("group-cancel").addEventListener("click", () => {
        members = initial.map((value) => ({ value, removed: false, added: false }));
        nameInput.value = initialName;
        descInput.value = initialDesc;
        newImage = null;
        removeImage = false;
        imageFile.value = "";
        setStatus("");
        showImage();
        open(false);
    });
    saveBtn.addEventListener("click", () => void save());
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && !panel.hidden) open(false);
    });

    render();
}
