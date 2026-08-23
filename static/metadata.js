const FIELDS = [
  { key: "title", kind: "string" },
  { key: "creators", kind: "list" },
  { key: "description", kind: "string" },
  { key: "publisher", kind: "string" },
  { key: "date", kind: "date" },
  { key: "language", kind: "string" },
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

function populateField(form, current, metas, key, kind) {
  const input = form.querySelector(`[name="${key}"]`);
  const buttonRow = form.querySelector(
    `.meta-source-buttons[data-field="${key}"]`,
  );
  if (!input || !buttonRow) return;

  if (current) {
    input.value = fieldValue(current, key, kind);
  }

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

    btn.addEventListener("click", () => {
      input.value = value;
      input.dispatchEvent(new Event("input", { bubbles: true }));
      input.focus();
    });

    buttonRow.appendChild(btn);
    buttons.push(btn);
  });

  // green if a source's value matches what's currently typed in the field,
  // yellow/gold otherwise — recomputed live so manual edits fall out of "current"
  const updateHighlights = () => {
    const currentVal = input.value.trim();
    for (const btn of buttons) {
      const matches = btn.dataset.value.trim() === currentVal;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  input.addEventListener("input", updateHighlights);
  updateHighlights();
}

function createCustomIdentifierRow(scheme = "", value = "", onChange = null) {
  const row = document.createElement("div");
  row.className = "identifier-row custom-identifier-row";

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
  removeBtn.addEventListener("click", () => {
    row.remove();
    if (onChange) onChange();
  });

  if (onChange) {
    typeInput.addEventListener("input", onChange);
    valInput.addEventListener("input", onChange);
  }

  row.appendChild(typeInput);
  row.appendChild(valInput);
  row.appendChild(removeBtn);

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

  function getCurrentIdentifiersNormalized() {
    const items = [];
    const isbn = isbnInput.value.trim();
    if (isbn) items.push(`isbn: ${isbn.toLowerCase()}`);
    for (const row of customList.querySelectorAll(".custom-identifier-row")) {
      const type = row.querySelector(".identifier-type-input")?.value.trim();
      const val = row.querySelector(".identifier-val-input")?.value.trim();
      if (type && val) {
        items.push(`${type.toLowerCase()}: ${val}`);
      }
    }
    return items.sort().join(", ");
  }

  function getSourceIdentifiersNormalized(meta) {
    return (meta.Identifiers || [])
      .map((id) => `${(id.Scheme || "").trim().toLowerCase()}: ${(id.Value || "").trim()}`)
      .filter((s) => s !== ":")
      .sort()
      .join(", ");
  }

  const updateHighlights = () => {
    const cur = getCurrentIdentifiersNormalized();
    for (const btn of buttons) {
      const srcNorm = btn.dataset.normalized;
      const matches = srcNorm === cur;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  function setIdentifiersFromList(ids) {
    customList.replaceChildren();
    isbnInput.value = "";
    for (const id of ids || []) {
      const scheme = (id.Scheme || "").trim();
      const val = (id.Value || "").trim();
      if (scheme.toLowerCase() === "isbn") {
        isbnInput.value = val;
      } else if (scheme || val) {
        const row = createCustomIdentifierRow(scheme, val, updateHighlights);
        customList.appendChild(row);
      }
    }
    updateHighlights();
  }

  if (current) {
    setIdentifiersFromList(current.Identifiers || []);
  }

  if (addBtn && !addBtn.dataset.bound) {
    addBtn.dataset.bound = "true";
    addBtn.addEventListener("click", () => {
      const row = createCustomIdentifierRow("", "", updateHighlights);
      customList.appendChild(row);
      row.querySelector(".identifier-type-input")?.focus();
    });
  }

  isbnInput.addEventListener("input", updateHighlights);

  buttonRow.replaceChildren();
  metas.forEach((meta) => {
    const ids = meta.Identifiers || [];
    if (ids.length === 0 && meta.Format !== "current") return;

    const norm = getSourceIdentifiersNormalized(meta);
    const displayVal = ids.map((id) => `${id.Scheme}: ${id.Value}`).join(", ");

    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "meta-source-btn";
    btn.textContent = sourceLabel(meta);
    btn.title = `Click to use from ${meta.Format}: "${displayVal}"`;
    btn.dataset.normalized = norm;

    btn.addEventListener("click", () => {
      setIdentifiersFromList(ids);
    });

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
      const parts = [];
      const isbnInput = form.querySelector('[name="identifier_isbn"]');
      if (isbnInput && isbnInput.value.trim()) {
        parts.push(`isbn: ${isbnInput.value.trim()}`);
      }
      const customRows = form.querySelectorAll(".custom-identifier-row");
      for (const row of customRows) {
        const type = row.querySelector(".identifier-type-input")?.value.trim();
        const val = row.querySelector(".identifier-val-input")?.value.trim();
        if (type && val) {
          parts.push(`${type}: ${val}`);
        }
      }
      values[key] = parts.join(", ");
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

// form must already exist in the DOM with inputs named per FIELDS
// and a `.meta-source-buttons[data-field="key"]` container next to each one.
export async function mountMetadataEditor(uuid, form) {
  const statusEl = document.getElementById("edit-status");
  const saveBtn = document.getElementById("btn-save") || form.querySelector('button[type="submit"]');
  const headerTitle = document.getElementById("edit-header-title");

  try {
    const metas = await fetchMetas(uuid);
    populateMetadataForm(form, metas);

    const current = metas.find((m) => m.Format === "current") || metas[0];
    if (current && headerTitle) {
      headerTitle.textContent = current.Title || "Untitled Book";
    }

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
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
