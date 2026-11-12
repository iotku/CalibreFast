import { createChipInput, parseList, uniqueItems, type ChipInput } from "./chips.js";
import { attachSuggest } from "./suggest.js";
import { languageChipLabel, languageKey, loadLanguageCodes, preferredLanguageCode } from "./languages.js";
import type { BookTarget } from "./book-target.js";
import { createDescriptionEditor } from "./description.js";
import { closeOnBackdropClick, errorMessage, maybeId } from "./dom.js";
import { isbnDigits, mountIsbnTools } from "./isbn-tools.js";
import { joinAuthors, splitAuthors, tidyAuthors } from "./authors.js";
import type { MetaSource } from "./types.js";

type FieldKind = "string" | "date" | "list" | "identifiers";
type FieldKey = "title" | "creators" | "description" | "publisher" | "date" | "languages" | "subjects" | "identifiers";
type TextKey = Exclude<FieldKey, "identifiers">;

/** An identifier as the editor sends it (Go: identifierList). */
interface Ident {
  scheme: string;
  value: string;
  id: string;
}

/** What the editor sends to /metadata/<uuid>/apply. */
export type FormValues = Partial<Record<TextKey, string>> & { identifiers?: Ident[] };

/** One changed field, for the compare views. */
type Change = { key: FieldKey; label: string } & (
  | { kind: "list"; added: string[]; removed: string[]; reordered: boolean; before: string[]; after: string[] }
  | { kind: "string"; before: string; after: string }
);

const FIELDS: { key: FieldKey; kind: FieldKind }[] = [
  { key: "title", kind: "string" },
  { key: "creators", kind: "list" },
  { key: "description", kind: "string" },
  { key: "publisher", kind: "string" },
  { key: "date", kind: "date" },
  { key: "languages", kind: "list" }, // several per book; order kept
  { key: "subjects", kind: "list" },
  { key: "identifiers", kind: "identifiers" },
];

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

// UploadedBookMeta embeds OPFMetadata anonymously in Go, so the JSON is
// flattened — Title/Creators/etc. live directly on the meta object
// alongside Format/FileName.
function metaProp(meta: MetaSource, key: FieldKey): unknown {
  return (meta as unknown as Record<string, unknown>)[capitalize(key)];
}
function metaList(meta: MetaSource | null, key: FieldKey): string[] {
  const v = meta ? metaProp(meta, key) : null;
  // A source's tags or languages without repeats (any case), as the chips hold them.
  return Array.isArray(v) ? uniqueItems(v as string[]) : [];
}

function fieldValue(meta: MetaSource, key: FieldKey, kind: FieldKind): string {
  switch (kind) {
    case "string":
    case "date": {
      const v = metaProp(meta, key);
      return typeof v === "string" ? v : "";
    }
    case "list":
      // Authors' names can hold commas ("Donovan, Alan"): "A & B", as Calibre.
      return key === "creators" ? joinAuthors(metaList(meta, key)) : metaList(meta, key).join(", ");
    case "identifiers":
      return (meta.Identifiers || [])
        .map((id) => `${id.Scheme}: ${id.Value}`)
        .join(", ");
  }
}

function sourceLabel(meta: MetaSource): string {
  return meta.FileName ? `${meta.Format} (${meta.FileName})` : meta.Format;
}

/** A source pill under a field. */
function sourceButton(meta: MetaSource, title: string): HTMLButtonElement {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "meta-source-btn";
  btn.textContent = sourceLabel(meta);
  btn.title = title;
  btn.dataset["format"] = meta.Format;
  if (meta.External) btn.dataset["external"] = "true";
  return btn;
}

/** A lookup source without values: adding it only removes that source's pills. */
function emptyLookup(format: string): MetaSource {
  return {
    Format: format, FileName: "", Title: "", Creators: null, Description: "", Publisher: "", Date: "",
    Languages: null, Subjects: null, Identifiers: null, External: true,
  };
}

/** A newer answer from a lookup replaces that source's earlier pill. */
function dropOldLookup(buttons: HTMLButtonElement[], meta: MetaSource): void {
  if (!meta.External) return;
  for (let i = buttons.length - 1; i >= 0; i--) {
    const b = buttons[i];
    if (b?.dataset["external"] && b.dataset["format"] === meta.Format) {
      b.remove();
      buttons.splice(i, 1);
    }
  }
}

export async function fetchMetas(book: BookTarget): Promise<MetaSource[]> {
  const res = await fetch(book.metadata, { credentials: "same-origin" });
  if (!res.ok) throw new Error(`Failed to load metadata: HTTP ${res.status}`);
  return (await res.json()) as MetaSource[];
}

/**
 * "edited" source button: once the user changes a field, their latest edit is
 * kept as its own source so they can flip to "current" (or a format) to look
 * and come back without retyping.
 */
function makeEditedButton(onClick: () => void): HTMLButtonElement {
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
const openDiffs = new Map<FieldKey, { el: HTMLElement; render: () => void }>();

function fieldDiffToggle(
  form: HTMLFormElement,
  current: MetaSource | null,
  key: FieldKey,
  buttonRow: HTMLElement,
  { alreadyShowingEdits }: { alreadyShowingEdits: boolean },
): void {
  const open = openDiffs.get(key);
  if (open && alreadyShowingEdits) {
    open.el.remove();
    openDiffs.delete(key);
    return;
  }
  if (open) return open.render();

  const el = document.createElement("div");
  el.className = "field-diff";
  const render = (): void => {
    const changes = computeChanges(current, collectValues(form)).filter((c) => c.key === key);
    el.replaceChildren(...renderChanges(changes, { compact: true }));
  };
  (buttonRow.closest("fieldset") ?? buttonRow).append(el);
  openDiffs.set(key, { el, render });
  render();
}

function refreshOpenDiffs(): void {
  for (const { render } of openDiffs.values()) render();
}

function insertEditedButton(buttonRow: HTMLElement, btn: HTMLButtonElement): void {
  // right after "current" (the pre-existing metadata) when present
  const first = buttonRow.firstElementChild;
  if (first instanceof HTMLElement && first.dataset["format"] === "current") first.after(btn);
  else buttonRow.prepend(btn);
}

/** Adds a source's pill to a field (a lookup's answer arrives later). */
type AddSource = (meta: MetaSource) => void;

function populateField(
  form: HTMLFormElement,
  current: MetaSource | null,
  metas: MetaSource[],
  key: FieldKey,
  kind: FieldKind,
): AddSource | null {
  const input = form.querySelector<HTMLInputElement | HTMLTextAreaElement>(`[name="${key}"]`);
  const buttonRow = form.querySelector<HTMLElement>(`.meta-source-buttons[data-field="${key}"]`);
  if (!input || !buttonRow) return null;

  let applying = false; // true while we set the value programmatically
  const setValue = (v: string): void => {
    applying = true;
    input.value = v;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    applying = false;
  };

  buttonRow.replaceChildren();
  const buttons: HTMLButtonElement[] = [];

  // green if a source's value matches what's in the field, gold otherwise
  const updateHighlights = (): void => {
    const currentVal = input.value.trim();
    for (const btn of buttons) {
      const matches = (btn.dataset["value"] ?? "").trim() === currentVal;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  const addSource: AddSource = (meta) => {
    dropOldLookup(buttons, meta);
    const value = fieldValue(meta, key, kind);
    if (!value) return; // nothing to offer from this source

    const btn = sourceButton(meta, `Click to use from ${meta.Format}: "${value}"`);
    btn.dataset["value"] = value;
    btn.addEventListener("click", () => {
      setValue(value);
      if (input.type !== "hidden") input.focus();
    });
    buttonRow.appendChild(btn);
    buttons.push(btn);
    updateHighlights();
  };
  metas.forEach(addSource);

  let editedBtn: HTMLButtonElement | null = null;
  input.addEventListener("input", () => {
    if (!applying) {
      if (!editedBtn) {
        const btn = makeEditedButton(() => {
          const edited = btn.dataset["value"] ?? "";
          const showing = input.value === edited;
          if (!showing) setValue(edited);
          fieldDiffToggle(form, current, key, buttonRow, { alreadyShowingEdits: showing });
        });
        editedBtn = btn;
        insertEditedButton(buttonRow, btn);
        buttons.push(btn);
      }
      editedBtn.dataset["value"] = input.value;
      editedBtn.title = `Your changes: "${input.value}" (click to restore)`;
    }
    updateHighlights();
  });

  if (current) setValue(fieldValue(current, key, kind));
  updateHighlights();
  return addSource;
}

// Identifiers are precious curated data: nothing is dropped unless the user
// explicitly clicks remove, and removals stay undoable until changes are applied.
const idKey = (scheme: string, value: string): string =>
  `${(scheme || "").trim().toLowerCase()}: ${(value || "").trim()}`;

function rowValues(row: HTMLElement): Ident {
  return {
    scheme: row.querySelector<HTMLInputElement>(".identifier-type-input")?.value.trim() || "",
    value: row.querySelector<HTMLInputElement>(".identifier-val-input")?.value.trim() || "",
    id: row.dataset["id"] || "",
  };
}

/** Active (not removed) identifiers currently in the form, ISBN first. */
function readIdentifiers(form: HTMLFormElement): Ident[] {
  const out: Ident[] = [];
  const isbnInput = form.querySelector<HTMLInputElement>('[name="identifier_isbn"]');
  const isbn = isbnInput?.value.trim();
  if (isbnInput && isbn) out.push({ scheme: "isbn", value: isbn, id: isbnInput.dataset["id"] || "" });
  for (const row of form.querySelectorAll<HTMLElement>(".custom-identifier-row:not(.removed)")) {
    const v = rowValues(row);
    if (v.scheme || v.value) out.push(v); // scheme-less identifiers are kept
  }
  return out;
}

interface IdentifierRowOptions {
  id?: string;
  onChange?: (() => void) | null;
  onRemove?: ((row: HTMLDivElement) => void) | null;
}

function createCustomIdentifierRow(
  scheme = "",
  value = "",
  { id = "", onChange = null, onRemove = null }: IdentifierRowOptions = {},
): HTMLDivElement {
  const row = document.createElement("div");
  row.className = "identifier-row custom-identifier-row";
  if (id) row.dataset["id"] = id;

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

/** The identifiers field, for sources added later and the ISBN finder. */
interface IdentifierField {
  addSource: AddSource;
  /** Put an ISBN in the ISBN field; a different one there is crossed out (Undo). */
  useIsbn(isbn: string): void;
}

/** Compares identifiers for merging: an ISBN by its 13 digits. */
const mergeKey = (scheme: string, value: string): string =>
  scheme.trim().toLowerCase() === "isbn" ? `isbn: ${isbnDigits(value)}` : idKey(scheme, value);

function populateIdentifiers(form: HTMLFormElement, current: MetaSource | null, metas: MetaSource[]): IdentifierField | null {
  const isbnEl = form.querySelector<HTMLInputElement>('[name="identifier_isbn"]');
  const listEl = form.querySelector<HTMLElement>("#custom-identifiers-list");
  const rowEl = form.querySelector<HTMLElement>('.meta-source-buttons[data-field="identifiers"]');
  const addBtn = form.querySelector<HTMLButtonElement>("#btn-add-identifier");

  if (!isbnEl || !listEl || !rowEl) return null;
  // Non-null from here on (also inside the closures below).
  const isbnInput: HTMLInputElement = isbnEl;
  const customList: HTMLElement = listEl;
  const buttonRow: HTMLElement = rowEl;

  const buttons: HTMLButtonElement[] = [];

  const normalize = (ids: Ident[]): string =>
    ids
      .filter((id) => (id.scheme || id.value))
      .map((id) => idKey(id.scheme, id.value))
      .sort()
      .join(", ");

  const fromMeta = (meta: MetaSource): Ident[] =>
    (meta.Identifiers || []).map((id) => ({
      scheme: (id.Scheme || "").trim(),
      value: (id.Value || "").trim(),
      id: id.ID || "",
    }));

  const updateHighlights = (): void => {
    const active = readIdentifiers(form);
    const cur = normalize(active);
    const have = new Set(active.map((i) => mergeKey(i.scheme, i.value)));
    for (const btn of buttons) {
      // A lookup's identifiers are added rather than switched to, so its
      // pill matches once they're all there.
      const keys = btn.dataset["keys"];
      const matches = keys !== undefined ? keys.split("\n").every((k) => have.has(k)) : btn.dataset["normalized"] === cur;
      btn.classList.toggle("meta-source-match", matches);
      btn.classList.toggle("meta-source-diff", !matches);
    }
  };

  const firstRemoved = (): Element | null => customList.querySelector(".custom-identifier-row.removed");

  // Crossed-out rows (with Undo) are only for identifiers the book had, each
  // once. Anything else, such as an ISBN tried and then replaced, just goes.
  const original = new Set(current ? fromMeta(current).map((i) => idKey(i.scheme, i.value)) : []);
  const isCrossedOut = (key: string): boolean =>
    [...customList.querySelectorAll<HTMLElement>(".custom-identifier-row.removed")]
      .some((row) => idKey(rowValues(row).scheme, rowValues(row).value) === key);
  const keepCrossedOut = (scheme: string, value: string): boolean => {
    const key = idKey(scheme, value);
    return original.has(key) && !isCrossedOut(key);
  };
  /** Cross out an identifier the book had; drop any other. */
  function park(scheme: string, value: string, id: string): void {
    if (keepCrossedOut(scheme, value)) customList.appendChild(makeRow(scheme, value, id, true));
  }

  function setRemoved(row: HTMLElement, removed: boolean): void {
    row.classList.toggle("removed", removed);
    for (const input of row.querySelectorAll("input")) input.disabled = removed;
    const btn = row.querySelector<HTMLButtonElement>(".btn-remove-id");
    if (!btn) return;
    btn.textContent = removed ? "Undo" : "×";
    btn.title = removed ? "Restore identifier" : "Remove identifier";
    btn.classList.toggle("btn-undo-id", removed);
  }

  /** Remove click: soft-remove to the bottom; second click (Undo) restores. */
  // "edited" source for identifiers: snapshot of the user's latest state.
  let editedBtn: HTMLButtonElement | null = null;
  let editedIds: Ident[] = [];
  function userChanged(): void {
    editedIds = readIdentifiers(form).map(({ scheme, value, id }) => ({ scheme, value, id }));
    if (!editedBtn) {
      const btn = makeEditedButton(() => {
        const showing = normalize(readIdentifiers(form)) === btn.dataset["normalized"];
        if (!showing) setIdentifiers(editedIds, { keepMissing: true });
        fieldDiffToggle(form, current, "identifiers", buttonRow, { alreadyShowingEdits: showing });
      });
      editedBtn = btn;
      insertEditedButton(buttonRow, btn);
      buttons.push(btn);
    }
    editedBtn.dataset["normalized"] = normalize(editedIds);
    updateHighlights();
  }

  function toggleRemove(row: HTMLElement): void {
    if (row.classList.contains("removed")) {
      setRemoved(row, false);
      customList.insertBefore(row, firstRemoved()); // back above the removed block
    } else {
      const v = rowValues(row);
      if (!keepCrossedOut(v.scheme, v.value)) {
        row.remove(); // not one the book had (or already crossed out): nothing to lose
      } else {
        setRemoved(row, true);
        customList.appendChild(row);
      }
    }
    userChanged();
  }

  const makeRow = (scheme: string, value: string, id: string, removed = false): HTMLDivElement => {
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
  function setIdentifiers(ids: Ident[], { keepMissing = false }: { keepMissing?: boolean } = {}): void {
    const previous = keepMissing ? readIdentifiers(form) : [];
    const prevIds = new Map(previous.map((p) => [idKey(p.scheme, p.value), p.id]));
    const incoming = new Set(ids.map((i) => idKey(i.scheme, i.value)));

    for (const row of customList.querySelectorAll<HTMLElement>(".custom-identifier-row")) {
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
      if (!incoming.has(idKey(p.scheme, p.value))) park(p.scheme, p.value, p.id);
    }
    updateHighlights();
  }

  /**
   * Add `ids` (from a lookup) without removing anything: ones already there
   * are skipped, crossed-out ones are restored, and an ISBN fills the ISBN
   * field if it's empty.
   */
  function mergeIdentifiers(ids: Ident[]): void {
    const have = new Set(readIdentifiers(form).map((i) => mergeKey(i.scheme, i.value)));
    for (const i of ids) {
      const k = mergeKey(i.scheme, i.value);
      if (have.has(k)) continue;
      have.add(k);
      const removed = [...customList.querySelectorAll<HTMLElement>(".custom-identifier-row.removed")]
        .find((row) => mergeKey(rowValues(row).scheme, rowValues(row).value) === k);
      if (removed) {
        setRemoved(removed, false);
        customList.insertBefore(removed, firstRemoved());
      } else if (i.scheme.toLowerCase() === "isbn" && !isbnInput.value.trim()) {
        isbnInput.value = i.value;
      } else {
        customList.insertBefore(makeRow(i.scheme, i.value, ""), firstRemoved());
      }
    }
    updateHighlights();
  }

  function useIsbn(isbn: string): void {
    const old = isbnInput.value.trim();
    if (old && isbnDigits(old) === isbnDigits(isbn)) return;
    if (old) park("isbn", old, isbnInput.dataset["id"] || "");
    isbnInput.value = isbn;
    delete isbnInput.dataset["id"];
    // Back to an ISBN the book had: it's no longer crossed out.
    const back = [...customList.querySelectorAll<HTMLElement>(".custom-identifier-row.removed")]
      .find((row) => mergeKey(rowValues(row).scheme, rowValues(row).value) === mergeKey("isbn", isbn));
    if (back) {
      if (back.dataset["id"]) isbnInput.dataset["id"] = back.dataset["id"];
      back.remove();
    }
    userChanged();
  }

  if (current) setIdentifiers(fromMeta(current));

  if (addBtn && !addBtn.dataset["bound"]) {
    addBtn.dataset["bound"] = "true";
    addBtn.addEventListener("click", () => {
      const row = makeRow("", "", "");
      customList.insertBefore(row, firstRemoved());
      row.querySelector<HTMLInputElement>(".identifier-type-input")?.focus();
    });
  }

  isbnInput.addEventListener("input", userChanged);

  const addSource: AddSource = (meta) => {
    dropOldLookup(buttons, meta);
    const ids = fromMeta(meta);
    if (ids.length === 0 && meta.Format !== "current") return;

    const displayVal = ids.map((id) => `${id.scheme}: ${id.value}`).join(", ");
    if (meta.External) {
      const btn = sourceButton(meta, `Click to add from ${meta.Format}: "${displayVal}"`);
      btn.dataset["keys"] = ids.map((i) => mergeKey(i.scheme, i.value)).join("\n");
      btn.addEventListener("click", () => mergeIdentifiers(ids));
      buttonRow.appendChild(btn);
      buttons.push(btn);
    } else {
      const btn = sourceButton(meta, `Click to use from ${meta.Format}: "${displayVal}"`);
      btn.dataset["normalized"] = normalize(ids);
      btn.addEventListener("click", () => setIdentifiers(ids, { keepMissing: true }));
      buttonRow.appendChild(btn);
      buttons.push(btn);
    }
    updateHighlights();
  };

  buttonRow.replaceChildren();
  metas.forEach(addSource);
  updateHighlights();
  return { addSource, useIsbn };
}

/** The editor after loading: more sources can be added (from a lookup). */
export interface MetadataForm {
  addSource: AddSource;
  /** Take a lookup's pills (by its Format) away again. */
  removeSource(format: string): void;
  useIsbn(isbn: string): void;
  /** What taking every value a lookup has would change, vs current. */
  preview(meta: MetaSource): HTMLElement[];
  /**
   * Take every value a lookup has (its pills), then apply. A book waiting in
   * the import queue isn't applied: that would add it to the library before
   * its cover and the rest are looked at.
   */
  acceptAll(meta: MetaSource): void;
}

export function populateMetadataForm(form: HTMLFormElement, metas: MetaSource[], { applyOnAccept = true } = {}): MetadataForm {
  const current = metas.find((m) => m.Format === "current") || metas[0] || null;
  const adders: AddSource[] = [];
  let identifiers: IdentifierField | null = null;
  for (const { key, kind } of FIELDS) {
    if (kind === "identifiers") {
      identifiers = populateIdentifiers(form, current, metas);
      if (identifiers) adders.push(identifiers.addSource);
    } else {
      const add = populateField(form, current, metas, key, kind);
      if (add) adders.push(add);
    }
  }
  return {
    addSource: (meta) => adders.forEach((add) => add(meta)),
    // A lookup source with no values: each field drops that source's pill.
    removeSource: (format) => adders.forEach((add) => add(emptyLookup(format))),
    useIsbn: (isbn) => identifiers?.useIsbn(isbn),
    preview: (meta) => renderChanges(computeChanges(current, withSource(collectValues(form), meta))),
    acceptAll: (meta) => {
      for (const btn of form.querySelectorAll<HTMLButtonElement>(".meta-source-btn[data-external]")) {
        if (btn.dataset["format"] === meta.Format) btn.click();
      }
      if (applyOnAccept) form.requestSubmit();
    },
  };
}

/** values with every field meta has taken from it; identifiers are added. */
function withSource(values: FormValues, meta: MetaSource): FormValues {
  const out: FormValues = { ...values };
  for (const { key, kind } of FIELDS) {
    if (kind === "identifiers") {
      const ids = [...(values.identifiers ?? [])];
      const have = new Set(ids.map((i) => mergeKey(i.scheme, i.value)));
      for (const id of meta.Identifiers ?? []) {
        if (!have.has(mergeKey(id.Scheme, id.Value))) ids.push({ scheme: id.Scheme, value: id.Value, id: "" });
      }
      out.identifiers = ids;
    } else {
      const v = fieldValue(meta, key, kind);
      if (v) out[key as TextKey] = v;
    }
  }
  return out;
}

export function collectValues(form: HTMLFormElement): FormValues {
  const values: FormValues = {};
  for (const { key, kind } of FIELDS) {
    if (kind === "identifiers") {
      // Sent as structured objects so commas, colons and empty schemes survive.
      values.identifiers = readIdentifiers(form);
    } else {
      const input = form.querySelector<HTMLInputElement | HTMLTextAreaElement>(`[name="${key}"]`);
      if (input) values[key as TextKey] = input.value;
    }
  }
  return values;
}

/** What applying answers; next is the import queue's next book, if any. */
export interface ApplyResult {
  uuid: string;
  title: string;
  next?: string;
}

export async function applyValues(book: BookTarget, values: FormValues): Promise<ApplyResult> {
  const res = await fetch(book.apply, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(values),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || `HTTP ${res.status}`);
  }
  return (await res.json()) as ApplyResult;
}

// ---------------------------------------------------------------------------
// Compare: pre-existing ("current") metadata vs what's in the form now
// ---------------------------------------------------------------------------

const FIELD_LABELS: Record<FieldKey, string> = {
  title: "Title",
  creators: "Authors",
  publisher: "Publisher",
  date: "Publication date",
  languages: "Languages",
  subjects: "Tags",
  identifiers: "Identifiers",
  description: "Description",
};

function listDiff(before: string[], after: string[]) {
  const b = new Set(before.map((x) => x.toLowerCase()));
  const a = new Set(after.map((x) => x.toLowerCase()));
  const added = after.filter((x) => !b.has(x.toLowerCase()));
  const removed = before.filter((x) => !a.has(x.toLowerCase()));
  const reordered =
    !added.length && !removed.length &&
    before.map((x) => x.toLowerCase()).join("\n") !== after.map((x) => x.toLowerCase()).join("\n");
  return { added, removed, reordered, before, after };
}

export function computeChanges(current: MetaSource | null, values: FormValues): Change[] {
  const changes: Change[] = [];
  for (const { key, kind } of FIELDS) {
    const label = FIELD_LABELS[key] || key;
    if (kind === "list") {
      const after = key === "creators" ? splitAuthors(values.creators) : parseList(values[key as TextKey]);
      const d = listDiff(metaList(current, key), after);
      if (d.added.length || d.removed.length || d.reordered) changes.push({ key, label, kind: "list", ...d });
    } else if (kind === "identifiers") {
      const fmt = (scheme: string, value: string): string => `${scheme || "(no type)"}: ${value}`;
      const before = (current?.Identifiers || []).map((i) => fmt(i.Scheme, i.Value));
      const after = (values.identifiers || []).map((i) => fmt(i.scheme, i.value));
      const d = listDiff(before, after);
      if (d.added.length || d.removed.length) changes.push({ key, label, kind: "list", ...d, reordered: false });
    } else {
      const before = (current ? fieldValue(current, key, kind) : "").trim();
      const after = (values[key as TextKey] || "").trim();
      // An unparseable stored date shows as an empty date input and is kept
      // by the server, so an empty field there isn't a change.
      if (kind === "date" && !after && before && !/^\d{4}-\d{2}-\d{2}$/.test(before)) continue;
      if (before !== after) changes.push({ key, label, kind: "string", before, after });
    }
  }
  return changes;
}

function el(tag: keyof HTMLElementTagNameMap, cls?: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function renderChanges(changes: Change[], { compact = false }: { compact?: boolean } = {}): HTMLElement[] {
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
// For a book waiting in the import queue (book.pending), applying adds it to
// the library with the form's values, changed or not.
export async function mountMetadataEditor(book: BookTarget, form: HTMLFormElement): Promise<MetaSource[] | undefined> {
  const statusEl = maybeId("edit-status");
  const saveBtn = maybeId("btn-save", HTMLButtonElement) ?? form.querySelector<HTMLButtonElement>('button[type="submit"]');
  const headerTitle = maybeId("edit-header-title");
  const applyLabel = saveBtn?.textContent ?? "Apply Changes";

  try {
    // Tags and languages are edited as chips over their (hidden) inputs.
    const languages = {
      // Chips read "English (eng)"; suggestions prefer the 3-letter code and
      // skip languages the book already has under any code ("en" vs "eng").
      labelFor: languageChipLabel,
      canonical: preferredLanguageCode,
      key: languageKey,
    };
    const chipFields: {
      name: string;
      placeholder: string;
      category: string;
      placeholders: string[];
      lang?: typeof languages;
    }[] = [
      { name: "subjects", placeholder: "Add tags (comma separated)…", category: "tag", placeholders: [] },
      // "und" (undetermined) disappears as soon as a real language is added.
      { name: "languages", placeholder: "e.g. eng, fra, deu", category: "language", placeholders: ["und"], lang: languages },
    ];
    const chipInputs = chipFields
      .map(({ name, placeholder, category, placeholders, lang }) => {
        const backing = form.querySelector<HTMLInputElement>(`[name="${name}"]`);
        if (!backing) return null;
        const chips = createChipInput(backing, { placeholder, placeholders, ...(lang && { labelFor: lang.labelFor }) });
        attachSuggest(chips.textInput, {
          category,
          multi: true,
          exclude: chips.activeValues,
          ...(lang && { canonical: lang.canonical, key: lang.key }),
          onPick: () => chips.commitPending(), // picking a suggestion adds it
        });
        if (lang) void loadLanguageCodes().then(() => chips.rerender());
        return chips;
      })
      .filter((c): c is ChipInput => c !== null);

    // Description: formatted preview + raw HTML tabs over the textarea.
    const descriptionInput = form.querySelector<HTMLTextAreaElement>('[name="description"]');
    if (descriptionInput) createDescriptionEditor(descriptionInput);

    // Library-backed suggestions for the plain list/single-value fields.
    const creatorsInput = form.querySelector<HTMLInputElement>('[name="creators"]');
    if (creatorsInput) {
      attachSuggest(creatorsInput, { category: "author", multi: true, separators: "&" });
      // Authors written another way ("Hailu, Gebreyesus; Negash, Ghirmai", or
      // "Last, First") are saved as written; Split rewrites them, only when
      // clicked (the "current" pill undoes it).
      const splitNote = maybeId("creators-split");
      const splitBtn = maybeId("btn-split-authors", HTMLButtonElement);
      const splitPreview = maybeId("creators-split-preview");
      if (splitNote && splitBtn && splitPreview) {
        creatorsInput.addEventListener("input", () => {
          const asIs = splitAuthors(creatorsInput.value);
          const tidy = tidyAuthors(creatorsInput.value);
          splitNote.hidden = joinAuthors(tidy) === joinAuthors(asIs);
          splitBtn.textContent = tidy.length > asIs.length ? `Split into ${tidy.length} authors` : "First name first";
          splitPreview.textContent = joinAuthors(tidy);
        });
        splitBtn.addEventListener("click", () => {
          creatorsInput.value = joinAuthors(tidyAuthors(creatorsInput.value));
          creatorsInput.dispatchEvent(new Event("input", { bubbles: true }));
        });
      }
    }
    const publisherInput = form.querySelector<HTMLInputElement>('[name="publisher"]');
    if (publisherInput) attachSuggest(publisherInput, { category: "publisher" });

    // Swap title and author (often mixed up in imported metadata). Firing
    // "input" on both updates the edited markers, diffs and unsaved guard.
    const titleInput = form.querySelector<HTMLInputElement>('[name="title"]');
    maybeId("swap-title-author")?.addEventListener("click", () => {
      if (!titleInput || !creatorsInput) return;
      [titleInput.value, creatorsInput.value] = [creatorsInput.value, titleInput.value];
      for (const input of [titleInput, creatorsInput]) input.dispatchEvent(new Event("input", { bubbles: true }));
    });

    const commitPending = (): void => chipInputs.forEach((c) => c.commitPending());

    const metas = await fetchMetas(book);
    const editor = populateMetadataForm(form, metas, { applyOnAccept: !book.pending });

    // Find ISBNs in the book's files; look the book up online.
    mountIsbnTools({
      book,
      form,
      currentIsbn: () => form.querySelector<HTMLInputElement>('[name="identifier_isbn"]')?.value.trim() ?? "",
      titleAndAuthor: () => ({
        title: titleInput?.value.trim() ?? "",
        author: tidyAuthors(creatorsInput?.value)[0] ?? "", // a search hint, not saved
      }),
      useIsbn: editor.useIsbn,
      addSource: editor.addSource,
      removeSource: editor.removeSource,
      preview: editor.preview,
      acceptAll: editor.acceptAll,
    });

    // Keep any open per-field diffs in sync with further edits.
    let diffFrame = 0;
    const scheduleDiffRefresh = (): void => {
      if (!openDiffs.size || diffFrame) return;
      diffFrame = requestAnimationFrame(() => {
        diffFrame = 0;
        refreshOpenDiffs();
      });
    };
    form.addEventListener("input", scheduleDiffRefresh);
    form.addEventListener("click", scheduleDiffRefresh);

    const compareBtn = maybeId("btn-compare");
    const compareDialog = maybeId("compare-dialog", HTMLDialogElement);
    if (compareBtn && compareDialog) {
      closeOnBackdropClick(compareDialog);
      compareBtn.addEventListener("click", () => openCompare({ leaving: false }));
    }

    /**
     * leaving: opened because the user cancelled the browser's "leave page?"
     * prompt — title and footer ask whether to apply the unapplied changes.
     */
    function openCompare({ leaving }: { leaving: boolean }): void {
      if (!compareDialog) return;
      commitPending();
      const current = metas.find((m) => m.Format === "current") || null;
      maybeId("compare-body")?.replaceChildren(...renderChanges(computeChanges(current, collectValues(form))));
      const title = maybeId("compare-title");
      if (title) title.textContent = leaving
        ? "You have unapplied changes"
        : "Changes vs current metadata";
      const footer = document.getElementById("compare-leave-actions");
      if (footer) footer.hidden = !leaving;
      if (!compareDialog.open) compareDialog.showModal();
    }

    maybeId("compare-apply")?.addEventListener("click", () => {
      compareDialog?.close();
      form.requestSubmit(); // same path as Apply Changes
    });

    // -----------------------------------------------------------------------
    // Unapplied-changes guard. "Dirty" = form differs from what was loaded or
    // last applied, or there's half-typed text in a chip box.
    // -----------------------------------------------------------------------
    const snapshot = (): string => JSON.stringify(collectValues(form));
    let baseline = snapshot();
    let saving = false;
    let added = false; // a waiting book, now in the library
    // Discarded from the import queue (the page's Discard): nothing left to lose.
    form.addEventListener("metadata-discarded", () => {
      added = true;
    });
    const isDirty = (): boolean =>
      !saving && !added &&
      (chipInputs.some((c) => c.textInput.value.trim()) || snapshot() !== baseline);

    // Apply Changes is only for changes the admin made: off until the form
    // differs from what was loaded (what the editor itself filled in or tidied
    // is part of that), and off again when the changes are undone.
    const updateApply = (): void => {
      if (!saveBtn || saving || book.pending) return; // a waiting book can always be added
      const dirty = isDirty();
      saveBtn.disabled = !dirty;
      saveBtn.title = dirty ? "" : "Nothing to apply: no field has been changed";
    };
    let applyFrame = 0;
    const scheduleApply = (): void => {
      if (applyFrame) return;
      applyFrame = requestAnimationFrame(() => {
        applyFrame = 0;
        updateApply();
      });
    };
    for (const type of ["input", "change", "click"]) form.addEventListener(type, scheduleApply);
    updateApply();

    window.addEventListener("beforeunload", (e) => {
      if (!isDirty()) return;
      e.preventDefault();
      e.returnValue = ""; // older browsers need this to show the prompt
      // Timers are paused while the browser's prompt is open. "Leave" unloads
      // the page so this never runs; "Cancel" (stay) runs it and we show what
      // would have been lost, offering to apply it.
      setTimeout(() => openCompare({ leaving: true }), 100);
    });

    const current = metas.find((m) => m.Format === "current") || metas[0];
    if (current && headerTitle) {
      headerTitle.textContent = current.Title || "Untitled Book";
    }

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      commitPending(); // half-typed tag/language text is never dropped
      // Every way to apply (the button, Enter, Accept all, the leave prompt)
      // comes here: nothing changed, nothing saved.
      if (!book.pending && !isDirty()) {
        if (statusEl) {
          statusEl.className = "edit-status pending";
          statusEl.textContent = "Nothing to apply: no field has been changed.";
        }
        updateApply();
        return;
      }
      const values = collectValues(form);

      if (saveBtn) {
        saveBtn.disabled = true;
        saveBtn.textContent = book.pending ? "Adding…" : "Applying Changes…";
      }
      if (statusEl) {
        statusEl.className = "edit-status pending";
        statusEl.textContent = book.pending ? "Adding the book to the Calibre library…" : "Saving metadata to Calibre library…";
      }

      try {
        saving = true;
        const result = await applyValues(book, values);
        baseline = JSON.stringify(values); // applied: nothing unsaved now
        added = true;
        if (statusEl) {
          statusEl.className = "edit-status success";
          statusEl.textContent = !book.pending
            ? "✓ Metadata applied. Opening the book…"
            : result.next ? "✓ Added to the library. Opening the next book to review…" : "✓ Added to the library. Opening the book…";
        }
        form.dispatchEvent(
          new CustomEvent("metadata-applied", { detail: result }),
        );
        if (book.pending && result.next) {
          // On through the queue; the next page says what was just added.
          location.assign(`/import/${encodeURIComponent(result.next)}?added=${encodeURIComponent(result.uuid)}`);
        } else {
          // The book page with the changes (?applied= skips its 30s cache).
          location.assign(`/book/get/${encodeURIComponent(result.uuid)}?applied=${Date.now()}`);
        }
      } catch (err) {
        console.error("Apply metadata failed:", err);
        if (statusEl) {
          statusEl.className = "edit-status error";
          statusEl.textContent = `✕ Failed to apply metadata: ${errorMessage(err)}`;
        }
      } finally {
        saving = false; // a failed apply leaves the changes unsaved (guard back on)
        if (saveBtn) {
          saveBtn.textContent = applyLabel;
          // A waiting book is gone from the queue once added: no second click.
          if (book.pending) saveBtn.disabled = added;
        }
        updateApply();
      }
    });

    return metas;
  } catch (err) {
    console.error("Failed to load metadata options:", err);
    if (statusEl) {
      statusEl.className = "edit-status error";
      statusEl.textContent = `Failed to load book metadata: ${errorMessage(err)}`;
    }
  }
}
