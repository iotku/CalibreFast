/**
 * Tags that are really a description. Some books' metadata puts the whole
 * blurb in the subjects, so it arrives as one very long tag, or (split on its
 * commas) as dozens of tags. When the field looks like that, a note under it
 * offers three fixes:
 *
 *   - remove the tags with more than two spaces (the sentence fragments),
 *     keeping short real tags such as "Science Fiction";
 *   - remove every tag;
 *   - move the tags into the description, only while that's empty.
 *
 * All three go through the backing input as an outside change, so the
 * book's own tags are struck through with Undo (chips.ts) and the "current"
 * pill brings them back; nothing changes until the form is applied.
 */

import { parseList } from "./chips.js";
import { cleanFragment } from "./description.js";
import { el } from "./dom.js";
import { escapeHtml } from "./utils.js";

/** A tag longer than this looks like a sentence, not a tag. */
export const LONG_TAG_CHARS = 40;
/** More tags than this looks like a description split on its commas. */
export const MANY_TAGS = 25;

/** Whether the tags look like a description rather than tags. */
export function tagsNeedCleanup(tags: string[]): boolean {
    return tags.length > MANY_TAGS || tags.some((t) => t.length > LONG_TAG_CHARS);
}

/** A tag with more than two spaces (four words or more). */
export function isWordyTag(tag: string): boolean {
    return (tag.trim().match(/\s+/g)?.length ?? 0) > 2;
}

/** The tags, joined back up as the text they were split from, as description HTML. */
export function tagsAsDescription(tags: string[]): string {
    return `<p>${escapeHtml(tags.join(", "))}</p>`;
}

function descriptionEmpty(textarea: HTMLTextAreaElement): boolean {
    if (!textarea.value.trim()) return true;
    const frag = cleanFragment(textarea.value);
    return !frag.textContent?.trim() && !frag.querySelector("img");
}

/** Mount the note under the tags field (its hidden backing input). */
export function mountTagCleanup(form: HTMLFormElement, backing: HTMLInputElement, description: HTMLTextAreaElement | null): void {
    const fieldset = backing.closest("fieldset");
    if (!fieldset) return;

    const set = (tags: string[]): void => {
        backing.value = tags.join(", ");
        backing.dispatchEvent(new Event("input", { bubbles: true }));
    };

    const summary = el("span");
    const wordyBtn = el("button", { type: "button", className: "meta-source-btn", title: "Remove the tags with more than two spaces in them; shorter tags stay" });
    const allBtn = el("button", { type: "button", className: "meta-source-btn", title: "Remove every tag" }, "Remove all tags");
    const moveBtn = el("button", {
        type: "button",
        className: "meta-source-btn",
        title: "Put the tags, joined with commas, in the empty description, and remove them as tags",
    }, "Move into description");
    const note = el("p", { className: "field-note split-note tag-cleanup", hidden: true }, summary, wordyBtn, allBtn, moveBtn);
    fieldset.append(note); // under the chips (the source pills are above them)

    wordyBtn.addEventListener("click", () => set(parseList(backing.value).filter((t) => !isWordyTag(t))));
    allBtn.addEventListener("click", () => set([]));
    moveBtn.addEventListener("click", () => {
        if (!description || !descriptionEmpty(description)) return;
        const tags = parseList(backing.value);
        if (tags.length === 0) return;
        description.value = tagsAsDescription(tags);
        description.dispatchEvent(new Event("input", { bubbles: true }));
        set([]);
    });

    const render = (): void => {
        const tags = parseList(backing.value);
        note.hidden = !tagsNeedCleanup(tags);
        if (note.hidden) return;
        const longest = Math.max(...tags.map((t) => t.length));
        summary.textContent = longest > LONG_TAG_CHARS
            ? `These tags look like a description (${tags.length} ${tags.length === 1 ? "tag" : "tags"}, the longest ${longest} characters).`
            : `${tags.length} tags: these may be a description split on its commas.`;
        const wordy = tags.filter(isWordyTag).length;
        wordyBtn.hidden = wordy === 0 || wordy === tags.length;
        wordyBtn.textContent = `Remove ${wordy} wordy ${wordy === 1 ? "tag" : "tags"}`;
        moveBtn.hidden = !description || !descriptionEmpty(description);
    };
    // Tags change by chips, pills and Undo; the description by typing and pills.
    form.addEventListener("input", render);
    form.addEventListener("click", () => requestAnimationFrame(render));
    render();
}
