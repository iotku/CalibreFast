/**
 * System alerts: a strip at the top of the page, above the top bar, on every
 * page with the bar (nav.ts). Nothing covers it and it covers nothing, and it
 * scrolls away with the page: once it's off screen the page is as it would
 * be without it, so it takes no room while someone gets on with things.
 *
 * The strip (#system-alerts) is absolute at the top of the page. Two
 * variables on <html> (style.css):
 *   - --system-alerts-height, its height: html's padding, so the page's
 *     content starts under it;
 *   - --system-alerts-shown, how much of it is still on screen (0 once
 *     scrolled away): the fixed bar, and the fixed panels and menus under
 *     it, sit that much lower.
 *
 *   - Critical alerts (e.g. no lasting way to sign in) come first, can't be
 *     dismissed and never expire: they stay until the caller hides them, once
 *     the problem is resolved. More than one stack.
 *   - Other alerts have an × that dismisses them on this device for good
 *     (localStorage, by id). One may expire (expiresAt), when it goes
 *     whether dismissed or not.
 *
 * Every active alert, dismissed or not, is also pinned at the top of the
 * notifications menu (activeSystemAlerts, nav.ts) while it lasts.
 *
 * Built with el() and textContent only: an alert's text may come from data.
 */
import { el, icon, lineIcon, safeUrl } from "./dom.js";

export interface SystemAlert {
    /** Names the alert: showing the same id again replaces it, and a dismissal is kept under it. */
    id: string;
    /** Can't be dismissed and never expires: shown until hideSystemAlert, when the problem is resolved. */
    critical?: boolean;
    text: string;
    /** A link after the text: a path on this site or an https URL (safeUrl). */
    action?: { href: string; label: string };
    /** When a non-critical alert stops (ms since the epoch); none: until hidden. */
    expiresAt?: number;
}

/** Fired on document whenever the set of active alerts changes. */
export const SYSTEM_ALERTS_EVENT = "cf:system-alerts";

const DISMISSED_KEY = "cf-system-alerts-dismissed";
const CROSS = "M6 6l12 12M18 6L6 18";
/** setTimeout's longest delay; an alert expiring later is checked on a later page. */
const MAX_TIMER_MS = 2 ** 31 - 1;

const active = new Map<string, SystemAlert>();
const timers = new Map<string, ReturnType<typeof setTimeout>>();
let box: HTMLElement | null = null;

/** id → when that alert expires (0: never): dismissals still in force. */
function readDismissed(): Record<string, number> {
    try {
        const v: unknown = JSON.parse(localStorage.getItem(DISMISSED_KEY) ?? "{}");
        return v && typeof v === "object" ? (v as Record<string, number>) : {};
    } catch {
        return {};
    }
}

function writeDismissed(d: Record<string, number>): void {
    try {
        if (Object.keys(d).length) localStorage.setItem(DISMISSED_KEY, JSON.stringify(d));
        else localStorage.removeItem(DISMISSED_KEY);
    } catch {
        // Storage full or blocked: the dismissal lasts for this page only.
    }
}

/** Drops dismissals of alerts that have expired, so the store doesn't grow. */
function pruneDismissed(): void {
    const d = readDismissed();
    const now = Date.now();
    let changed = false;
    for (const [id, until] of Object.entries(d)) {
        if (typeof until !== "number" || (until && until <= now)) {
            delete d[id];
            changed = true;
        }
    }
    if (changed) writeDismissed(d);
}

function isDismissed(id: string): boolean {
    return Object.hasOwn(readDismissed(), id);
}

function expired(a: SystemAlert): boolean {
    return !a.critical && a.expiresAt !== undefined && a.expiresAt <= Date.now();
}

let height = 0;
let shown = -1;

/** How much of the strip is on screen, for the fixed bar. Set only when it changes: once scrolled away, scrolling costs nothing. */
function follow(): void {
    const s = Math.max(0, Math.round(height - window.scrollY));
    if (s === shown) return;
    shown = s;
    document.documentElement.style.setProperty("--system-alerts-shown", `${s}px`);
}

/** Puts the strip's height where the CSS reads it. */
function measure(): void {
    const h = box?.offsetHeight ?? 0;
    if (h !== height) {
        height = h;
        document.documentElement.style.setProperty("--system-alerts-height", `${h}px`);
    }
    follow();
}

function container(): HTMLElement {
    if (box) return box;
    box = el("div", { id: "system-alerts", className: "system-alerts" });
    document.body.prepend(box);
    // Text wrapping differently (a narrower window, fonts loading) changes the height too.
    new ResizeObserver(measure).observe(box);
    window.addEventListener("scroll", follow, { passive: true });
    return box;
}

function changed(): void {
    measure();
    document.dispatchEvent(new CustomEvent(SYSTEM_ALERTS_EVENT));
}

/** The alert's text and link, shared by the strip and the notifications menu. */
export function systemAlertParts(a: SystemAlert, linkClass = "btn-standard btn-primary"): (HTMLElement | string)[] {
    const parts: HTMLElement[] = [el("span", { className: "system-alert-text", textContent: a.text })];
    if (a.action) parts.push(el("a", { className: linkClass, href: safeUrl(a.action.href), textContent: a.action.label }));
    return parts;
}

function render(a: SystemAlert): HTMLElement {
    const item = el(
        "div",
        { className: a.critical ? "system-alert is-critical" : "system-alert", role: a.critical ? "alert" : "status" },
        ...systemAlertParts(a),
    );
    item.dataset["alertId"] = a.id;
    if (!a.critical) {
        const dismiss = el("button", { type: "button", className: "system-alert-dismiss", title: "Dismiss", ariaLabel: `Dismiss: ${a.text}` });
        dismiss.append(icon(lineIcon(16), CROSS));
        dismiss.addEventListener("click", () => dismissSystemAlert(a.id));
        item.append(dismiss);
    }
    return item;
}

function removeItem(id: string): void {
    for (const item of box?.querySelectorAll<HTMLElement>(".system-alert") ?? []) {
        if (item.dataset["alertId"] === id) item.remove();
    }
}

/**
 * Shows an alert, or replaces the one with the same id. A dismissed one is
 * kept (for the menu) but not shown; an expired one is dropped.
 */
export function showSystemAlert(a: SystemAlert): void {
    clearTimeout(timers.get(a.id));
    timers.delete(a.id);
    removeItem(a.id);
    active.delete(a.id);
    if (expired(a)) {
        changed();
        return;
    }
    active.set(a.id, a);
    if (a.critical || !isDismissed(a.id)) {
        const strip = container();
        const item = render(a);
        // Critical ones first, in the order shown; the rest after them.
        const firstOther = strip.querySelector(".system-alert:not(.is-critical)");
        if (a.critical && firstOther) firstOther.before(item);
        else strip.append(item);
    }
    if (!a.critical && a.expiresAt !== undefined) {
        const wait = a.expiresAt - Date.now();
        if (wait < MAX_TIMER_MS) timers.set(a.id, setTimeout(() => hideSystemAlert(a.id), wait));
    }
    changed();
}

/** Takes an alert away: resolved, expired or withdrawn. Its dismissal is forgotten too. */
export function hideSystemAlert(id: string): void {
    clearTimeout(timers.get(id));
    timers.delete(id);
    const was = active.delete(id);
    removeItem(id);
    const d = readDismissed();
    if (Object.hasOwn(d, id)) {
        delete d[id];
        writeDismissed(d);
    }
    if (was) changed();
}

/** The × on a non-critical alert: hidden here for good, still listed in the menu while active. */
export function dismissSystemAlert(id: string): void {
    const a = active.get(id);
    if (!a || a.critical) return;
    const d = readDismissed();
    d[id] = a.expiresAt ?? 0;
    writeDismissed(d);
    removeItem(id);
    changed();
}

/** The alerts in force, dismissed or not: critical ones first. */
export function activeSystemAlerts(): SystemAlert[] {
    const all = [...active.values()].filter((a) => !expired(a));
    return [...all.filter((a) => a.critical), ...all.filter((a) => !a.critical)];
}

pruneDismissed();
