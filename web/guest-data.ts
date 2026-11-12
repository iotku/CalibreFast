/**
 * Reading done on this device while signed out: the guest's positions,
 * highlights and bookmarks, kept by the readers under the plain localStorage
 * names (local-keys.ts). The top bar (nav.ts) offers to add them to the
 * account after signing in; /account/import (account-import.ts) reviews and
 * adds them (guest_import.go). Records written before guest and account
 * data were kept apart are in the same names; the review shows the ones an
 * account already has as such, and they're cleared.
 */

export interface GuestPosition {
    format: "epub" | "pdf";
    position: string;
    percent: number;
    updated_at: number;
}

export interface GuestBook {
    uuid: string;
    positions: GuestPosition[];
    annotations: Record<string, unknown>[]; // as the readers keep them (annotations.ts)
    bookmarks: Record<string, unknown>[]; // reading-sync.ts's Bookmark
}

const PROGRESS = /^cf-progress-(.+)-(epub|pdf)$/;
const ANNOTATIONS = /^cf-annotations-(.+)$/;
const BOOKMARKS = /^cf-bookmarks-(?!deleted-)(.+)$/;

function read(key: string): unknown {
    try {
        return JSON.parse(localStorage.getItem(key) ?? "null") as unknown;
    } catch {
        return null;
    }
}

function keys(): string[] {
    try {
        return Object.keys(localStorage);
    } catch {
        return [];
    }
}

/** Every book with guest records on this device. */
export function collectGuestData(): GuestBook[] {
    const books = new Map<string, GuestBook>();
    const book = (uuid: string): GuestBook => {
        let b = books.get(uuid);
        if (!b) books.set(uuid, (b = { uuid, positions: [], annotations: [], bookmarks: [] }));
        return b;
    };
    for (const key of keys()) {
        let m = PROGRESS.exec(key);
        if (m) {
            const v = read(key) as { position?: string; percent?: number; updatedAt?: number } | null;
            if (v?.position) {
                book(m[1]!).positions.push({
                    format: m[2] as "epub" | "pdf",
                    position: String(v.position),
                    percent: Number(v.percent) || 0,
                    updated_at: Number(v.updatedAt) || 0,
                });
            }
            continue;
        }
        if ((m = ANNOTATIONS.exec(key))) {
            const list = read(key);
            const live = Array.isArray(list) ? (list as Record<string, unknown>[]).filter((a) => a && !a["deleted"]) : [];
            if (live.length) book(m[1]!).annotations.push(...live.map(({ synced: _s, ...a }) => a));
            continue;
        }
        if ((m = BOOKMARKS.exec(key))) {
            const list = read(key);
            const all = Array.isArray(list) ? (list as Record<string, unknown>[]).filter(Boolean) : [];
            if (all.length) book(m[1]!).bookmarks.push(...all.map(({ synced: _s, ...b }) => b));
        }
    }
    return [...books.values()];
}

/** How many books have guest records (cheap enough for every page). */
export function guestBookCount(): number {
    return collectGuestData().length;
}

/** Remove a book's guest records (added to the account, or discarded). */
export function forgetGuestBooks(uuids: string[]): void {
    if (!uuids.length) return;
    const gone = new Set(uuids);
    try {
        for (const u of gone) {
            for (const k of [
                `cf-progress-${u}-epub`,
                `cf-progress-${u}-pdf`,
                `cf-annotations-${u}`,
                `cf-bookmarks-${u}`,
                `cf-bookmarks-deleted-${u}`,
                `reader-cfi-${u}`, // the readers' older position keys
                `pdf-page-${u}`,
            ]) {
                localStorage.removeItem(k);
            }
        }
        const index = read("cf-local-reading") as Record<string, unknown> | null;
        if (index) {
            for (const u of gone) delete index[u];
            localStorage.setItem("cf-local-reading", JSON.stringify(index));
        }
    } catch {
        /* storage blocked: nothing was kept anyway */
    }
}

// The offer after signing in: declined for good per account ("these aren't
// mine"), or for this visit ("not now").
const DECLINED_KEY = "cf-guest-import-declined";
const LATER_KEY = "cf-guest-import-later";

/** Said "these aren't mine": not offered to this account again, not even in its menu. */
export function offerDeclinedForGood(user: string): boolean {
    try {
        return (JSON.parse(localStorage.getItem(DECLINED_KEY) ?? "[]") as string[]).includes(user);
    } catch {
        return false;
    }
}

/** Declined for good, or put off for this visit. */
export function offerDeclined(user: string): boolean {
    if (offerDeclinedForGood(user)) return true;
    try {
        return sessionStorage.getItem(LATER_KEY) === user;
    } catch {
        return false;
    }
}

export function declineOffer(user: string, forGood: boolean): void {
    try {
        if (forGood) {
            const v = JSON.parse(localStorage.getItem(DECLINED_KEY) ?? "[]") as string[];
            if (!v.includes(user)) localStorage.setItem(DECLINED_KEY, JSON.stringify([...v, user]));
        } else {
            sessionStorage.setItem(LATER_KEY, user);
        }
    } catch {
        /* storage blocked: asked again next time */
    }
}
