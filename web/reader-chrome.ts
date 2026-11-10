/**
 * What both readers share around the page (templates/reader-parts.html):
 *   - a top bar (back, title, tools) and a bottom bar (previous/next, a
 *     scrubber, the position), which hide together while reading;
 *   - panels: the Contents/Bookmarks/Search sidebar and the settings sheet;
 *   - the theme (light, sepia, dark, black, or the system's).
 *
 * The readers decide what a tap or key means; this module only offers
 * toggleBars(), closePanels() and friends.
 */

import { byId, maybeId } from "./dom.js";
import { bindSettingControls, getSettings, onSettingsChange } from "./reader-settings.js";

export interface Panel {
    isOpen(): boolean;
    open(): void;
    close(): void;
}

export interface Sidebar extends Panel {
    /** Opens on a tab: "contents", "bookmarks", "search", … */
    show(tab: string): void;
}

export interface Chrome {
    sidebar: Sidebar;
    settings: Panel;
    barsHidden(): boolean;
    showBars(): void;
    hideBars(): void;
    toggleBars(): void;
    /** Closes any open panel. True if one was open (the press that did it shouldn't also turn the page). */
    closePanels(): boolean;
    /** The line under the book's title. */
    setChapter(text: string): void;
    /** Position text, and how far through the book (0..1) for the scrubber. */
    setPosition(text: string, fraction?: number): void;
    /** Whether the scrubber can be used yet (EPUB locations take a moment). */
    setScrubbable(on: boolean): void;
    /** The quiet sync line in the settings sheet. */
    setStatus(text: string): void;
    /** A short message that fades, for things the reader just did. */
    toast(text: string): void;
}

export interface ChromeOptions {
    /**
     * The scrubber moved to fraction (0..1). While dragging (done false)
     * return the text to preview; on release (done true) go there.
     */
    onScrub(fraction: number, done: boolean): string | void;
    /** The page colours changed (theme setting, or the system's with "auto"). */
    onTheme?(): void;
}

/** The current theme's page colours, as CSS sets them (web/reader.css). */
export function pageColors(): { bg: string; fg: string; link: string; dark: boolean } {
    const css = getComputedStyle(document.documentElement);
    const v = (name: string): string => css.getPropertyValue(name).trim();
    return { bg: v("--page-bg"), fg: v("--page-fg"), link: v("--page-link"), dark: v("--page-dark") === "1" };
}

const SCRUB_MAX = 1000;

export function setupChrome(opts: ChromeOptions): Chrome {
    const body = document.body;
    const bars = [...document.querySelectorAll<HTMLElement>(".r-bar")];
    const sidebarEl = byId("toc-sidebar");
    const sheetEl = byId("settings-sheet");

    // ---- theme ------------------------------------------------------------

    const themeMeta = document.querySelector<HTMLMetaElement>('meta[name="theme-color"]');
    const applyTheme = (): void => {
        document.documentElement.dataset.theme = getSettings().theme;
        if (themeMeta) themeMeta.content = getComputedStyle(document.documentElement).getPropertyValue("--chrome-bg").trim();
    };
    applyTheme();
    const themeChanged = (): void => {
        applyTheme();
        opts.onTheme?.();
    };
    onSettingsChange((_s, changed) => {
        if (changed.includes("theme")) themeChanged();
    });
    matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
        if (getSettings().theme === "auto") themeChanged();
    });

    // ---- bars -------------------------------------------------------------

    const barsHidden = (): boolean => body.classList.contains("bars-hidden");
    const setBars = (hidden: boolean): void => {
        body.classList.toggle("bars-hidden", hidden);
        for (const b of bars) b.inert = hidden;
    };
    const showBars = (): void => setBars(false);
    const hideBars = (): void => {
        if (anyOpen()) return; // a panel hangs off the bars
        // Don't pull the bar from under a keyboard user.
        if (bars.some((b) => b.matches(":focus-within") && b.querySelector(":focus-visible"))) return;
        setBars(true);
    };

    // The sidebar starts below the top bar and the sheet above the bottom one.
    const measure = (): void => {
        const top = bars.find((b) => b.classList.contains("r-top"));
        const bottom = bars.find((b) => b.classList.contains("r-bottom"));
        if (top) body.style.setProperty("--bar-top", `${top.offsetHeight}px`);
        if (bottom) body.style.setProperty("--bar-bottom", `${bottom.offsetHeight}px`);
    };
    const ro = new ResizeObserver(measure);
    for (const b of bars) ro.observe(b);

    // ---- panels -----------------------------------------------------------

    const panels: { el: HTMLElement; panel: Panel }[] = [];
    function makePanel(el: HTMLElement, btn: HTMLElement | null, onOpen?: () => void): Panel {
        const panel: Panel = {
            isOpen: () => el.classList.contains("open"),
            open() {
                for (const p of panels) if (p.panel !== panel) p.panel.close();
                showBars();
                el.classList.add("open");
                el.inert = false;
                btn?.setAttribute("aria-expanded", "true");
                onOpen?.();
            },
            close() {
                if (!panel.isOpen()) return;
                // Focus goes back to the button rather than being lost in a hidden panel.
                if (el.contains(document.activeElement)) btn?.focus();
                el.classList.remove("open");
                el.inert = true;
                btn?.setAttribute("aria-expanded", "false");
            },
        };
        el.inert = true;
        btn?.addEventListener("click", () => (panel.isOpen() ? panel.close() : panel.open()));
        for (const c of el.querySelectorAll<HTMLElement>("[data-close]")) c.addEventListener("click", () => panel.close());
        panels.push({ el, panel });
        return panel;
    }
    const anyOpen = (): boolean => panels.some((p) => p.panel.isOpen());
    const closePanels = (): boolean => {
        const was = anyOpen();
        for (const p of panels) p.panel.close();
        return was;
    };

    // Sidebar, with tabs.
    const tabs = [...sidebarEl.querySelectorAll<HTMLElement>("[role=tab]")];
    const selectTab = (name: string): void => {
        for (const t of tabs) {
            const on = t.dataset.tab === name;
            t.setAttribute("aria-selected", String(on));
            t.tabIndex = on ? 0 : -1;
            const p = maybeId(t.getAttribute("aria-controls") ?? "");
            if (p) p.hidden = !on;
        }
        sidebarEl.dataset.tab = name;
        sidebarEl.dispatchEvent(new CustomEvent("cf:tab", { detail: name }));
    };
    for (const t of tabs) {
        t.addEventListener("click", () => selectTab(t.dataset.tab ?? ""));
        t.addEventListener("keydown", (e) => {
            const d = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
            if (!d) return;
            const next = tabs[(tabs.indexOf(t) + d + tabs.length) % tabs.length];
            if (next) {
                selectTab(next.dataset.tab ?? "");
                next.focus();
            }
        });
    }
    const sidebarPanel = makePanel(sidebarEl, maybeId("toc-btn"), () =>
        sidebarEl.querySelector("[aria-current]")?.scrollIntoView({ block: "center" }),
    );
    const sidebar: Sidebar = {
        ...sidebarPanel,
        show(tab) {
            selectTab(tab);
            sidebarPanel.open();
        },
    };
    selectTab(tabs[0]?.dataset.tab ?? "contents");

    const settings = makePanel(sheetEl, maybeId("settings-btn"));
    bindSettingControls(sheetEl);

    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") {
            if (!closePanels()) setBars(!barsHidden());
        } else if (e.key === "Tab" && barsHidden()) {
            showBars(); // the controls are in the bars
        }
    });

    // A press outside an open panel (and outside the bars, whose buttons
    // toggle them) closes it, and the click it turns into does nothing else.
    let dismissedAt = 0;
    document.addEventListener(
        "pointerdown",
        (e) => {
            const t = e.target;
            if (!anyOpen() || !(t instanceof Element)) return;
            if (t.closest(".r-bar") || panels.some((p) => p.panel.isOpen() && p.el.contains(t))) return;
            closePanels();
            dismissedAt = e.timeStamp;
        },
        true,
    );
    document.addEventListener(
        "click",
        (e) => {
            if (dismissedAt && e.timeStamp - dismissedAt < 1000) {
                e.stopPropagation();
                e.preventDefault();
            }
            dismissedAt = 0;
        },
        true,
    );

    // ---- position ---------------------------------------------------------

    const label = byId("progress-label");
    const mini = byId("reader-mini");
    const scrubber = byId("scrubber", HTMLInputElement);
    scrubber.max = String(SCRUB_MAX);
    let scrubbing = false;
    let positionText = "";
    scrubber.addEventListener("input", () => {
        scrubbing = true;
        const preview = opts.onScrub(Number(scrubber.value) / SCRUB_MAX, false);
        if (preview) label.textContent = preview;
    });
    scrubber.addEventListener("change", () => {
        scrubbing = false;
        label.textContent = positionText;
        opts.onScrub(Number(scrubber.value) / SCRUB_MAX, true);
    });

    const chapter = byId("chapter-title");
    const status = maybeId("sync-status");
    const toastEl = byId("reader-toast");
    let toastTimer: ReturnType<typeof setTimeout> | undefined;

    return {
        sidebar,
        settings,
        barsHidden,
        showBars,
        hideBars,
        toggleBars: () => (barsHidden() ? showBars() : hideBars()),
        closePanels,
        setChapter(text) {
            chapter.textContent = text;
            chapter.hidden = !text;
        },
        setPosition(text, fraction) {
            positionText = text;
            if (mini.textContent !== text) {
                mini.textContent = text;
                // Restart the PDF reader's fade (web/reader.css).
                mini.classList.remove("flash");
                void mini.offsetWidth;
                mini.classList.add("flash");
            }
            if (scrubbing) return;
            label.textContent = text;
            if (fraction !== undefined) {
                scrubber.value = String(Math.round(fraction * SCRUB_MAX));
                scrubber.setAttribute("aria-valuetext", text);
            }
        },
        setScrubbable(on) {
            scrubber.disabled = !on;
        },
        setStatus(text) {
            if (status) status.textContent = text;
        },
        toast(text) {
            toastEl.textContent = text;
            toastEl.classList.add("show");
            clearTimeout(toastTimer);
            toastTimer = setTimeout(() => toastEl.classList.remove("show"), 2200);
        },
    };
}
