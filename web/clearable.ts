/**
 * A text box with a ✕ inside its right end that empties it, shown only
 * while there's something to clear (style.css, .clearable). Clearing works
 * as typing would: an "input" event, so whatever listens to the box (a
 * search, a filter) runs again; the focus stays in the box. The browser's
 * own search-box ✕ is hidden: Firefox has none, and Chrome's is tiny.
 *
 * Used by the top bar's search, the aggregate pages' "Find a …" box and the
 * bulk editor's search and column filters.
 */

import { el } from "./dom.js";

/**
 * Wraps input (where it is, if it's in the page) with its ✕, and returns
 * the wrapper. label names the ✕ for screen readers and as its tooltip.
 */
export function clearable(input: HTMLInputElement, label: string): HTMLElement {
    const x = el("button", { type: "button", className: "clear-x", title: label, ariaLabel: label }, "✕");
    // Keep the focus (and a phone's keyboard) in the box while pressing it.
    x.addEventListener("mousedown", (e) => e.preventDefault());
    x.addEventListener("click", () => {
        input.value = "";
        input.dispatchEvent(new Event("input", { bubbles: true }));
        input.focus();
    });
    // The ✕ hides on :placeholder-shown, which needs a placeholder.
    if (!input.placeholder) input.placeholder = " ";
    const wrap = el("span", { className: "clearable" });
    input.replaceWith(wrap);
    wrap.append(input, x);
    return wrap;
}
