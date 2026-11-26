/**
 * Preferences (/preferences, preferences.ts).
 *
 * Two kinds:
 *   - the account's, which follow it to every device: kept on the server
 *     (prefs.go, PUT /api/me/prefs), sent with /api/me, and copied here by
 *     nav.ts so they're known at once and offline. A guest's are only here.
 *   - this device's (how long books stay in its storage, how much they may
 *     use): only here, per user (local-keys.ts), since each device has its
 *     own storage.
 *
 * A guest who makes an account keeps both (account-prefs.ts, which nav.ts
 * runs on every page, so it's kept apart and small). Signing in to an
 * existing account doesn't: its own preferences stand.
 */

import { api } from "./dom.js";
import { lastKnownUser, localKey, type Owner } from "./local-keys.js";

const MB = 1 << 20;

/** This device's preferences. */
export interface DevicePrefs {
    /** Days a Reading book stays unopened; 0: for good; -1: books aren't kept automatically. */
    readingKeepDays: number;
    /** The most recently read N Reading books stay however long they go unopened. */
    keepReadingN: number;
    /** The largest file kept automatically (bytes); 0: no limit. A pinned book is kept anyway. */
    maxBookBytes: number;
    /** The first N On deck then Saved books (oldest saved, unless arranged) are downloaded ahead of time (signed in); 0: none. */
    prefetchSavedN: number;
    /** The most all kept books may use (bytes); 0: no limit; AUTO_LIMIT: half of what the browser allows (retention.ts). */
    maxBytes: number;
}

/** The choices for each, as the page offers them (anything else read back is dropped). */
export const DEVICE_CHOICES: { [K in keyof DevicePrefs]: readonly number[] } = {
    readingKeepDays: [1, 7, 30, 0, -1],
    keepReadingN: [0, 1, 3, 5, 10],
    maxBookBytes: [25 * MB, 50 * MB, 100 * MB, 250 * MB, 500 * MB, 0],
    prefetchSavedN: [0, 3, 5, 10, 20, 50],
    maxBytes: [-1, 250 * MB, 500 * MB, 1024 * MB, 2048 * MB, 5120 * MB, 0],
};

export const DEVICE_DEFAULTS: DevicePrefs = {
    readingKeepDays: 7,
    // Someone back after a long time away, with no connection, still has something to read.
    keepReadingN: 5,
    maxBookBytes: 100 * MB,
    // The first books of Discover's "On deck / Saved for later" row are ready offline.
    prefetchSavedN: 3,
    maxBytes: -1,
};

/** maxBytes: half of what the browser lets this site store (the default). */
export const AUTO_LIMIT = -1;
/** The automatic limit when the browser doesn't say how much it allows. */
export const FALLBACK_LIMIT = 1024 * MB;

/** Finished books stay this long unopened (not a preference): to go back to straight away, not to keep. */
export const FINISHED_KEEP_DAYS = 1;

function readJson<T>(key: string): T | null {
    try {
        return JSON.parse(localStorage.getItem(key) ?? "null") as T | null;
    } catch {
        return null;
    }
}

function writeJson(key: string, v: unknown): void {
    try {
        localStorage.setItem(key, JSON.stringify(v));
    } catch (err) {
        console.warn(`prefs: couldn't save ${key} in localStorage (storage full, or blocked by the browser); the defaults stand:`, err);
    }
}

/** What was saved for owner, only the valid parts. */
function savedDevicePrefs(owner: Owner): Partial<DevicePrefs> {
    const raw = readJson<Record<string, unknown>>(localKey(owner, "prefs")) ?? {};
    const out: Partial<DevicePrefs> = {};
    for (const k of Object.keys(DEVICE_CHOICES) as (keyof DevicePrefs)[]) {
        const v = raw[k];
        if (typeof v === "number" && DEVICE_CHOICES[k].includes(v)) out[k] = v;
    }
    return out;
}

/** This device's preferences for whoever is signed in (or the guest). */
export function devicePrefs(owner: Owner = lastKnownUser()): DevicePrefs {
    return { ...DEVICE_DEFAULTS, ...savedDevicePrefs(owner) };
}

export function setDevicePref<K extends keyof DevicePrefs>(key: K, value: DevicePrefs[K], owner: Owner = lastKnownUser()): void {
    if (!DEVICE_CHOICES[key].includes(value)) return;
    writeJson(localKey(owner, "prefs"), { ...savedDevicePrefs(owner), [key]: value });
}

/* ------------------------------------------------------------------ */
/* The account's (synced)                                             */
/* ------------------------------------------------------------------ */

/** The format a book with both an EPUB and a PDF opens in: the one last read, or always one. */
export type FormatPref = "last" | "epub" | "pdf";
export const FORMAT_PREFS: readonly FormatPref[] = ["last", "epub", "pdf"];

/** The account's preferences, as /api/me sends them (Go: userPrefs). */
export interface AccountPrefs {
    format: FormatPref;
    /** Record pages and time read (stats.go, activity.ts). */
    track_reading: boolean;
    /** Reading goals; 0: none. */
    daily_pages: number;
    yearly_books: number;
    /** A notice in the reader when the daily goal is reached; off unless asked for. */
    goal_notice: boolean;
    /** "No thanks" to system notifications: not offered again (push.ts). */
    push_declined: boolean;
    /** Don't show books not in the library yet anywhere: series, author and publisher pages, search, On Deck, shelves, Saved (Go: hidesPlaceholders). */
    hide_placeholders: boolean;
}

const ACCOUNT_DEFAULTS: AccountPrefs = {
    format: "last", track_reading: true, daily_pages: 0, yearly_books: 0, goal_notice: false, push_declined: false, hide_placeholders: false,
};

const goal = (n: unknown): number => (typeof n === "number" && Number.isInteger(n) && n >= 0 && n <= 1000 ? n : 0);

function validAccount(v: Partial<AccountPrefs> | null): AccountPrefs {
    return {
        format: v?.format && FORMAT_PREFS.includes(v.format) ? v.format : ACCOUNT_DEFAULTS.format,
        track_reading: typeof v?.track_reading === "boolean" ? v.track_reading : ACCOUNT_DEFAULTS.track_reading,
        daily_pages: goal(v?.daily_pages),
        yearly_books: goal(v?.yearly_books),
        goal_notice: v?.goal_notice === true,
        push_declined: v?.push_declined === true,
        hide_placeholders: v?.hide_placeholders === true,
    };
}

/** The account's preferences as last known here (a guest's: only here). */
export function accountPrefs(owner: Owner = lastKnownUser()): AccountPrefs {
    return validAccount(readJson<Partial<AccountPrefs>>(localKey(owner, "account-prefs")));
}

export function formatPref(owner: Owner = lastKnownUser()): FormatPref {
    return accountPrefs(owner).format;
}

/** Change the account's preferences: on the server when signed in, then here. */
export async function setAccountPrefs(change: Partial<AccountPrefs>, owner: Owner = lastKnownUser()): Promise<void> {
    const next = validAccount({ ...accountPrefs(owner), ...change });
    if (owner !== null) await api("PUT", "/api/me/prefs", next);
    writeJson(localKey(owner, "account-prefs"), next);
}
