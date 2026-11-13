/**
 * Chip ("bean") editor layered over a plain backing <input>.
 *
 * The backing input keeps holding "a, b, c" (active chips only), so form
 * collection, source buttons and highlighting work unchanged. Text is typed
 * in an entry row above the chips (so new chips never push it around) and is
 * only added on Enter or the Add button; commas there separate several items.
 * Removing one of the book's own values (those the field was loaded with)
 * strikes it through and parks it at the end with Undo; nothing the book
 * had is lost until the form is applied. Values added since (typed, or from
 * a source's pill) just go: Undo is for what the book had, and the pills
 * bring the others back.
 *
 * Chips containing `splitOn` (";") get a Split button, and the field gets a
 * "Split all" button, for tags that were glued together with semicolons. A
 * split chip is replaced by its parts (no struck-through copy is kept).
 */

const lower = (s: string): string => s.toLowerCase();

interface Chip {
    text: string;
    removed: boolean;
}

export interface ChipInputOptions {
    placeholder?: string;
    splitOn?: string;
    placeholders?: string[];
    /** Text shown on a chip for a value (default: the value). */
    labelFor?: (value: string) => string;
}

/** The chip widget's root element, with its small public API. */
export type ChipInput = HTMLDivElement & {
    /** Add any half-typed text as chips (call before reading the form). */
    commitPending(): void;
    /** Active (not removed) chip values. */
    activeValues(): string[];
    /** The entry text box (for attaching autocomplete). */
    textInput: HTMLInputElement;
    /** Redraw the chips (e.g. once labelFor has its data). */
    rerender(): void;
    /** Add one value as a chip, as if typed (what's half typed stays). */
    addValue(value: string): void;
};

export function parseList(value: string | null | undefined): string[] {
    return uniqueItems((value || "").split(","));
}

/**
 * Items trimmed, without empty ones or repeats (ignoring case): the first
 * spelling stays. A chip field never holds the same value twice (Go:
 * uniqueTags in main.go).
 */
export function uniqueItems(items: string[]): string[] {
    const out: string[] = [];
    const seen = new Set<string>();
    for (const raw of items) {
        const t = raw.trim();
        if (t && !seen.has(lower(t))) {
            seen.add(lower(t));
            out.push(t);
        }
    }
    return out;
}

/**
 * placeholders: values that only stand in for "nothing set" (e.g. "und" for
 * languages). They're removed outright as soon as a real value is added or
 * edited in.
 */
export function createChipInput(
    backing: HTMLInputElement,
    { placeholder = "", splitOn = ";", placeholders = [], labelFor = (v) => v }: ChipInputOptions = {},
): ChipInput {
    const placeholderSet = new Set(placeholders.map((p) => p.toLowerCase()));
    let chips: Chip[] = [];
    let emitting = false;
    /** The book's own values (lowercase), from the first value set from outside (loading). */
    let original: Set<string> | null = null;
    const isOriginal = (c: Chip): boolean => original?.has(lower(c.text)) ?? false;

    const root = document.createElement("div") as ChipInput;
    root.className = "chip-input";
    const list = document.createElement("div");
    list.className = "chip-list";
    const text = document.createElement("input");
    text.type = "text";
    text.className = "chip-text";
    text.placeholder = placeholder;
    text.autocomplete = "off";
    text.spellcheck = false;
    const addBtn = document.createElement("button");
    addBtn.type = "button";
    addBtn.className = "chip-add-btn";
    addBtn.textContent = "Add";
    const entry = document.createElement("div");
    entry.className = "chip-entry";
    entry.append(text, addBtn);
    const tools = document.createElement("div");
    tools.className = "chip-tools";
    const splitAllBtn = document.createElement("button");
    splitAllBtn.type = "button";
    splitAllBtn.className = "chip-tool-btn";
    tools.append(splitAllBtn);

    root.append(entry, list, tools);
    backing.type = "hidden";
    backing.after(root);

    const active = () => chips.filter((c) => !c.removed);

    function emit() {
        const value = active().map((c) => c.text).join(", ");
        if (value === backing.value) return;
        backing.value = value;
        emitting = true;
        backing.dispatchEvent(new Event("input", { bubbles: true }));
        emitting = false;
    }

    /** Index where active chips end (removed ones are parked after). */
    function activeEnd() {
        const i = chips.findIndex((c) => c.removed);
        return i === -1 ? chips.length : i;
    }

    /** Add items (deduped case-insensitively); revives removed duplicates. */
    function addItems(items: string[], at = activeEnd()): void {
        for (const raw of items) {
            const t = raw.trim();
            if (!t) continue;
            const existing = chips.find((c) => lower(c.text) === lower(t));
            if (existing) {
                if (existing.removed) {
                    chips.splice(chips.indexOf(existing), 1);
                    existing.removed = false;
                    chips.splice(Math.min(at, activeEnd()), 0, existing);
                    at++;
                }
                continue;
            }
            chips.splice(at, 0, { text: t, removed: false });
            at++;
        }
    }

    /** Once there's a real value, placeholder chips (e.g. "und") go away. */
    function dropPlaceholders() {
        if (!placeholderSet.size) return;
        const hasReal = chips.some((c) => !c.removed && !placeholderSet.has(lower(c.text)));
        if (hasReal) chips = chips.filter((c) => !placeholderSet.has(lower(c.text)));
    }

    function commitText() {
        const items = parseList(text.value);
        text.value = "";
        if (items.length === 0) return;
        addItems(items);
        dropPlaceholders();
        render();
        emit();
    }

    function toggle(chip: Chip): void {
        chips.splice(chips.indexOf(chip), 1);
        if (chip.removed) {
            chip.removed = false;
            chips.splice(activeEnd(), 0, chip);
        } else if (isOriginal(chip)) {
            chip.removed = true;
            chips.push(chip); // parked with Undo
        } // else gone: it wasn't the book's
        render();
        emit();
    }

    function split(chip: Chip): void {
        const at = chips.indexOf(chip);
        const parts = chip.text.split(splitOn).map((s) => s.trim()).filter(Boolean);
        // Replaced by its parts outright (no struck-through copy): the parts
        // carry the same content, and "current" / Compare still show the original.
        chips.splice(at, 1);
        addItems(parts, at);
        render();
        emit();
    }

    /** Apply an inline edit: commas split, empty soft-removes, dupes merge. */
    function applyEdit(chip: Chip, value: string): void {
        const parts = parseList(value);
        if (parts.length === 0) {
            if (!chip.removed) toggle(chip); // struck through with its original text; Undo restores
            else render();
            return;
        }
        const at = chips.indexOf(chip);
        const [first, ...rest] = parts;
        const dupe = chips.find((c) => c !== chip && lower(c.text) === lower(first));
        if (dupe) {
            chips.splice(at, 1); // merged into the existing chip
            if (dupe.removed) toggle(dupe);
            addItems(rest, Math.min(at, activeEnd()));
        } else {
            chip.text = first;
            addItems(rest, at + 1);
        }
        dropPlaceholders();
        render();
        emit();
    }

    function startEdit(chip: Chip, el: HTMLElement, label: HTMLElement): void {
        if (el.classList.contains("editing")) return;
        // Textarea so long values wrap inside the field instead of widening it.
        const input = document.createElement("textarea");
        input.className = "chip-edit";
        input.value = chip.text;
        input.rows = 1;
        input.spellcheck = false;
        input.autocomplete = "off";
        const fit = () => {
            input.style.height = "auto";
            input.style.height = `${input.scrollHeight}px`;
        };
        let done = false;
        const finish = (save: boolean): void => {
            if (done) return;
            done = true;
            const value = input.value.replace(/\s*\n\s*/g, " "); // values are single-line
            if (save && value.trim() !== chip.text) applyEdit(chip, value);
            else render();
        };
        input.addEventListener("keydown", (e) => {
            if (e.key === "Enter") {
                e.preventDefault(); // no newlines, and don't submit the form
                finish(true);
                text.focus();
            } else if (e.key === "Escape") {
                e.preventDefault();
                e.stopPropagation();
                finish(false);
            }
        });
        input.addEventListener("input", fit);
        input.addEventListener("blur", () => finish(true));
        label.replaceWith(input);
        el.classList.add("editing");
        fit();
        input.focus();
        input.select();
    }

    function render() {
        const nodes = chips.map((chip) => {
            const el = document.createElement("span");
            el.className = "chip";
            el.classList.toggle("removed", chip.removed);
            const label = document.createElement("span");
            label.className = "chip-label";
            label.textContent = labelFor(chip.text);
            el.append(label);
            if (!chip.removed) {
                // Anywhere on the chip edits it, except its own buttons (×, Split).
                el.tabIndex = 0;
                el.setAttribute("role", "button");
                el.title = "Click to edit";
                el.addEventListener("click", (e) => {
                    if (e.target instanceof Element && e.target.closest(".chip-btn, .chip-edit")) return;
                    startEdit(chip, el, label);
                });
                el.addEventListener("keydown", (e) => {
                    if (e.target !== el) return;
                    if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        startEdit(chip, el, label);
                    }
                });
            }

            if (!chip.removed && splitOn && chip.text.includes(splitOn)) {
                const s = document.createElement("button");
                s.type = "button";
                s.className = "chip-btn chip-split";
                s.textContent = `Split ${splitOn}`;
                s.title = `Split into ${chip.text.split(splitOn).filter((p) => p.trim()).length} separate items`;
                s.addEventListener("click", () => split(chip));
                el.append(s);
            }

            const x = document.createElement("button");
            x.type = "button";
            x.className = "chip-btn";
            x.textContent = chip.removed ? "Undo" : "×";
            x.title = chip.removed ? `Keep ${labelFor(chip.text)}` : `Remove ${labelFor(chip.text)}`;
            x.addEventListener("click", () => toggle(chip));
            el.append(x);
            return el;
        });
        list.replaceChildren(...nodes);
        list.hidden = nodes.length === 0;

        const splittable = active().filter((c) => splitOn && c.text.includes(splitOn));
        splitAllBtn.hidden = splittable.length === 0;
        splitAllBtn.textContent = `Split all on “${splitOn}” (${splittable.length})`;
    }

    splitAllBtn.addEventListener("click", () => {
        for (const c of active().filter((c) => c.text.includes(splitOn))) split(c);
    });

    // Only Enter or Add commits; typing commas just types commas. Pending text
    // stays in the box (and is committed before apply/compare via commitPending).
    text.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
            e.preventDefault(); // Enter must not submit the form
            commitText();
        }
    });
    addBtn.addEventListener("click", () => {
        commitText();
        text.focus();
    });

    /**
     * External changes (initial load, source buttons): the book's own values
     * no longer present are struck through rather than dropped; others just
     * go. A value the field already has, in any case, keeps the field's
     * spelling. The first change is the load: its values are the book's.
     */
    backing.addEventListener("input", () => {
        if (emitting) return;
        original ??= new Set(parseList(backing.value).map(lower));
        const known = new Map(chips.map((c) => [lower(c.text), c.text]));
        const next = parseList(backing.value).map((t) => known.get(lower(t)) ?? t);
        const nextKeys = new Set(next.map(lower));
        const prevActive = active().filter((c) => !nextKeys.has(lower(c.text)));
        const prevRemoved = chips.filter(
            (c) => c.removed && !nextKeys.has(lower(c.text)),
        );
        chips = next.map((t) => ({ text: t, removed: false }));
        if (next.join(", ") !== backing.value) emit(); // the backing value without repeats
        for (const c of [...prevActive, ...prevRemoved]) {
            if (isOriginal(c)) chips.push({ text: c.text, removed: true });
        }
        render();
    });

    /** Commit any half-typed text (call before reading the form). */
    root.commitPending = commitText;
    /** Active chip values (for autocomplete exclusion). */
    root.activeValues = () => active().map((c) => c.text);
    root.textInput = text;
    root.rerender = render;
    root.addValue = (value: string) => {
        addItems([value]);
        dropPlaceholders();
        render();
        emit();
    };
    render();
    return root;
}
