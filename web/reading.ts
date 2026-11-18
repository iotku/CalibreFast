/**
 * The signed-in user's marks on the library grid's cards: a ring colored by
 * reading status (dashed for saved; solid for reading and finished,
 * with a bar along the bottom showing how far along, full once finished),
 * and every card gets two buttons over its cover on hover:
 *   - top left: favorite (☆/★; a ★ stays shown without hover);
 *   - top right, by reading status: "save" for a book not started
 *     (a toggle: /me/saved), "continue reading" for one being read (opens
 *     the reader in the format last read), a check mark once finished.
 *
 * The last answer is kept in localStorage (with whose it is), so on repeat
 * visits cards are marked as they render; the request then corrects it.
 * Changes made here (cover buttons, the book popup's controls) are applied
 * to the marks in place rather than asking the server again; the popup's
 * controls, loaded fresh whenever it opens, are checked against them
 * (agrees), and a disagreement fetches them anew.
 */

import { api, el, icon, safeUrl } from "./dom.js";
import { meRequest } from "./me.js";

export interface Marks {
    /** Whose marks these are; null when signed out. */
    user: string | null;
    signedIn: boolean;
    /** uuid -> fraction read (0..1), or null when no position is known. */
    reading: Map<string, number | null>;
    /** uuid -> reader URL, for books being read. */
    resume: Map<string, string>;
    wanted: Set<string>;
    finished: Set<string>;
    favorites: Set<string>;
}

const KEY = "cf-reading";
/** The tag of the marks kept under KEY (listtag.go): sent back, the server answers only if they changed. */
export const TAG_KEY = "cf-reading-tag";

/** /api/me/reading's answer, as stored. */
export interface StoredMarks {
    user: string;
    reading: Record<string, number | null>;
    resume?: Record<string, string>;
    wanted?: string[];
    finished?: string[];
    favorites?: string[];
}

function toMarks(v: StoredMarks | null): Marks {
    return {
        user: v?.user ?? null,
        signedIn: v !== null,
        reading: new Map(Object.entries(v?.reading ?? {})),
        resume: new Map(Object.entries(v?.resume ?? {})),
        wanted: new Set(v?.wanted),
        finished: new Set(v?.finished),
        favorites: new Set(v?.favorites),
    };
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

function store(v: StoredMarks | null, tag: string | null = null): void {
    try {
        if (v) localStorage.setItem(KEY, JSON.stringify(v));
        else localStorage.removeItem(KEY);
        // A tag only with the answer it came with: marks changed here have none.
        if (tag && v) localStorage.setItem(TAG_KEY, tag);
        else localStorage.removeItem(TAG_KEY);
    } catch {
        /* storage blocked: cards are marked once the request answers */
    }
}

/** The tag of the marks kept here, if they're whole as the server sent them. */
function storedTag(): string | null {
    try {
        return localStorage.getItem(TAG_KEY);
    } catch {
        return null;
    }
}

/** The marks kept here, as stored (whoever they belong to). */
function storedMarks(): StoredMarks | null {
    try {
        return JSON.parse(localStorage.getItem(KEY) ?? "null") as StoredMarks | null;
    } catch {
        return null;
    }
}

/** The last known marks, if they belong to whoever is signed in now. */
export function cachedMarks(): Marks {
    try {
        const v = JSON.parse(localStorage.getItem(KEY) ?? "null") as StoredMarks | null;
        if (v && v.user === lastUser()) return toMarks(v);
    } catch {
        /* blocked or unreadable */
    }
    return toMarks(null);
}

/** Marks from an answer that carries them (/api/me/home), kept as the cache. */
export function marksFromAnswer(v: StoredMarks | null): Marks {
    store(v);
    return toMarks(v);
}

function toStored(m: Marks): StoredMarks | null {
    if (m.user === null) return null;
    return {
        user: m.user,
        reading: Object.fromEntries(m.reading),
        resume: Object.fromEntries(m.resume),
        wanted: [...m.wanted],
        finished: [...m.finished],
        favorites: [...m.favorites],
    };
}

/** Keep marks changed in place as the cache; returns them as stored (null for a guest's). */
export function saveMarks(m: Marks): StoredMarks | null {
    const v = toStored(m);
    if (v) store(v);
    return v;
}

/** A book's reading status as the server has it: "", "wanted", "reading" or "finished". */
export function statusOf(m: Marks, uuid: string): string {
    return m.finished.has(uuid) ? "finished" : m.reading.has(uuid) ? "reading" : m.wanted.has(uuid) ? "wanted" : "";
}

/** A change to one book, made and saved elsewhere (the book popup's controls). */
export interface BookChange {
    uuid: string;
    favorite?: boolean;
    status?: string;
}

/**
 * Apply a saved change to marks, in place. False when marks can't show it
 * without the server: a book newly marked "reading" (where to resume it,
 * how far along).
 */
export function applyChange(m: Marks, c: BookChange): boolean {
    if (c.favorite !== undefined) {
        if (c.favorite) m.favorites.add(c.uuid);
        else m.favorites.delete(c.uuid);
    }
    if (c.status === undefined || c.status === statusOf(m, c.uuid)) return true;
    if (c.status === "reading") return false;
    m.reading.delete(c.uuid);
    m.resume.delete(c.uuid);
    m.wanted.delete(c.uuid);
    m.finished.delete(c.uuid);
    if (c.status === "wanted") m.wanted.add(c.uuid);
    else if (c.status === "finished") m.finished.add(c.uuid);
    return true;
}

/** What the book popup's controls show for a book, just loaded from the server. */
export interface BookState {
    uuid: string;
    signedIn: boolean;
    favorite: boolean;
    status: string;
}

/** Whether marks agree with what the server just said about a book. */
export function agrees(m: Marks, s: BookState): boolean {
    if (s.signedIn !== m.signedIn) return false;
    return !s.signedIn || (s.favorite === m.favorites.has(s.uuid) && s.status === statusOf(m, s.uuid));
}

/**
 * The current marks; none when signed out, null when they couldn't be fetched.
 * first: the page's first look, which takes them from its /api/me answer
 * (me.ts asks for them on the grids) rather than asking separately.
 */
export async function fetchMarks(first = false): Promise<Marks | null> {
    if (first) {
        try {
            const me = await meRequest();
            if (me.status === 401) {
                store(null);
                return toMarks(null);
            }
            if (me.ok && me.body.reading) {
                store(me.body.reading, me.body.reading_tag ?? null);
                return toMarks(me.body.reading);
            }
            // Unchanged since the copy kept here (me.ts sent its tag).
            const kept = storedMarks();
            if (me.ok && me.body.reading_same && kept) return toMarks(kept);
        } catch {
            /* offline, or no marks in the answer: ask /api/me/reading */
        }
    }
    try {
        const kept = storedMarks();
        const tag = kept ? storedTag() : null;
        const res = await fetch("/api/me/reading", { credentials: "same-origin", headers: tag ? { "if-none-match": tag } : {} });
        if (res.status === 401) {
            store(null);
            return toMarks(null);
        }
        if (res.status === 304 && kept) return toMarks(kept); // the copy kept here is current
        if (!res.ok) return null;
        const body = (await res.json()) as StoredMarks;
        store(body, res.headers.get("etag"));
        return toMarks(body);
    } catch {
        return null; // offline: keep what's shown
    }
}

/** Mark a card: status ring and bar, and the buttons over its cover. */
export function markCard(card: HTMLElement, marks: Marks): void {
    const uuid = card.dataset["uuid"];
    if (!uuid) return;
    // Same precedence as the right button (rightState).
    const finished = marks.finished.has(uuid);
    const reading = !finished && marks.reading.has(uuid);
    const wanted = !finished && !reading && marks.wanted.has(uuid);
    card.classList.toggle("is-finished", finished);
    card.classList.toggle("is-reading", reading);
    card.classList.toggle("is-wanted", wanted);
    paintProgress(card, finished ? 1 : reading ? (marks.reading.get(uuid) ?? null) : undefined, finished);
    paintActions(card, uuid, marks);
}

function paintProgress(card: HTMLElement, progress: number | null | undefined, finished = false): void {
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
    const label = finished ? "Finished" : `${pct}% read`;
    bar.setAttribute("aria-label", label);
    bar.title = label;
    (bar.firstElementChild as HTMLElement).style.width = `${Math.max(0, Math.min(100, progress * 100))}%`;
}

const BOOKMARK_PATH = "M6 3h12a1 1 0 0 1 1 1v17l-7-4.5L5 21V4a1 1 0 0 1 1-1z";
/** The cover buttons' icons (also the book popup's Save button, user.ts). Each call makes a new one. */
export const ICONS = {
    want: () => icon({}, ["path", { d: BOOKMARK_PATH, fill: "none", stroke: "currentColor", "stroke-width": 2, "stroke-linejoin": "round" }]),
    wanted: () => icon({}, ["path", { d: BOOKMARK_PATH, fill: "currentColor", stroke: "currentColor", "stroke-width": 2, "stroke-linejoin": "round" }]),
    read: () => icon({}, ["path", { d: "M8 5v14l11-7z", fill: "currentColor" }]),
    done: () =>
        icon({}, ["path", { d: "M5 12.5l4.5 4.5L19 7.5", fill: "none", stroke: "currentColor", "stroke-width": 2.5, "stroke-linecap": "round", "stroke-linejoin": "round" }]),
};

/** A cover button (or link, or badge) with a title, label and icon. */
function coverButton<K extends "button" | "a" | "span">(tag: K, cls: string, title: string, ...children: (Node | string)[]): HTMLElementTagNameMap[K] {
    const b = el(tag, { className: `cover-action ${cls}`, title } as Partial<HTMLElementTagNameMap[K]>, ...children);
    b.setAttribute("aria-label", title);
    if (b instanceof HTMLButtonElement) b.type = "button";
    return b;
}

/** The right button's state, which decides what it shows and does. */
function rightState(uuid: string, marks: Marks): "want" | "wanted" | "read" | "done" | "" {
    if (marks.finished.has(uuid)) return "done";
    if (marks.reading.has(uuid)) return marks.resume.has(uuid) ? "read" : "";
    return marks.wanted.has(uuid) ? "wanted" : "want";
}

function paintActions(card: HTMLElement, uuid: string, marks: Marks): void {
    let box = card.querySelector<HTMLElement>(".cover-actions");
    if (!marks.signedIn) {
        box?.remove();
        return;
    }
    const fav = marks.favorites.has(uuid);
    const right = rightState(uuid, marks);
    const key = `${fav}:${right}`;
    if (box?.dataset["key"] === key) return;
    if (!box) {
        box = document.createElement("div");
        box.className = "cover-actions";
        card.append(box);
    }
    box.dataset["key"] = key;

    const favTitle = fav ? "Remove from favorites" : "Add to favorites";
    const favBtn = coverButton("button", `cover-fav${fav ? " is-on" : ""}`, favTitle, fav ? "★" : "☆");
    favBtn.dataset["action"] = "favorite";
    favBtn.setAttribute("aria-pressed", String(fav));
    const buttons: HTMLElement[] = [favBtn];
    switch (right) {
        case "want":
        case "wanted": {
            const title = right === "wanted" ? "Remove from Saved" : "Save for later";
            const b = coverButton("button", `cover-want${right === "wanted" ? " is-on" : ""}`, title, ICONS[right]());
            b.dataset["action"] = "want";
            b.setAttribute("aria-pressed", String(right === "wanted"));
            buttons.push(b);
            break;
        }
        case "read": {
            const a = coverButton("a", "cover-read", "Continue reading", ICONS.read());
            a.href = safeUrl(marks.resume.get(uuid));
            buttons.push(a);
            break;
        }
        case "done": {
            const done = coverButton("span", "cover-done", "Finished", ICONS.done());
            done.setAttribute("role", "img");
            buttons.push(done);
            break;
        }
    }
    box.replaceChildren(...buttons);
}

/**
 * Do a cover button's action for a book: toggle favorite, or toggle "want
 * to read" (only offered for books not started). marks is updated at once;
 * repaint is called then, and again if saving fails (marks put back). Once
 * saved, marks are kept as the cache: no need to ask the server again.
 */
export async function coverAction(action: string, uuid: string, marks: Marks, repaint: () => void): Promise<void> {
    if (action === "favorite") {
        const on = !marks.favorites.has(uuid);
        const set = (v: boolean): void => void (v ? marks.favorites.add(uuid) : marks.favorites.delete(uuid));
        set(on);
        repaint();
        try {
            await api("PUT", `/api/me/books/${encodeURIComponent(uuid)}/favorite`, { favorite: on });
            saveMarks(marks);
        } catch {
            set(!on);
            repaint();
        }
    } else if (action === "want") {
        const on = !marks.wanted.has(uuid);
        const set = (v: boolean): void => void (v ? marks.wanted.add(uuid) : marks.wanted.delete(uuid));
        set(on);
        repaint();
        try {
            await api("PUT", `/api/me/books/${encodeURIComponent(uuid)}/status`, { status: on ? "wanted" : "" });
            saveMarks(marks);
        } catch {
            set(!on);
            repaint();
        }
    }
}
