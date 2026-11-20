/**
 * Settling the account's preferences (prefs.ts) as a page loads: nav.ts
 * runs this on every signed-in page, so it's kept apart from prefs.ts and
 * small (no imports but local-keys.ts).
 *
 *   - Signing in to an existing account: the account's own preferences, as
 *     /api/me sends them, are kept here (they follow it to every device).
 *   - An account just made here (the server's one-shot cf-new-account
 *     cookie, prefs.go): the preferences the guest had become the account's,
 *     both this device's and the synced ones.
 */

import { localKey } from "./local-keys.js";

const NEW_ACCOUNT_COOKIE = "cf-new-account";
const FORMATS = ["last", "epub", "pdf"];

declare global {
    interface Window {
        /** settleAccountPrefs's work, for the page's own script (another bundle) to wait for. */
        cfPrefsSettled?: Promise<boolean>;
    }
}

function read(key: string): Record<string, unknown> | null {
    try {
        return JSON.parse(localStorage.getItem(key) ?? "null") as Record<string, unknown> | null;
    } catch {
        return null;
    }
}

function write(key: string, v: unknown): void {
    try {
        localStorage.setItem(key, JSON.stringify(v));
    } catch (err) {
        console.warn(`prefs: couldn't save ${key} in localStorage (storage full, or blocked by the browser); the defaults stand:`, err);
    }
}

/** The account's preferences that a guest has too (format); the others are the server's alone. */
interface AccountFields {
    format: string;
    track_reading?: boolean;
    daily_pages?: number;
    yearly_books?: number;
    goal_notice?: boolean;
}

const goal = (n: unknown): number => (typeof n === "number" && Number.isInteger(n) && n >= 0 && n <= 1000 ? n : 0);

/** Only known values: the rest is the default (prefs.ts reads them the same way). */
function account(given: object | null | undefined): AccountFields {
    const v = given as Record<string, unknown> | null | undefined;
    const f = v?.["format"];
    const track = v?.["track_reading"];
    return {
        format: typeof f === "string" && FORMATS.includes(f) ? f : "last",
        track_reading: typeof track === "boolean" ? track : true,
        daily_pages: goal(v?.["daily_pages"]),
        yearly_books: goal(v?.["yearly_books"]),
        goal_notice: v?.["goal_notice"] === true,
    };
}

/**
 * Signed in (nav.ts, with /api/me's answer). True when the guest's
 * preferences were taken.
 */
export function settleAccountPrefs(user: string, server: AccountFields | undefined): Promise<boolean> {
    window.cfPrefsSettled = settle(user, server);
    return window.cfPrefsSettled;
}

/** Once nav.ts has settled the account's preferences on this page (at once if it hasn't started). */
export function prefsSettled(): Promise<unknown> {
    return window.cfPrefsSettled ?? Promise.resolve();
}

async function settle(user: string, server: AccountFields | undefined): Promise<boolean> {
    const userAccount = localKey(user, "account-prefs");
    const isNew = document.cookie.split(";").some((c) => c.trim() === `${NEW_ACCOUNT_COOKIE}=1`);
    if (!isNew) {
        if (server) write(userAccount, account(server));
        return false;
    }
    document.cookie = `${NEW_ACCOUNT_COOKIE}=; Path=/; Max-Age=0; SameSite=Lax`; // once, whatever happens next
    const guestDevice = read(localKey(null, "prefs"));
    if (guestDevice) write(localKey(user, "prefs"), { ...guestDevice, ...read(localKey(user, "prefs")) });
    const guest = read(localKey(null, "account-prefs"));
    if (!guest) {
        if (server) write(userAccount, account(server));
        return true;
    }
    const prefs = { format: account(guest).format };
    try {
        const res = await fetch("/api/me/prefs", {
            method: "PUT",
            credentials: "same-origin",
            headers: { "content-type": "application/json" },
            body: JSON.stringify(prefs),
        });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        write(userAccount, { ...account(server), ...prefs });
    } catch {
        if (server) write(userAccount, account(server)); // not saved: the server's stand
    }
    return true;
}
