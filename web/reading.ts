/**
 * The books the signed-in user is reading, and how far along each is, for
 * the library grid: their cards get a ring and a bar along the bottom.
 *
 * The last answer is kept in localStorage (with whose it is), so on repeat
 * visits cards are marked as they render; the request then corrects it.
 */

/** uuid -> fraction read (0..1), or null when no position is known. */
export type Reading = Map<string, number | null>;

const KEY = "cf-reading";

interface Stored {
    user: string;
    reading: Record<string, number | null>;
}

/** Who nav.ts last saw signed in (its cached /api/me answer), if anyone. */
function lastUser(): string | null | undefined {
    try {
        const me = localStorage.getItem("cf-me");
        return me === null ? undefined : (JSON.parse(me) as { username: string | null }).username;
    } catch {
        return undefined;
    }
}

function store(v: Stored | null): void {
    try {
        if (v) localStorage.setItem(KEY, JSON.stringify(v));
        else localStorage.removeItem(KEY);
    } catch {
        /* storage blocked: cards are marked once the request answers */
    }
}

/** The last known list, if it belongs to whoever is signed in now. */
export function cachedReading(): Reading {
    try {
        const v = JSON.parse(localStorage.getItem(KEY) ?? "null") as Stored | null;
        if (v && v.user === lastUser()) return new Map(Object.entries(v.reading));
    } catch {
        /* blocked or unreadable */
    }
    return new Map();
}

/** The current list; empty when signed out, null when it couldn't be fetched. */
export async function fetchReading(): Promise<Reading | null> {
    try {
        const res = await fetch("/api/me/reading", { credentials: "same-origin" });
        if (res.status === 401) {
            store(null);
            return new Map();
        }
        if (!res.ok) return null;
        const body = (await res.json()) as Stored;
        store(body);
        return new Map(Object.entries(body.reading));
    } catch {
        return null; // offline: keep what's shown
    }
}

/** Mark a card as being read, with how far (null: unknown), or not (undefined). */
export function markCard(card: HTMLElement, progress: number | null | undefined): void {
    card.classList.toggle("is-reading", progress !== undefined);
    let bar = card.querySelector<HTMLElement>(".book-progress");
    if (progress === undefined || progress === null) {
        bar?.remove();
        return;
    }
    if (!bar) {
        bar = document.createElement("div");
        bar.className = "book-progress";
        bar.setAttribute("role", "progressbar");
        bar.setAttribute("aria-valuemin", "0");
        bar.setAttribute("aria-valuemax", "100");
        bar.append(document.createElement("span"));
        card.append(bar);
    }
    const pct = Math.round(progress * 100);
    bar.setAttribute("aria-valuenow", String(pct));
    bar.setAttribute("aria-label", `${pct}% read`);
    bar.title = `${pct}% read`;
    (bar.firstElementChild as HTMLElement).style.width = `${Math.max(0, Math.min(100, progress * 100))}%`;
}
