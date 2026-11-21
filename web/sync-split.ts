/**
 * The book page's sync controls under the Read/Download buttons: keeping
 * the book on this device (offline-control.ts) and sending it to Hardcover
 * (hardcover-book.ts). Each is a split button like Save's (user.ts): a
 * toggle for the usual choice, with a menu (a <select> showing only its
 * caret) for the rest. Built with el()/icon(): nothing here parses HTML.
 */
import { el, icon, lineIcon, type SvgShape } from "./dom.js";

const phone = (): SvgShape => ["rect", { x: 6, y: 2, width: 12, height: 20, rx: 2 }];

/** The toggles' icons. Each call makes a new one. */
export const SYNC_ICONS = {
    /** Not kept here: a device with an arrow into it. */
    device: () => icon(lineIcon(16), phone(), "M12 7v7", "M9 11l3 3 3-3"),
    /** Kept here: the device, ticked. */
    deviceKept: () => icon(lineIcon(16), phone(), "M9 12l2 2 4-4"),
    /** Never kept here: the device, struck out. */
    deviceNever: () => icon(lineIcon(16), phone(), "M3 3l18 18"),
    /** Hardcover: two arrows going round. */
    sync: () => icon(lineIcon(16), "M21 12a9 9 0 0 1-15.5 6.2L3 16", "M3 21v-5h5", "M3 12a9 9 0 0 1 15.5-6.2L21 8", "M21 3v5h-5"),
    /** Something needs the user's attention. */
    problem: () => icon(lineIcon(16), "M12 3l10 18H2z", "M12 10v4", "M12 17.5v.01"),
};

export interface SyncLook {
    icon: () => SVGSVGElement;
    label: string;
    title: string;
    /** Highlighted: it's on (kept, synced). */
    on: boolean;
    /** A toggle (aria-pressed) and whether it's pressed; undefined: the button does something else (opens the menu). */
    pressed?: boolean;
    problem?: boolean;
    busy?: boolean;
}

/** Paint a split button's toggle. */
export function paintSync(btn: HTMLButtonElement, look: SyncLook): void {
    btn.replaceChildren(look.icon(), el("span", { className: "btn-sync-label", textContent: look.label }));
    btn.title = look.title;
    btn.classList.toggle("is-on", look.on);
    btn.classList.toggle("is-problem", !!look.problem);
    if (look.pressed === undefined) btn.removeAttribute("aria-pressed");
    else btn.setAttribute("aria-pressed", String(look.pressed));
    if (look.busy) btn.setAttribute("aria-busy", "true");
    else btn.removeAttribute("aria-busy");
}

/** Open a split button's menu (its <select>), where the browser can. */
export function openMenu(select: HTMLSelectElement): void {
    try {
        select.showPicker();
    } catch {
        select.focus(); // no showPicker for selects (older Safari)
    }
}
