/**
 * Description editor: an editable rendered preview plus a raw HTML tab, both
 * backed by the original <textarea name="description"> (so source buttons,
 * "edited", Compare and the unsaved-changes guard keep working).
 *
 * Pasted rich text (e.g. a shop listing) keeps its formatting but is cleaned
 * with the same rules the book page applies when displaying descriptions
 * (bluemonday UGCPolicy in book.go): formatting tags survive, inline styles,
 * classes, scripts and other markup don't. So the preview matches what will
 * actually be shown.
 */
// Mirrors bluemonday's UGCPolicy element list.
const ALLOWED = new Set(("a abbr acronym address article aside b bdi bdo blockquote br caption cite code col colgroup " +
    "dd del details dfn div dl dt em figcaption figure footer h1 h2 h3 h4 h5 h6 header hgroup hr i " +
    "img ins kbd li main mark nav ol p pre q rp rt ruby s samp section small span strike strong sub " +
    "summary sup table tbody td tfoot th thead time tr tt u ul var wbr").split(" "));
// Dropped together with everything inside them.
const DROP = new Set("script style noscript template iframe object embed svg math canvas form input button select textarea video audio head title meta link".split(" "));
const SAFE_URL = /^(https?:|mailto:|\/|#)/i;
/**
 * Inline styles are stripped (the book page doesn't allow them), but shops
 * often bold/italicise via CSS: keep that meaning as real tags.
 */
function styleToTags(el, doc) {
    if (!(el instanceof HTMLElement) || !el.getAttribute("style"))
        return;
    const st = el.style;
    const wraps = [];
    const w = st.fontWeight;
    if (w === "bold" || w === "bolder" || Number(w) >= 600)
        wraps.push("strong");
    if (st.fontStyle === "italic" || st.fontStyle === "oblique")
        wraps.push("em");
    if ((st.textDecorationLine || st.textDecoration || "").includes("underline"))
        wraps.push("u");
    if ((st.textDecorationLine || st.textDecoration || "").includes("line-through"))
        wraps.push("s");
    for (const t of wraps) {
        const wrap = doc.createElement(t);
        wrap.append(...el.childNodes);
        el.append(wrap);
    }
}
/** Clean HTML to what the book page will display. */
export function cleanHTML(html) {
    const doc = new DOMParser().parseFromString(html || "", "text/html");
    const walk = (node) => {
        for (const child of [...node.childNodes]) {
            if (child.nodeType === Node.COMMENT_NODE) {
                child.remove(); // e.g. Word's <!--StartFragment-->
                continue;
            }
            if (!(child instanceof Element))
                continue;
            const tag = child.tagName.toLowerCase();
            if (DROP.has(tag)) {
                child.remove();
                continue;
            }
            walk(child);
            styleToTags(child, doc);
            if (!ALLOWED.has(tag)) {
                child.replaceWith(...child.childNodes); // unwrap, keep content
                continue;
            }
            for (const attr of [...child.attributes]) {
                const name = attr.name.toLowerCase();
                const keep = name === "title" ||
                    name === "lang" ||
                    name === "dir" ||
                    (tag === "a" && name === "href" && SAFE_URL.test(attr.value.trim())) ||
                    (tag === "img" && (name === "alt" || (name === "src" && /^https?:/i.test(attr.value.trim())))) ||
                    ((tag === "td" || tag === "th") && (name === "colspan" || name === "rowspan"));
                if (!keep)
                    child.removeAttribute(attr.name);
            }
            // A <span> with no attributes left adds nothing (pasted layout noise).
            if (tag === "span" && child.attributes.length === 0) {
                child.replaceWith(...child.childNodes);
            }
        }
    };
    walk(doc.body);
    return doc.body.innerHTML.trim();
}
const TOOLS = [
    ["bold", "B", "Bold"],
    ["italic", "I", "Italic"],
    ["underline", "U", "Underline"],
    ["insertUnorderedList", "• List", "Bulleted list"],
    ["insertOrderedList", "1. List", "Numbered list"],
    ["createLink", "Link", "Link selected text"],
    ["removeFormat", "Clear", "Clear formatting on selection (incl. lists, links, headings)"],
];
export function createDescriptionEditor(textarea) {
    const root = document.createElement("div");
    root.className = "desc-editor";
    const bar = document.createElement("div");
    bar.className = "desc-toolbar";
    const tabs = document.createElement("div");
    tabs.className = "desc-tabs";
    tabs.setAttribute("role", "tablist");
    const mkTab = (label) => {
        const b = document.createElement("button");
        b.type = "button";
        b.className = "desc-tab";
        b.setAttribute("role", "tab");
        b.textContent = label;
        tabs.append(b);
        return b;
    };
    const previewTab = mkTab("Preview");
    const htmlTab = mkTab("HTML");
    const tools = document.createElement("div");
    tools.className = "desc-tools";
    for (const [cmd, label, title] of TOOLS) {
        const b = document.createElement("button");
        b.type = "button";
        b.className = "desc-tool";
        b.textContent = label;
        b.title = title;
        b.dataset.cmd = cmd;
        tools.append(b);
    }
    const cleanBtn = document.createElement("button");
    cleanBtn.type = "button";
    cleanBtn.className = "desc-tool";
    cleanBtn.textContent = "Clean HTML";
    cleanBtn.title = "Strip markup the book page won't show (inline styles, classes, scripts…)";
    tools.append(cleanBtn);
    bar.append(tabs, tools);
    const preview = document.createElement("div");
    preview.className = "desc-preview description";
    preview.contentEditable = "true";
    preview.dataset.placeholder = textarea.placeholder || "";
    preview.setAttribute("role", "textbox");
    preview.setAttribute("aria-multiline", "true");
    preview.setAttribute("aria-label", "Description (formatted)");
    textarea.classList.add("desc-source");
    textarea.spellcheck = false;
    textarea.replaceWith(root);
    root.append(bar, preview, textarea);
    let mode = "preview";
    let fromPreview = false;
    const render = () => {
        preview.innerHTML = cleanHTML(textarea.value);
    };
    function setMode(m) {
        mode = m;
        root.dataset.mode = m;
        previewTab.setAttribute("aria-selected", String(m === "preview"));
        htmlTab.setAttribute("aria-selected", String(m === "html"));
        for (const b of tools.querySelectorAll("[data-cmd]"))
            b.disabled = m !== "preview";
        if (m === "preview")
            render();
    }
    // Browsers' editing commands sprinkle inline styles into contenteditable;
    // what's saved from the preview is always cleaned. (The HTML tab is left
    // raw for manual control; "Clean HTML" cleans it on demand.)
    function syncFromPreview() {
        fromPreview = true;
        textarea.value = cleanHTML(preview.innerHTML);
        textarea.dispatchEvent(new Event("input", { bubbles: true }));
        fromPreview = false;
    }
    // Programmatic or HTML-tab changes re-render the preview.
    textarea.addEventListener("input", () => {
        if (!fromPreview && mode === "preview")
            render();
    });
    preview.addEventListener("input", syncFromPreview);
    preview.addEventListener("paste", (e) => {
        const html = e.clipboardData?.getData("text/html");
        const text = e.clipboardData?.getData("text/plain");
        if (!html && !text)
            return;
        e.preventDefault();
        if (!html) {
            document.execCommand("insertText", false, text); // fires "input"
            return;
        }
        // Insert the cleaned fragment ourselves: Chrome's insertHTML re-adds
        // inline styles copied from the surrounding text.
        const sel = window.getSelection();
        if (!sel || !sel.rangeCount || !preview.contains(sel.anchorNode))
            return;
        const range = sel.getRangeAt(0);
        range.deleteContents();
        const frag = range.createContextualFragment(cleanHTML(html));
        const last = frag.lastChild;
        range.insertNode(frag);
        if (last) {
            range.setStartAfter(last);
            range.collapse(true);
            sel.removeAllRanges();
            sel.addRange(range);
        }
        syncFromPreview();
    });
    /** Closest ancestor (inside the preview) matching selector, or null. */
    const within = (node, selector) => {
        const el = node instanceof Element ? node : node?.parentElement;
        const hit = el?.closest(selector);
        return hit && preview.contains(hit) ? hit : null;
    };
    /**
     * "Clear": removeFormat only strips inline formatting, so also take the
     * selection out of lists (incl. nested) and links, and turn headings /
     * quotes / code blocks back into paragraphs.
     */
    function clearFormatting() {
        const sel = window.getSelection();
        if (!sel || !sel.rangeCount)
            return;
        document.execCommand("removeFormat", false);
        document.execCommand("unlink", false);
        // Toggling the list command on list items removes that list level.
        for (let i = 0; i < 10; i++) {
            const list = within(sel.anchorNode, "ul, ol") || within(sel.focusNode, "ul, ol");
            if (!list)
                break;
            const cmd = list.tagName === "OL" ? "insertOrderedList" : "insertUnorderedList";
            document.execCommand(cmd, false);
            if ((within(sel.anchorNode, "ul, ol") || within(sel.focusNode, "ul, ol")) === list)
                break; // didn't change
        }
        if (within(sel.anchorNode, "h1,h2,h3,h4,h5,h6,blockquote,pre") || within(sel.focusNode, "h1,h2,h3,h4,h5,h6,blockquote,pre")) {
            document.execCommand("formatBlock", false, "p");
        }
    }
    tools.addEventListener("mousedown", (e) => {
        if (e.target instanceof Element && e.target.closest(".desc-tool"))
            e.preventDefault(); // keep the selection
    });
    tools.addEventListener("click", (e) => {
        const b = e.target instanceof Element ? e.target.closest("[data-cmd]") : null;
        if (!b || mode !== "preview")
            return;
        preview.focus();
        if (b.dataset.cmd === "createLink") {
            const url = prompt("Link URL (https://…)");
            if (!url || !SAFE_URL.test(url.trim()))
                return;
            document.execCommand("createLink", false, url.trim());
        }
        else if (b.dataset.cmd === "removeFormat") {
            clearFormatting();
        }
        else if (b.dataset["cmd"]) {
            document.execCommand(b.dataset["cmd"], false);
        }
    });
    cleanBtn.addEventListener("click", () => {
        const cleaned = cleanHTML(textarea.value);
        if (cleaned === textarea.value)
            return;
        textarea.value = cleaned;
        textarea.dispatchEvent(new Event("input", { bubbles: true }));
        if (mode === "preview")
            render();
    });
    previewTab.addEventListener("click", () => setMode("preview"));
    htmlTab.addEventListener("click", () => setMode("html"));
    try {
        document.execCommand("defaultParagraphSeparator", false, "p");
    }
    catch {
        /* not supported: browser default */
    }
    setMode("preview");
    return root;
}
