/**
 * The user's favorite authors, series, tags, publishers and languages
 * (aggregate_favorites.go), kept in localStorage under the user's prefix so
 * pages (which are the same for everyone) can show them at once. The list
 * is asked for again only when /api/me's `favs` differs from the one kept
 * (or, on an aggregate page, the page's generation does: counts, pictures).
 * Changes are told to the page as `cf:favorites`.
 */
import { api } from "./dom.js";
import { lastKnownUser, localKey } from "./local-keys.js";
import { meRequest } from "./me.js";
import type { AggFavorite } from "./types.js";

/*
 * The kept JSON is also read by templates/aggregate.html's inline script,
 * which draws /authors' carousel from it while the page parses: keep the
 * key and the list's shape (kind, name, label, href, count, image_url,
 * initial) in step with it.
 */
interface Kept {
    favs: number;
    /** The aggregate pages' generation when the list was got (counts and pictures as then). */
    gen?: number;
    list: AggFavorite[];
    /** The user's Hardcover link can unfollow, and the unstar dialog's box starts ticked. */
    hcUnfollow?: boolean;
    hcUnfollowDefault?: boolean;
    /** Authors starred here are followed on Hardcover (perhaps not yet: it's sent in the background). */
    hcFollowStars?: boolean;
}

/** The unstar dialog's Hardcover choice, for the user last seen (null: not offered). */
export function hardcoverUnfollow(): { can: boolean; ticked: boolean; followStars: boolean } {
    const owner = lastKnownUser();
    const k = owner === null ? null : read(owner);
    return { can: !!k?.hcUnfollow, ticked: k?.hcUnfollowDefault ?? true, followStars: !!k?.hcFollowStars };
}

/**
 * The list as the server has it now, kept. Before taking a star off an
 * author: a follow sent to Hardcover since the list was kept (in the
 * background, after starring) isn't in the kept one yet.
 */
export async function freshFavorites(): Promise<AggFavorite[]> {
    return refresh(owner ?? lastKnownUser() ?? "");
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

/**
 * The signed-in user's favorites, from the kept list when it's current; null:
 * signed out. gen (an aggregate page's, from its data-src): the kept list
 * is current only if got at that generation too, so its counts and
 * pictures are.
 */
export async function loadFavorites(gen?: number): Promise<AggFavorite[] | null> {
    const me = await meRequest();
    const user = me.ok ? me.body.user : null;
    if (!user) return null;
    owner = user.username;
    const kept = read(owner);
    if (kept && kept.favs === (me.body.favs ?? 0) && (gen === undefined || kept.gen === gen)) return kept.list;
    return refresh(owner);
}

async function refresh(who: string): Promise<AggFavorite[]> {
    const got = await api<{
        favs: number;
        gen?: number;
        favorites: AggFavorite[];
        hc_unfollow?: boolean;
        hc_unfollow_default?: boolean;
        hc_follow_stars?: boolean;
    }>("GET", "/api/me/favorites");
    write(who, {
        favs: got.favs,
        gen: got.gen ?? 0,
        list: got.favorites,
        hcUnfollow: !!got.hc_unfollow,
        hcUnfollowDefault: got.hc_unfollow_default ?? true,
        hcFollowStars: !!got.hc_follow_stars,
    });
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
