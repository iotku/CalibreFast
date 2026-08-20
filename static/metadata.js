const FIELDS = [
  { key: "title", kind: "string" },
  { key: "creators", kind: "list" },
  { key: "description", kind: "string" },
  { key: "publisher", kind: "string" },
  { key: "date", kind: "string" },
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

export function populateMetadataForm(form, metas) {
  const current = metas.find((m) => m.Format === "current") || metas[0] || null;
  for (const { key, kind } of FIELDS) {
    populateField(form, current, metas, key, kind);
  }
}

export function collectValues(form) {
  const values = {};
  for (const { key } of FIELDS) {
    const input = form.querySelector(`[name="${key}"]`);
    if (input) values[key] = input.value;
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
