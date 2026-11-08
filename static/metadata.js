import { createChipInput, parseList } from "./chips.js";

const FIELDS = [
  { key: "title", kind: "string" },
  { key: "creators", kind: "list" },
  { key: "description", kind: "string" },
  { key: "publisher", kind: "string" },
  { key: "date", kind: "date" },
  { key: "languages", kind: "list" }, // several per book; order kept
  { key: "subjects", kind: "list" },
  { key: "identifiers", kind: "identifiers" },
];

function capitalize(s) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// UploadedBookMeta embeds OPFMetadata anonymously in Go, so the JSON is
// flattened — Title/Creators/etc. live directly on the meta object
// alongside Format/FileName.
function fieldValue(meta, key, kind) {
  switch (kind) {
    case "string":
      return meta[capitalize(key)] || "";
    case "date": {
      return meta[capitalize(key)] || "";
    }
    case "list":
      return (meta[capitalize(key)] || []).join(", ");
    case "identifiers":
      return (meta.Identifiers || [])
        .map((id) => `${id.Scheme}: ${id.Value}`)
        .join(", ");
  }
}

function sourceLabel(meta) {
  return meta.FileName ? `${meta.Format} (${meta.FileName})` : meta.Format;
}

export async function fetchMetas(uuid) {
  const res = await fetch(`/metadata/${uuid}`);
  if (!res.ok) throw new Error(`Failed to load metadata: HTTP ${res.status}`);
  return res.json();
}

/**
 * "edited" source button: once the user changes a field, their latest edit is
 * kept as its own source so they can flip to "current" (or a format) to look
 * and come back without retyping.
 */
function makeEditedButton(onClick) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "meta-source-btn meta-source-edited";
  btn.textContent = "edited";
  btn.title = "Your changes to this field (click to restore them)";
  btn.addEventListener("click", onClick);
  return btn;
}

/**
 * Per-field diff shown under a field when its "edited" button is clicked:
 * this field's changes vs the pre-existing ("current") metadata. Stays live
 * while open; clicking "edited" again (while edits are showing) hides it.
 */
const openDiffs = new Map(); // key -> { el, render }

function fieldDiffToggle(form, current, key, buttonRow, { alreadyShowingEdits }) {
  const open = openDiffs.get(key);
  if (open && alreadyShowingEdits) {
    open.el.remove();
    openDiffs.delete(key);
    return;
  }
  if (open) return open.render();

  const el = document.createElement("div");
  el.className = "field-diff";
  const render = () => {
    const changes = computeChanges(current, collectValues(form)).filter((c) => c.key === key);
    el.replaceChildren(...renderChanges(changes, { compact: true }));
  };
  buttonRow.closest("fieldset").append(el);
  openDiffs.set(key, { el, render });
  render();
}

function refreshOpenDiffs() {
  for (const { render } of openDiffs.values()) render();
}

function insertEditedButton(buttonRow, btn) {
  // right after "current" (the pre-existing metadata) when present
  const first = buttonRow.firstElementChild;
  if (first && first.dataset.format === "current") first.after(btn);
  else buttonRow.prepend(btn);
}

function populateField(form, current, metas, key, kind) {
  const input = form.querySelector(`[name="${key}"]`);
  const buttonRow = form.querySelector(
    `.meta-source-buttons[data-field="${key}"]`,
  );
  if (!input || !buttonRow) return;

  let applying = false; // true while we set the value programmatically
  const setValue = (v) => {
    applying = true;
    input.value = v;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    applying = false;
  };

  buttonRow.replaceChildren();
  const buttons = [];

  metas.forEach((meta) => {
    const value = fieldValue(meta, key, kind);
    if (!value) return; // nothing to offer from this source

    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "meta-source-btn";
    btn.textContent = sourceLabel(meta);
    btn.title = `Click to use from ${meta.Format}: "${value}"`;
    btn.dataset.value = value;
    btn.dataset.format = meta.Format;

    btn.addEventListener("click", () => {
      setValue(value);
      if (input.type !== "hidden") input.focus();
    });

    buttonRow.appendChild(btn);
    buttons.push(btn);
  });

  // green if a source's value matches what's in the field, gold otherwise
  const updateHighlights = () => {
    const currentVal = input.value.trim();
    for (const btn of buttons) {
      const matches = btn.dataset.value.trim() === currentVal;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  let editedBtn = null;
  input.addEventListener("input", () => {
    if (!applying) {
      if (!editedBtn) {
        editedBtn = makeEditedButton(() => {
          const showing = input.value === editedBtn.dataset.value;
          if (!showing) setValue(editedBtn.dataset.value);
          fieldDiffToggle(form, current, key, buttonRow, { alreadyShowingEdits: showing });
        });
        insertEditedButton(buttonRow, editedBtn);
        buttons.push(editedBtn);
      }
      editedBtn.dataset.value = input.value;
      editedBtn.title = `Your changes: "${input.value}" (click to restore)`;
    }
    updateHighlights();
  });

  if (current) setValue(fieldValue(current, key, kind));
  updateHighlights();
}

// Identifiers are precious curated data: nothing is dropped unless the user
// explicitly clicks remove, and removals stay undoable until changes are applied.
const idKey = (scheme, value) =>
  `${(scheme || "").trim().toLowerCase()}: ${(value || "").trim()}`;

function rowValues(row) {
  return {
    scheme: row.querySelector(".identifier-type-input")?.value.trim() || "",
    value: row.querySelector(".identifier-val-input")?.value.trim() || "",
    id: row.dataset.id || "",
  };
}

/** Active (not removed) identifiers currently in the form, ISBN first. */
function readIdentifiers(form) {
  const out = [];
  const isbnInput = form.querySelector('[name="identifier_isbn"]');
  const isbn = isbnInput?.value.trim();
  if (isbn) out.push({ scheme: "isbn", value: isbn, id: isbnInput.dataset.id || "" });
  for (const row of form.querySelectorAll(".custom-identifier-row:not(.removed)")) {
    const v = rowValues(row);
    if (v.scheme || v.value) out.push(v); // scheme-less identifiers are kept
  }
  return out;
}

function createCustomIdentifierRow(scheme = "", value = "", { id = "", onChange = null, onRemove = null } = {}) {
  const row = document.createElement("div");
  row.className = "identifier-row custom-identifier-row";
  if (id) row.dataset.id = id;

  const typeInput = document.createElement("input");
  typeInput.type = "text";
  typeInput.className = "meta-input identifier-type-input";
  typeInput.placeholder = "Type (e.g. google, amazon, calibre, uuid)";
  typeInput.value = scheme;

  const valInput = document.createElement("input");
  valInput.type = "text";
  valInput.className = "meta-input identifier-val-input";
  valInput.placeholder = "Identifier value";
  valInput.value = value;

  const removeBtn = document.createElement("button");
  removeBtn.type = "button";
  removeBtn.className = "btn-remove-id";
  removeBtn.title = "Remove identifier";
  removeBtn.textContent = "×";
  removeBtn.addEventListener("click", () => onRemove?.(row));

  if (onChange) {
    typeInput.addEventListener("input", onChange);
    valInput.addEventListener("input", onChange);
  }

  row.append(typeInput, valInput, removeBtn);
  return row;
}

function populateIdentifiers(form, current, metas) {
  const isbnInput = form.querySelector('[name="identifier_isbn"]');
  const customList = form.querySelector("#custom-identifiers-list");
  const buttonRow = form.querySelector(
    '.meta-source-buttons[data-field="identifiers"]',
  );
  const addBtn = form.querySelector("#btn-add-identifier");

  if (!isbnInput || !customList || !buttonRow) return;

  const buttons = [];

  const normalize = (ids) =>
    ids
      .filter((id) => (id.scheme || id.value))
      .map((id) => idKey(id.scheme, id.value))
      .sort()
      .join(", ");

  const fromMeta = (meta) =>
    (meta.Identifiers || []).map((id) => ({
      scheme: (id.Scheme || "").trim(),
      value: (id.Value || "").trim(),
      id: id.ID || "",
    }));

  const updateHighlights = () => {
    const cur = normalize(readIdentifiers(form));
    for (const btn of buttons) {
      const matches = btn.dataset.normalized === cur;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  const firstRemoved = () => customList.querySelector(".custom-identifier-row.removed");

  function setRemoved(row, removed) {
    row.classList.toggle("removed", removed);
    for (const input of row.querySelectorAll("input")) input.disabled = removed;
    const btn = row.querySelector(".btn-remove-id");
    btn.textContent = removed ? "Undo" : "×";
    btn.title = removed ? "Restore identifier" : "Remove identifier";
    btn.classList.toggle("btn-undo-id", removed);
  }

  /** Remove click: soft-remove to the bottom; second click (Undo) restores. */
  // "edited" source for identifiers: snapshot of the user's latest state.
  let editedBtn = null;
  let editedIds = null;
  function userChanged() {
    editedIds = readIdentifiers(form).map(({ scheme, value, id }) => ({ scheme, value, id }));
    if (!editedBtn) {
      editedBtn = makeEditedButton(() => {
        const showing = normalize(readIdentifiers(form)) === editedBtn.dataset.normalized;
        if (!showing) setIdentifiers(editedIds, { keepMissing: true });
        fieldDiffToggle(form, current, "identifiers", buttonRow, { alreadyShowingEdits: showing });
      });
      insertEditedButton(buttonRow, editedBtn);
      buttons.push(editedBtn);
    }
    editedBtn.dataset.normalized = normalize(editedIds);
    updateHighlights();
  }

  function toggleRemove(row) {
    if (row.classList.contains("removed")) {
      setRemoved(row, false);
      customList.insertBefore(row, firstRemoved()); // back above the removed block
    } else {
      const v = rowValues(row);
      if (!v.scheme && !v.value && !v.id) {
        row.remove(); // blank row the user just added: nothing to lose
      } else {
        setRemoved(row, true);
        customList.appendChild(row);
      }
    }
    userChanged();
  }

  const makeRow = (scheme, value, id, removed = false) => {
    const row = createCustomIdentifierRow(scheme, value, {
      id,
      onChange: userChanged,
      onRemove: toggleRemove,
    });
    if (removed) setRemoved(row, true);
    return row;
  };

  /**
   * Replace the active identifiers with `ids`. With keepMissing, anything
   * currently active that isn't in `ids` is soft-removed (undoable) rather
   * than discarded, and matching identifiers keep their existing OPF id.
   */
  function setIdentifiers(ids, { keepMissing = false } = {}) {
    const previous = keepMissing ? readIdentifiers(form) : [];
    const prevIds = new Map(previous.map((p) => [idKey(p.scheme, p.value), p.id]));
    const incoming = new Set(ids.map((i) => idKey(i.scheme, i.value)));

    for (const row of customList.querySelectorAll(".custom-identifier-row")) {
      const removed = row.classList.contains("removed");
      // Drop active rows (re-added below); drop removed rows the new set restores.
      if (!removed || incoming.has(idKey(rowValues(row).scheme, rowValues(row).value))) {
        row.remove();
      }
    }
    isbnInput.value = "";
    delete isbnInput.dataset.id;

    for (const i of ids) {
      if (!i.scheme && !i.value) continue;
      const id = prevIds.get(idKey(i.scheme, i.value)) || i.id;
      if (i.scheme.toLowerCase() === "isbn" && !isbnInput.value) {
        isbnInput.value = i.value;
        if (id) isbnInput.dataset.id = id;
      } else {
        // Extra ISBNs land here too instead of overwriting the first.
        customList.insertBefore(makeRow(i.scheme, i.value, id), firstRemoved());
      }
    }

    for (const p of previous) {
      if (!incoming.has(idKey(p.scheme, p.value))) {
        customList.appendChild(makeRow(p.scheme, p.value, p.id, true));
      }
    }
    updateHighlights();
  }

  if (current) setIdentifiers(fromMeta(current));

  if (addBtn && !addBtn.dataset.bound) {
    addBtn.dataset.bound = "true";
    addBtn.addEventListener("click", () => {
      const row = makeRow("", "", "");
      customList.insertBefore(row, firstRemoved());
      row.querySelector(".identifier-type-input")?.focus();
    });
  }

  isbnInput.addEventListener("input", userChanged);

  buttonRow.replaceChildren();
  metas.forEach((meta) => {
    const ids = fromMeta(meta);
    if (ids.length === 0 && meta.Format !== "current") return;

    const displayVal = ids.map((id) => `${id.scheme}: ${id.value}`).join(", ");

    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "meta-source-btn";
    btn.textContent = sourceLabel(meta);
    btn.title = `Click to use from ${meta.Format}: "${displayVal}"`;
    btn.dataset.normalized = normalize(ids);
    btn.dataset.format = meta.Format;

    btn.addEventListener("click", () => setIdentifiers(ids, { keepMissing: true }));

    buttonRow.appendChild(btn);
    buttons.push(btn);
  });

  updateHighlights();
}

export function populateMetadataForm(form, metas) {
  const current = metas.find((m) => m.Format === "current") || metas[0] || null;
  for (const { key, kind } of FIELDS) {
    if (kind === "identifiers") {
      populateIdentifiers(form, current, metas);
    } else {
      populateField(form, current, metas, key, kind);
    }
  }
}

export function collectValues(form) {
  const values = {};
  for (const { key, kind } of FIELDS) {
    if (kind === "identifiers") {
      // Sent as structured objects so commas, colons and empty schemes survive.
      values[key] = readIdentifiers(form);
    } else {
      const input = form.querySelector(`[name="${key}"]`);
      if (input) values[key] = input.value;
    }
  }
  return values;
}

export async function applyValues(uuid, values) {
  const res = await fetch(`/metadata/${uuid}/apply`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(values),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || `HTTP ${res.status}`);
  }
  return res.json();
}

// ---------------------------------------------------------------------------
// Compare: pre-existing ("current") metadata vs what's in the form now
// ---------------------------------------------------------------------------

const FIELD_LABELS = {
  title: "Title",
  creators: "Authors",
  publisher: "Publisher",
  date: "Publication date",
  languages: "Languages",
  subjects: "Tags",
  identifiers: "Identifiers",
  description: "Description",
};

function listDiff(before, after) {
  const b = new Set(before.map((x) => x.toLowerCase()));
  const a = new Set(after.map((x) => x.toLowerCase()));
  const added = after.filter((x) => !b.has(x.toLowerCase()));
  const removed = before.filter((x) => !a.has(x.toLowerCase()));
  const reordered =
    !added.length && !removed.length &&
    before.map((x) => x.toLowerCase()).join("\n") !== after.map((x) => x.toLowerCase()).join("\n");
  return { added, removed, reordered, before, after };
}

export function computeChanges(current, values) {
  const changes = [];
  for (const { key, kind } of FIELDS) {
    const label = FIELD_LABELS[key] || key;
    if (kind === "list") {
      const d = listDiff(current?.[capitalize(key)] || [], parseList(values[key]));
      if (d.added.length || d.removed.length || d.reordered) changes.push({ key, label, kind, ...d });
    } else if (kind === "identifiers") {
      const fmt = (scheme, value) => `${scheme || "(no type)"}: ${value}`;
      const before = (current?.Identifiers || []).map((i) => fmt(i.Scheme, i.Value));
      const after = (values.identifiers || []).map((i) => fmt(i.scheme, i.value));
      const d = listDiff(before, after);
      if (d.added.length || d.removed.length) changes.push({ key, label, kind: "list", ...d, reordered: false });
    } else {
      const before = (current ? fieldValue(current, key, kind) : "").trim();
      const after = (values[key] || "").trim();
      // An unparseable stored date shows as an empty date input and is kept
      // by the server, so an empty field there isn't a change.
      if (kind === "date" && !after && before && !/^\d{4}-\d{2}-\d{2}$/.test(before)) continue;
      if (before !== after) changes.push({ key, label, kind: "string", before, after });
    }
  }
  return changes;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function renderChanges(changes, { compact = false } = {}) {
  if (changes.length === 0) {
    return [el("p", "compare-empty", compact ? "No changes from current." : "No changes from the current metadata.")];
  }
  return changes.map((c) => {
    const row = el("section", compact ? "compare-row compact" : "compare-row");
    row.append(el("h3", "compare-field", compact ? "Changes vs current" : c.label));
    if (c.kind === "list") {
      const items = el("div", "compare-items");
      for (const x of c.removed) items.append(el("span", "compare-chip removed", x));
      for (const x of c.added) items.append(el("span", "compare-chip added", x));
      if (c.reordered) items.append(el("span", "compare-note", `Order: ${c.before.join(", ")} → ${c.after.join(", ")}`));
      row.append(items);
    } else {
      const grid = el("div", "compare-grid");
      grid.append(
        el("span", "compare-label", "Before"),
        el("div", "compare-before", c.before || "(empty)"),
        el("span", "compare-label", "After"),
        el("div", "compare-after", c.after || "(empty)"),
      );
      row.append(grid);
    }
    return row;
  });
}

// form must already exist in the DOM with inputs named per FIELDS
// and a `.meta-source-buttons[data-field="key"]` container next to each one.
export async function mountMetadataEditor(uuid, form) {
  const statusEl = document.getElementById("edit-status");
  const saveBtn = document.getElementById("btn-save") || form.querySelector('button[type="submit"]');
  const headerTitle = document.getElementById("edit-header-title");

  try {
    // Tags and languages are edited as chips over their (hidden) inputs.
    const chipInputs = [
      ["subjects", "Add tags (comma separated)…"],
      ["languages", "e.g. eng, fra, deu"],
    ]
      .map(([name, placeholder]) => {
        const backing = form.querySelector(`[name="${name}"]`);
        return backing && createChipInput(backing, { placeholder });
      })
      .filter(Boolean);
    const commitPending = () => chipInputs.forEach((c) => c.commitPending());

    const metas = await fetchMetas(uuid);
    populateMetadataForm(form, metas);

    // Keep any open per-field diffs in sync with further edits.
    let diffFrame = 0;
    const scheduleDiffRefresh = () => {
      if (!openDiffs.size || diffFrame) return;
      diffFrame = requestAnimationFrame(() => {
        diffFrame = 0;
        refreshOpenDiffs();
      });
    };
    form.addEventListener("input", scheduleDiffRefresh);
    form.addEventListener("click", scheduleDiffRefresh);

    const compareBtn = document.getElementById("btn-compare");
    const compareDialog = document.getElementById("compare-dialog");
    if (compareBtn && compareDialog) {
      // Click outside the dialog box (on the backdrop) closes it.
      compareDialog.addEventListener("click", (e) => {
        const r = compareDialog.getBoundingClientRect();
        const inside = e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
        if (!inside) compareDialog.close();
      });
      compareBtn.addEventListener("click", () => {
        commitPending();
        const current = metas.find((m) => m.Format === "current") || null;
        document
          .getElementById("compare-body")
          .replaceChildren(...renderChanges(computeChanges(current, collectValues(form))));
        compareDialog.showModal();
      });
    }

    const current = metas.find((m) => m.Format === "current") || metas[0];
    if (current && headerTitle) {
      headerTitle.textContent = current.Title || "Untitled Book";
    }

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      commitPending(); // half-typed tag/language text is never dropped
      const values = collectValues(form);

      if (saveBtn) {
        saveBtn.disabled = true;
        saveBtn.textContent = "Applying Changes…";
      }
      if (statusEl) {
        statusEl.className = "edit-status pending";
        statusEl.textContent = "Saving metadata to Calibre library…";
      }

      try {
        const result = await applyValues(uuid, values);
        if (statusEl) {
          statusEl.className = "edit-status success";
          statusEl.innerHTML = `✓ Metadata applied successfully! <a href="/book/get/${uuid}" class="status-link">View updated book →</a>`;
        }
        if (headerTitle && values.title) {
          headerTitle.textContent = values.title;
        }
        form.dispatchEvent(
          new CustomEvent("metadata-applied", { detail: result }),
        );
      } catch (err) {
        console.error("Apply metadata failed:", err);
        if (statusEl) {
          statusEl.className = "edit-status error";
          statusEl.textContent = `✕ Failed to apply metadata: ${err.message || err}`;
        }
      } finally {
        if (saveBtn) {
          saveBtn.disabled = false;
          saveBtn.textContent = "Apply Changes";
        }
      }
    });

    return metas;
  } catch (err) {
    console.error("Failed to load metadata options:", err);
    if (statusEl) {
      statusEl.className = "edit-status error";
      statusEl.textContent = `Failed to load book metadata: ${err.message || err}`;
    }
  }
}
