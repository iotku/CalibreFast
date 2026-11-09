/**
 * Reading position + bookmarks for the built-in readers.
 *
 * Always kept in localStorage (works offline / when the server is down).
 * When signed in, also synced with the server so another device or a fresh
 * login can resume:
 *   - position: last-writer-wins on updatedAt (ms); the newer of local and
 *     server is used when a book opens; failed pushes are retried when the
 *     connection comes back and when the page is hidden.
 *   - bookmarks: ids are generated here, so copies made offline merge; local
 *     deletions are queued until the server confirms them.
 */
import { api } from "./dom.js";
const LS = {
    get(key, fallback) {
        try {
            const v = localStorage.getItem(key);
            return v === null ? fallback : JSON.parse(v);
        }
        catch {
            return fallback;
        }
    },
    set(key, value) {
        try {
            localStorage.setItem(key, JSON.stringify(value));
        }
        catch {
            /* storage full or blocked: in-memory only */
        }
    },
};
const newId = () => (crypto.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`).slice(0, 64);
export async function createReadingSync(uuid, format, { legacyKey, onStatus = () => { } } = {}) {
    const progressKey = `cf-progress-${uuid}-${format}`;
    const bookmarksKey = `cf-bookmarks-${uuid}`;
    const deletedKey = `cf-bookmarks-deleted-${uuid}`;
    let signedIn = false;
    try {
        signedIn = (await fetch("/api/me", { credentials: "same-origin" })).ok;
    }
    catch {
        /* offline */
    }
    // ---- position ---------------------------------------------------------
    let local = LS.get(progressKey, null);
    if (!local && legacyKey) {
        // Older readers stored a bare position under another key.
        const legacy = localStorage.getItem(legacyKey);
        if (legacy && legacy !== "undefined")
            local = { position: legacy, percent: 0, updatedAt: 0 };
    }
    let timer;
    async function push(keepalive = false) {
        clearTimeout(timer);
        if (!signedIn || !local?.pending)
            return;
        const body = { format, position: local.position, percent: local.percent, updated_at: local.updatedAt };
        try {
            await api("PUT", `/api/me/progress/${uuid}`, body, { keepalive });
            if (local && local.updatedAt === body.updated_at) {
                local.pending = false;
                LS.set(progressKey, local);
            }
            onStatus("Saved");
        }
        catch {
            onStatus("Offline — saved on this device");
        }
    }
    async function loadProgress() {
        if (!signedIn)
            return local;
        try {
            const { progress: server } = await api("GET", `/api/me/progress/${uuid}?format=${format}`);
            if (server && (!local || server.updated_at > (local.updatedAt || 0))) {
                local = { position: server.position, percent: server.percent, updatedAt: server.updated_at };
                LS.set(progressKey, local);
                onStatus("Synced");
            }
            else if (local?.pending) {
                void push(); // local is newer and never reached the server
            }
        }
        catch {
            onStatus("Offline — saved on this device");
        }
        return local;
    }
    /** Record a new position (call when the reader actually moves). */
    function saveProgress(position, percent) {
        local = {
            position: String(position),
            percent: Math.max(0, Math.min(1, percent || 0)),
            updatedAt: Date.now(),
            pending: signedIn,
        };
        LS.set(progressKey, local);
        if (legacyKey)
            localStorage.setItem(legacyKey, String(position));
        clearTimeout(timer);
        timer = setTimeout(() => void push(), 1500);
    }
    // ---- bookmarks --------------------------------------------------------
    let bookmarks = LS.get(bookmarksKey, []);
    let deleted = LS.get(deletedKey, []);
    const saveLocal = () => {
        LS.set(bookmarksKey, bookmarks);
        LS.set(deletedKey, deleted);
    };
    async function flushBookmarks() {
        if (!signedIn)
            return;
        for (const id of [...deleted]) {
            try {
                await api("DELETE", `/api/me/bookmarks/${uuid}/${encodeURIComponent(id)}`);
                deleted = deleted.filter((d) => d !== id);
            }
            catch {
                break;
            }
        }
        for (const b of bookmarks.filter((x) => !x.synced)) {
            try {
                await api("PUT", `/api/me/bookmarks/${uuid}/${encodeURIComponent(b.id)}`, b);
                b.synced = true;
            }
            catch {
                break;
            }
        }
        saveLocal();
    }
    async function loadBookmarks() {
        if (signedIn) {
            try {
                await flushBookmarks();
                const { bookmarks: server } = await api("GET", `/api/me/bookmarks/${uuid}`);
                const ids = new Set(server.map((b) => b.id));
                const unsynced = bookmarks.filter((b) => !b.synced && !ids.has(b.id));
                bookmarks = [...server.map((b) => ({ ...b, synced: true })), ...unsynced]
                    .filter((b) => !deleted.includes(b.id))
                    .sort((a, b) => a.created_at - b.created_at);
                saveLocal();
            }
            catch {
                /* offline: use the local copy */
            }
        }
        return bookmarks.filter((b) => b.format === format);
    }
    function addBookmark(position, label) {
        const b = {
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
    function removeBookmark(id) {
        const had = bookmarks.find((b) => b.id === id);
        bookmarks = bookmarks.filter((b) => b.id !== id);
        if (had?.synced)
            deleted.push(id);
        saveLocal();
        void flushBookmarks();
    }
    // Retry anything pending when we come back online / leave the page.
    window.addEventListener("online", () => {
        void push();
        void flushBookmarks();
    });
    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "hidden")
            void push(true);
    });
    window.addEventListener("pagehide", () => void push(true));
    return {
        signedIn,
        loadProgress,
        saveProgress,
        loadBookmarks,
        addBookmark,
        removeBookmark,
        bookmarks: () => bookmarks.filter((b) => b.format === format),
    };
}
/** Renders a bookmark list (shared by both readers). */
export function renderBookmarks(listEl, items, { onGo, onDelete }) {
    listEl.replaceChildren();
    if (items.length === 0) {
        const li = document.createElement("li");
        li.className = "bookmark-empty";
        li.textContent = "No bookmarks yet. Use 🔖 to add one.";
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
