/**
 * Reading settings for both readers, kept per device in localStorage
 * (a phone and a desktop want different sizes and margins).
 *
 * Controls in the settings sheet bind to a setting by attribute, so adding
 * one is a default here plus markup in the template (bindSettingControls).
 */

export interface ReaderSettings {
    /** "auto" follows the system's light/dark setting. */
    theme: "auto" | "light" | "sepia" | "dark" | "black";

    // ---- EPUB ----
    /** "publisher" leaves the book's own fonts alone. */
    font: "publisher" | "serif" | "sans" | "mono";
    /** Percent of the browser's default size. */
    fontSize: number;
    /** 0 leaves the book's own line height alone. */
    lineHeight: number;
    margin: "narrow" | "medium" | "wide";
    align: "publisher" | "left" | "justify";
    hyphens: boolean;
    flow: "paginated" | "scrolled";

    // ---- PDF ----
    /**
     * Two pages side by side. The PDF reader sets this from the window's
     * shape when it opens (wide: two), so what is stored is only the last visit's.
     */
    pdfDual: boolean;
    /** Right to left (manga): spreads are swapped and "forward" is to the left. */
    pdfRtl: boolean;
    /** With two pages: the first page stands alone, as a cover does. */
    pdfCover: boolean;
    /**
     * What a page is fitted to. Zooming by hand lasts until the reader is
     * closed: the next book opens fitted again.
     */
    pdfZoom: "page" | "width";

    // ---- both, with a mouse ----
    /**
     * How long (ms) a bar the mouse brought all the way out stays after the
     * pointer moves away. 0: until hidden by hand (its handle, Escape, a click
     * on the page).
     */
    barLinger: number;
}

/** What each setting with a fixed set of choices may be (anything else stored is ignored). */
const CHOICES: Partial<Record<keyof ReaderSettings, readonly string[]>> = {
    theme: ["auto", "light", "sepia", "dark", "black"],
    font: ["publisher", "serif", "sans", "mono"],
    margin: ["narrow", "medium", "wide"],
    align: ["publisher", "left", "justify"],
    flow: ["paginated", "scrolled"],
    pdfZoom: ["page", "width"],
    barLinger: ["250", "500", "1000", "0"],
};

const KEY = "cf-reader-settings";

function defaults(): ReaderSettings {
    // Values the readers kept under their own keys before this store.
    const old = (k: string): string | null => {
        try {
            return localStorage.getItem(k);
        } catch {
            return null;
        }
    };
    return {
        theme: "auto",
        font: "publisher",
        fontSize: parseInt(old("reader-fontsize") || "", 10) || 100,
        lineHeight: 0,
        margin: "medium",
        align: "publisher",
        hyphens: false,
        flow: "paginated",
        pdfDual: old("pdf-dual") === "true",
        pdfRtl: false,
        pdfCover: true,
        pdfZoom: "page",
        barLinger: 500,
    };
}

type Listener = (settings: ReaderSettings, changed: (keyof ReaderSettings)[]) => void;

let current: ReaderSettings | null = null;
const listeners: Listener[] = [];

export function getSettings(): ReaderSettings {
    if (current) return current;
    const d = defaults();
    let stored: Partial<ReaderSettings> = {};
    try {
        stored = (JSON.parse(localStorage.getItem(KEY) || "{}") ?? {}) as Partial<ReaderSettings>;
    } catch {
        /* unreadable: start from the defaults */
    }
    // Only known keys, and only values they can have.
    const out = d as unknown as Record<string, unknown>;
    for (const [k, v] of Object.entries(stored)) {
        if (!(k in d) || typeof v !== typeof out[k]) continue;
        const choices = CHOICES[k as keyof ReaderSettings];
        if (choices && !choices.includes(String(v))) continue;
        out[k] = v;
    }
    current = d;
    return current;
}

export function setSettings(patch: Partial<ReaderSettings>): void {
    const s = getSettings() as unknown as Record<string, unknown>;
    const changed: (keyof ReaderSettings)[] = [];
    for (const [k, v] of Object.entries(patch)) {
        if (s[k] === v) continue;
        s[k] = v;
        changed.push(k as keyof ReaderSettings);
    }
    if (!changed.length) return;
    try {
        localStorage.setItem(KEY, JSON.stringify(s));
    } catch {
        /* storage full or blocked: this visit only */
    }
    for (const l of listeners) l(getSettings(), changed);
}

/** Called after any setting changes, with the keys that did. */
export function onSettingsChange(l: Listener): void {
    listeners.push(l);
}

/**
 * Wires the controls under root to the settings:
 *   <button data-setting="theme" data-value="dark">     one of a set of choices
 *   <button data-setting="fontSize" data-step="10">     adds to a number (data-min/-max)
 *   <input type="checkbox" data-setting="hyphens">
 *   <output data-shows="fontSize" data-suffix="%">      shows a value
 */
export function bindSettingControls(root: ParentNode): void {
    const coerce = (key: keyof ReaderSettings, raw: string): ReaderSettings[keyof ReaderSettings] => {
        const was = getSettings()[key];
        if (typeof was === "number") return Number(raw);
        if (typeof was === "boolean") return raw === "true";
        return raw as ReaderSettings[keyof ReaderSettings];
    };
    const show = (): void => {
        const s = getSettings() as unknown as Record<string, unknown>;
        for (const el of root.querySelectorAll<HTMLElement>("[data-setting]")) {
            const v = s[el.dataset.setting ?? ""];
            if (el instanceof HTMLInputElement && el.type === "checkbox") el.checked = v === true;
            else if (el.dataset.value !== undefined) el.setAttribute("aria-pressed", String(String(v) === el.dataset.value));
            else if (el instanceof HTMLButtonElement && el.dataset.step) {
                const next = Number(v) + Number(el.dataset.step);
                el.disabled = next < Number(el.dataset.min ?? -Infinity) || next > Number(el.dataset.max ?? Infinity);
            }
        }
        for (const el of root.querySelectorAll<HTMLElement>("[data-shows]")) {
            el.textContent = `${String(s[el.dataset.shows ?? ""])}${el.dataset.suffix ?? ""}`;
        }
    };
    root.addEventListener("click", (e) => {
        const el = e.target instanceof Element ? e.target.closest<HTMLElement>("button[data-setting]") : null;
        if (!el) return;
        const key = el.dataset.setting as keyof ReaderSettings;
        if (el.dataset.value !== undefined) setSettings({ [key]: coerce(key, el.dataset.value) });
        else if (el.dataset.step) setSettings({ [key]: Math.round((Number(getSettings()[key]) + Number(el.dataset.step)) * 100) / 100 });
    });
    root.addEventListener("change", (e) => {
        const el = e.target;
        if (el instanceof HTMLInputElement && el.dataset.setting && el.type === "checkbox") {
            setSettings({ [el.dataset.setting]: el.checked });
        }
    });
    onSettingsChange(show);
    show();
}
