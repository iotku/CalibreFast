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
  if (!res.ok) throw new Error(`failed to load metadata: ${res.status}`);
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
    btn.title = value;
    btn.dataset.value = value;

    btn.addEventListener("click", () => {
      input.value = value;
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });

    buttonRow.appendChild(btn);
    buttons.push(btn);
  });

  // green if a source's value matches what's currently typed in the field,
  // yellow otherwise — recomputed live so manual edits fall out of "current"
  const updateHighlights = () => {
    for (const btn of buttons) {
      const matches = btn.dataset.value === input.value;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  input.addEventListener("input", updateHighlights);
  updateHighlights();
}

export function populateMetadataForm(form, metas) {
  const current = metas.find((m) => m.Format === "current") || null;
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
  if (!res.ok) throw new Error(`failed to apply metadata: ${res.status}`);
  return res.json();
}

// form must already exist in the DOM (see metadata_form.html) with inputs
// named per FIELDS and a `.meta-source-buttons[data-field="key"]` container
// next to each one.
export async function mountMetadataEditor(uuid, form) {
  const metas = await fetchMetas(uuid);
  populateMetadataForm(form, metas);

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const values = collectValues(form);
    try {
      await applyValues(uuid, values);
      form.dispatchEvent(
        new CustomEvent("metadata-applied", { detail: values }),
      );
    } catch (err) {
      console.error(err);
    }
  });

  return metas;
}
