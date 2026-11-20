/**
 * Reading position + bookmarks for the built-in readers.
 *
 * Always kept in localStorage (works offline / when the server is down).
 * When signed in, also synced with the server so another device or a fresh
 * login can resume:
 *   - position: last-writer-wins on updatedAt (ms); the newer of local and
 *     server is used when a book opens. Sent as soon as the reader moves (one
 *     request at a time: moves made meanwhile go in the next); failed pushes
 *     are retried when the server can be reached again (server-watch.ts),
 *     when the page is hidden, and from any page (send-pending.ts).
 *   - bookmarks: ids are generated here, so copies made offline merge; local
 *     deletions are queued until the server confirms them.
 */

import { api, random } from "./dom.js";
import { lastKnownUser, localKey, type Owner } from "./local-keys.js";
import { noteLocalReading } from "./local-reading.js";
import { noteReadingHere } from "./offline-books.js";
import { refusedForGood } from "./send-pending.js";
import { isServerDown, isUnreachable, onServerBack, serverDown } from "./server-watch.js";
import { SYNC_EVENT } from "./sync-state.js";
import type { MeResponse } from "./types.js";

export type BookFormat = "epub" | "pdf";

/** Position as kept locally. */
interface LocalProgress {
    position: string;
    percent: number;
    updatedAt: number;
    pending?: boolean;
}

/** Position as sent/received by the server (Go: progressRec). */
interface ServerProgress {
    format: BookFormat;
    position: string;
    percent: number;
    updated_at: number;
}

/** A bookmark (Go: bookmarkRec), plus whether the server has it yet. */
export interface Bookmark {
    id: string;
    format: BookFormat;
    position: string;
    label: string;
    created_at: number;
    synced?: boolean;
}

export interface ReadingSync {
    signedIn: boolean;
    /** Whose records these are (null: signed out). */
    user: Owner;
    loadProgress(): Promise<LocalProgress | null>;
    saveProgress(position: string | number, percent: number): void;
    loadBookmarks(): Promise<Bookmark[]>;
    addBookmark(position: string | number, label: string): Bookmark;
    removeBookmark(id: string): void;
    bookmarks(): Bookmark[];
}

const LS = {
    get<T>(key: string, fallback: T): T {
        try {
            const v = localStorage.getItem(key);
            return v === null ? fallback : (JSON.parse(v) as T);
        } catch {
            return fallback;
        }
    },
    set(key: string, value: unknown): void {
        try {
            localStorage.setItem(key, JSON.stringify(value));
        } catch {
            /* storage full or blocked: in-memory only */
        }
        window.dispatchEvent(new CustomEvent(SYNC_EVENT)); // the sync buttons count what's unsent
    },
};

const newId = (): string =>
    (crypto.randomUUID?.() ?? `${Date.now().toString(36)}-${random().toString(36).slice(2)}`).slice(0, 64);

/** The server answered that nobody is signed in (the session ended). */
const signedOut = (e: unknown): boolean => (e as { status?: number } | null)?.status === 401;

export async function createReadingSync(
    uuid: string,
    format: BookFormat,
    {
        legacyKey,
        onStatus = () => {},
        title = () => "",
    }: { legacyKey?: string; onStatus?: (text: string) => void; title?: () => string } = {},
): Promise<ReadingSync> {
    // A signed-in user's records are under their name, a guest's under the
    // plain names (local-keys.ts). Only an answer saying nobody is signed in,
    // on a server where guests may read, makes this a guest. Otherwise
    // (offline; an error answer, as a proxy gives while the server restarts;
    // signed out where guests can't read, the session having ended under a
    // page from the offline copy) it's whoever the top bar last saw signed
    // in: their changes wait under their name and are sent once the server
    // takes them. Saved as a guest's, they'd come back later as reading
    // "done signed out on this device", to review and add to the account.
    let user: Owner = null;
    try {
        const r = await fetch("/api/me", { credentials: "same-origin" });
        const me = (await r.json().catch(() => null)) as MeResponse | null;
        if (r.ok) user = me?.user?.username ?? null;
        else if (r.status === 401 && me?.guest_reading) user = null;
        else {
            user = lastKnownUser();
            if (isUnreachable({ status: r.status })) serverDown();
        }
    } catch {
        user = lastKnownUser();
        serverDown();
    }
    const signedIn = user !== null;
    const progressKey = localKey(user, `progress-${uuid}-${format}`);
    const bookmarksKey = localKey(user, `bookmarks-${uuid}`);
    const deletedKey = localKey(user, `bookmarks-deleted-${uuid}`);

    // ---- position ---------------------------------------------------------

    let local = LS.get<LocalProgress | null>(progressKey, null);
    if (!local && legacyKey && !signedIn) {
        // Older readers stored a bare position under another key.
        const legacy = localStorage.getItem(legacyKey);
        if (legacy && legacy !== "undefined") local = { position: legacy, percent: 0, updatedAt: 0 };
    }

    async function push(keepalive = false): Promise<void> {
        if (!signedIn || !local?.pending) return;
        const body = { format, position: local.position, percent: local.percent, updated_at: local.updatedAt };
        try {
            await api("PUT", `/api/me/progress/${uuid}`, body, { keepalive });
            if (local && local.updatedAt === body.updated_at) {
                local.pending = false;
                LS.set(progressKey, local);
            }
            onStatus("Saved");
        } catch (e) {
            if (refusedForGood(e)) {
                // The book is gone from the library, or the server won't take it: don't keep it waiting forever.
                if (local && local.updatedAt === body.updated_at) {
                    local.pending = false;
                    LS.set(progressKey, local);
                }
                onStatus("Not saved to the server");
                return;
            }
            if (isUnreachable(e)) serverDown();
            onStatus(signedOut(e) ? "Signed out — saved on this device until you sign in" : "Offline — saved on this device");
        }
    }

    async function loadProgress(): Promise<LocalProgress | null> {
        if (!signedIn) return local;
        try {
            const { progress: server } = await api<{ progress: ServerProgress | null }>(
                "GET",
                `/api/me/progress/${uuid}?format=${format}`,
            );
            if (server && (!local || server.updated_at > (local.updatedAt || 0))) {
                local = { position: server.position, percent: server.percent, updatedAt: server.updated_at };
                LS.set(progressKey, local);
                onStatus("Synced");
            } else if (local?.pending) {
                void push(); // local is newer and never reached the server
            }
        } catch (e) {
            if (isUnreachable(e)) serverDown();
            onStatus(signedOut(e) ? "Signed out — saved on this device until you sign in" : "Offline — saved on this device");
        }
        return local;
    }

    // Sent at once; while a send is on its way, the latest position goes right after it.
    let sending: Promise<void> | null = null;
    let sendAgain = false;
    function sendNow(): void {
        // The server isn't answering: it's sent when it is again (onServerBack below).
        if (isServerDown()) return;
        if (sending) {
            sendAgain = true;
            return;
        }
        sending = push().finally(() => {
            sending = null;
            if (sendAgain) {
                sendAgain = false;
                sendNow();
            }
        });
    }

    /** Record a new position (call when the reader actually moves). */
    function saveProgress(position: string | number, percent: number): void {
        local = {
            position: String(position),
            percent: Math.max(0, Math.min(1, percent || 0)),
            updatedAt: Date.now(),
            pending: signedIn,
        };
        LS.set(progressKey, local);
        if (!signedIn) noteLocalReading(uuid, format, local.percent, local.updatedAt);
        // The Reading list kept for the offline page: this book is being read now.
        else if (user !== null) noteReadingHere(user, uuid, local.percent, title());
        if (legacyKey && !signedIn) localStorage.setItem(legacyKey, String(position));
        sendNow();
    }

    // ---- bookmarks --------------------------------------------------------

    let bookmarks = LS.get<Bookmark[]>(bookmarksKey, []);
    let deleted = LS.get<string[]>(deletedKey, []);
    const saveLocal = (): void => {
        LS.set(bookmarksKey, bookmarks);
        LS.set(deletedKey, deleted);
    };

    async function flushBookmarks(): Promise<void> {
        if (!signedIn) return;
        for (const id of [...deleted]) {
            try {
                await api("DELETE", `/api/me/bookmarks/${uuid}/${encodeURIComponent(id)}`);
                deleted = deleted.filter((d) => d !== id);
            } catch (e) {
                if (refusedForGood(e)) {
                    deleted = deleted.filter((d) => d !== id); // gone there already, or never will be
                    continue;
                }
                if (isUnreachable(e)) serverDown();
                break;
            }
        }
        for (const b of bookmarks.filter((x) => !x.synced)) {
            try {
                await api("PUT", `/api/me/bookmarks/${uuid}/${encodeURIComponent(b.id)}`, b);
                b.synced = true;
            } catch (e) {
                // Refused for good (the book gone, a bad or one-too-many bookmark): kept here, not retried forever.
                if (refusedForGood(e)) {
                    b.synced = true;
                    continue;
                }
                if (isUnreachable(e)) serverDown();
                break;
            }
        }
        saveLocal();
    }

    async function loadBookmarks(): Promise<Bookmark[]> {
        if (signedIn) {
            try {
                await flushBookmarks();
                const { bookmarks: server } = await api<{ bookmarks: Bookmark[] }>("GET", `/api/me/bookmarks/${uuid}`);
                const ids = new Set(server.map((b) => b.id));
                const unsynced = bookmarks.filter((b) => !b.synced && !ids.has(b.id));
                bookmarks = [...server.map((b) => ({ ...b, synced: true })), ...unsynced]
                    .filter((b) => !deleted.includes(b.id))
                    .sort((a, b) => a.created_at - b.created_at);
                saveLocal();
            } catch (e) {
                if (isUnreachable(e)) serverDown(); // offline: use the local copy
            }
        }
        return bookmarks.filter((b) => b.format === format);
    }

    function addBookmark(position: string | number, label: string): Bookmark {
        const b: Bookmark = {
            id: newId(),
            format,
            position: String(position),
            label: (label || "").slice(0, 200),
            created_at: Date.now(),
            synced: false,
        };
        bookmarks.push(b);
        saveLocal();
        void flushBookmarks();
        return b;
    }

    function removeBookmark(id: string): void {
        const had = bookmarks.find((b) => b.id === id);
        bookmarks = bookmarks.filter((b) => b.id !== id);
        if (had?.synced) deleted.push(id);
        saveLocal();
        void flushBookmarks();
    }

    // Retry anything pending when the server can be reached again (the
    // device's network coming back, or the server itself) / leave the page.
    onServerBack(() => {
        if (!signedIn) return;
        if (local?.pending) sendNow();
        else onStatus("Synced");
        void flushBookmarks();
    });
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden") void push(true);
    });
    window.addEventListener("pagehide", () => void push(true));

    return {
        signedIn,
        user,
        loadProgress,
        saveProgress,
        loadBookmarks,
        addBookmark,
        removeBookmark,
        bookmarks: () => bookmarks.filter((b) => b.format === format),
    };
}

export interface BookmarkHandlers {
    onGo(b: Bookmark): void;
    onDelete(b: Bookmark): void;
}

/** Renders a bookmark list (shared by both readers). */
export function renderBookmarks(listEl: HTMLElement, items: Bookmark[], { onGo, onDelete }: BookmarkHandlers): void {
    listEl.replaceChildren();
    if (items.length === 0) {
        const li = document.createElement("li");
        li.className = "bookmark-empty";
        li.textContent = "No bookmarks yet. The bookmark button in the top bar adds one.";
        listEl.append(li);
        return;
    }
    for (const b of items) {
        const li = document.createElement("li");
        li.className = "bookmark-item";
        const go = document.createElement("a");
        go.href = "#";
        go.textContent = b.label || b.position;
        go.addEventListener("click", (e) => {
            e.preventDefault();
            onGo(b);
        });
        const del = document.createElement("button");
        del.type = "button";
        del.className = "bookmark-delete";
        del.textContent = "×";
        del.title = "Delete bookmark";
        del.addEventListener("click", () => onDelete(b));
        li.append(go, del);
        listEl.append(li);
    }
}
