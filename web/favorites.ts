/**
 * The user's favorite authors, series, tags, publishers and languages
 * (aggregate_favorites.go), kept in localStorage under the user's prefix so
 * pages (which are the same for everyone) can show them at once. The list
 * is asked for again only when /api/me's `favs` differs from the one kept.
 * Changes are told to the page as `cf:favorites`.
 */
import { api } from "./dom.js";
import { lastKnownUser, localKey } from "./local-keys.js";
import { meRequest } from "./me.js";
import type { AggFavorite } from "./types.js";

interface Kept {
    favs: number;
    list: AggFavorite[];
    /** The user's Hardcover link can unfollow, and the unstar dialog's box starts ticked. */
    hcUnfollow?: boolean;
    hcUnfollowDefault?: boolean;
}

/** The unstar dialog's Hardcover choice, for the user last seen (null: not offered). */
export function hardcoverUnfollow(): { can: boolean; ticked: boolean } {
    const owner = lastKnownUser();
    const k = owner === null ? null : read(owner);
    return { can: !!k?.hcUnfollow, ticked: k?.hcUnfollowDefault ?? true };
}

const NAME = "favorites";

function read(owner: string): Kept | null {
    try {
        const v = JSON.parse(localStorage.getItem(localKey(owner, NAME)) ?? "null") as Kept | null;
        return v && Array.isArray(v.list) ? v : null;
    } catch {
        return null;
    }
}

function write(owner: string, kept: Kept): void {
    try {
        localStorage.setItem(localKey(owner, NAME), JSON.stringify(kept));
    } catch {
        /* storage blocked: asked for again next time */
    }
}

/** The list kept for whoever was last signed in, for painting before /api/me answers; null: none. */
export function keptFavorites(): AggFavorite[] | null {
    const owner = lastKnownUser();
    return owner === null ? null : (read(owner)?.list ?? null);
}

let owner: string | null = null;

/** The signed-in user's favorites, from the kept list when it's current; null: signed out. */
export async function loadFavorites(): Promise<AggFavorite[] | null> {
    const me = await meRequest();
    const user = me.ok ? me.body.user : null;
    if (!user) return null;
    owner = user.username;
    const kept = read(owner);
    if (kept && kept.favs === (me.body.favs ?? 0)) return kept.list;
    return refresh(owner);
}

async function refresh(who: string): Promise<AggFavorite[]> {
    const got = await api<{ favs: number; favorites: AggFavorite[]; hc_unfollow?: boolean; hc_unfollow_default?: boolean }>("GET", "/api/me/favorites");
    write(who, { favs: got.favs, list: got.favorites, hcUnfollow: !!got.hc_unfollow, hcUnfollowDefault: got.hc_unfollow_default ?? true });
    document.dispatchEvent(new CustomEvent("cf:favorites"));
    return got.favorites;
}

/**
 * Favorite (or not) a value; resolves to the new list. unfollow: taking an
 * author off also unfollows them on Hardcover (the user ticked it).
 */
export async function setFavorite(kind: string, name: string, on: boolean, unfollow = false): Promise<AggFavorite[]> {
    const url = `/api/me/favorites/${encodeURIComponent(kind)}/${encodeURIComponent(name)}`;
    await api(on ? "PUT" : "DELETE", !on && unfollow ? `${url}?hardcover=1` : url);
    return refresh(owner ?? lastKnownUser() ?? "");
}

/** Whether list has kind's value name (as browsed; case doesn't matter). */
export function isFavorite(list: AggFavorite[], kind: string, name: string): boolean {
    const n = name.toLowerCase();
    return list.some((f) => f.kind === kind && f.name.toLowerCase() === n);
}
