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

/** Only known values: the rest is the default (prefs.ts reads them the same way). */
function account(v: Record<string, unknown> | null | undefined): { format: string } {
    const f = v?.["format"];
    return { format: typeof f === "string" && FORMATS.includes(f) ? f : "last" };
}

/**
 * Signed in (nav.ts, with /api/me's answer). True when the guest's
 * preferences were taken.
 */
export function settleAccountPrefs(user: string, server: { format: string } | undefined): Promise<boolean> {
    window.cfPrefsSettled = settle(user, server);
    return window.cfPrefsSettled;
}

/** Once nav.ts has settled the account's preferences on this page (at once if it hasn't started). */
export function prefsSettled(): Promise<unknown> {
    return window.cfPrefsSettled ?? Promise.resolve();
}

async function settle(user: string, server: { format: string } | undefined): Promise<boolean> {
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
    const prefs = account(guest);
    try {
        const res = await fetch("/api/me/prefs", {
            method: "PUT",
            credentials: "same-origin",
            headers: { "content-type": "application/json" },
            body: JSON.stringify(prefs),
        });
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        write(userAccount, prefs);
    } catch {
        if (server) write(userAccount, account(server)); // not saved: the server's stand
    }
    return true;
}
