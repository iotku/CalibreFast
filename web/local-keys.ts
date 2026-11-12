/**
 * Whose reading data a localStorage key holds.
 *
 * The readers keep each book's position, bookmarks and highlights in
 * localStorage. Records written while signed out (guest data) use the plain
 * names (`cf-progress-<uuid>-<format>`, …), as they always have. Records
 * written while signed in live under the user's own prefix
 * (`cf-u/<username>/progress-<uuid>-<format>`, …), so:
 *   - guest highlights and bookmarks are never sent into whichever account
 *     next opens the book on this device (they stay where they are, for the
 *     visitor to add to an account deliberately);
 *   - one user's unsent changes never reach another user's account;
 *   - signing out can drop what the server already has (forgetSynced).
 *
 * Records written before this split are in the plain names and so count as
 * guest data: nothing is deleted, they're just not sent automatically.
 */

/** null: signed out (guest data). */
export type Owner = string | null;

const USER_PREFIX = "cf-u/";

function userPrefix(owner: string): string {
    return `${USER_PREFIX}${encodeURIComponent(owner)}/`;
}

/** The localStorage key for `name` (e.g. `progress-<uuid>-epub`) of owner. */
export function localKey(owner: Owner, name: string): string {
    return owner === null ? `cf-${name}` : userPrefix(owner) + name;
}

/** Who the top bar (nav.ts) last saw signed in; null: nobody, or unknown. */
export function lastKnownUser(): Owner {
    try {
        const me = localStorage.getItem("cf-me");
        return me ? ((JSON.parse(me) as { username?: string | null }).username ?? null) : null;
    } catch {
        return null;
    }
}

function read(key: string): unknown {
    try {
        const v = localStorage.getItem(key);
        return v === null ? null : (JSON.parse(v) as unknown);
    } catch {
        return null;
    }
}

/**
 * On sign-out: drop the user's records the server already has, and keep only
 * the changes it doesn't (still under the user's prefix, so they're sent when
 * that user signs in again and never become guest data).
 */
export function forgetSynced(owner: string): void {
    const prefix = userPrefix(owner);
    let keys: string[];
    try {
        keys = Object.keys(localStorage).filter((k) => k.startsWith(prefix));
    } catch {
        return; // storage blocked: nothing kept anyway
    }
    for (const key of keys) {
        const name = key.slice(prefix.length);
        const v = read(key);
        let keep: unknown = null;
        if (name.startsWith("progress-")) {
            // {position, percent, updatedAt, pending?}
            if (v && typeof v === "object" && (v as { pending?: boolean }).pending) keep = v;
        } else if (name.startsWith("bookmarks-deleted-")) {
            // Deletions not yet confirmed by the server.
            if (Array.isArray(v) && v.length) keep = v;
        } else if (name.startsWith("bookmarks-") || name.startsWith("annotations-")) {
            // Lists of {…, synced?}; an annotation tombstone counts as a change.
            const unsent = Array.isArray(v) ? v.filter((x: { synced?: boolean }) => !x?.synced) : [];
            if (unsent.length) keep = unsent;
        }
        try {
            if (keep === null) localStorage.removeItem(key);
            else localStorage.setItem(key, JSON.stringify(keep));
        } catch {
            /* storage blocked */
        }
    }
}
