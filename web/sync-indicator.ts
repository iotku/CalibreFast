/**
 * The reader's sync button (top bar, reader-parts.html): at a glance,
 *   - whether this book is saved on this device for reading offline
 *     (book-cache.ts), being saved (a ring with its progress), or not, and
 *     why (retention.ts's rules, the book's own choice);
 *   - whether the reader's place, bookmarks and highlights have reached the
 *     server (reading-sync.ts, annotations.ts keep them here until it
 *     confirms), are being sent, or wait for a connection.
 * Pressing it shows both in words.
 *
 * Reads the same records the sync code writes (localStorage, IndexedDB),
 * so it needs no hooks into it: looked at again every couple of seconds
 * while shown, and at once when something it hears of changes.
 */

import { currentUser, getBook, offlinePref, type KeptFormat } from "./book-cache.js";
import { maybeId } from "./dom.js";
import { devicePrefs } from "./prefs.js";
import { isServerDown, onServerBack } from "./server-watch.js";
import { unsentFor, type UnsentBook } from "./sync-state.js";

export interface SyncIndicator {
    /** The reader is downloading the whole file to keep: 0..1, null (size unknown), or false (done). */
    downloading(progress: number | null | false): void;
    /** The file's size, once known (to say when it's over the size limit). */
    setSize(bytes: number): void;
    /** Look again now (something changed). */
    refresh(): void;
}

const megabytes = (n: number): string => `${(n / 1048576).toFixed(n < 10485760 ? 1 : 0)} MB`;
const plural = (n: number, one: string): string => `${n} ${one}${n === 1 ? "" : "s"}`;

/** What's not yet on the server, in words ("your place and 2 highlights"). */
function describe(u: UnsentBook): string {
    const parts = [
        ...(u.position ? ["your place"] : []),
        ...(u.bookmarks ? [plural(u.bookmarks, "bookmark")] : []),
        ...(u.highlights ? [plural(u.highlights, "highlight")] : []),
    ];
    return parts.length > 1 ? `${parts.slice(0, -1).join(", ")} and ${parts.at(-1)}` : (parts[0] ?? "");
}

export function createSyncIndicator(uuid: string, format: KeptFormat): SyncIndicator {
    const btn = maybeId("sync-btn") as HTMLButtonElement | null;
    const pop = maybeId("sync-pop");
    const copyLine = maybeId("sync-pop-copy");
    const syncLine = maybeId("sync-pop-sync");
    const glyph = btn?.querySelector<SVGUseElement>(".r-sync-glyph use") ?? null;
    const arc = btn?.querySelector<SVGCircleElement>(".r-sync-ring circle") ?? null;
    const none: SyncIndicator = { downloading: () => {}, setSize: () => {}, refresh: () => {} };
    if (!btn || !pop || !copyLine || !syncLine || !glyph || !arc) return none;

    let progress: number | null | false = false;
    let size = 0;
    let kept: { size: number } | null = null;

    async function paint(): Promise<void> {
        const user = currentUser();
        kept = await getBook(uuid, format).catch(() => null);
        const pref = offlinePref(uuid);
        const prefs = devicePrefs();

        // This device's copy.
        let copy: string;
        if (kept) copy = `Saved on this device for reading offline (${format.toUpperCase()}, ${megabytes(kept.size)})${pref === "keep" ? ", kept for good" : ""}.`;
        else if (progress !== false) copy = `Saving on this device for reading offline…${progress === null ? "" : ` ${Math.round(progress * 100)}%`}`;
        else if (pref === "never") copy = "Not saved on this device: you chose not to keep this book (on its page).";
        else if (prefs.readingKeepDays === -1) copy = "Not saved on this device: books aren't kept automatically (Preferences).";
        else if (size && prefs.maxBookBytes && size > prefs.maxBookBytes)
            copy = `Not saved on this device: larger than your ${megabytes(prefs.maxBookBytes)} limit (Preferences, or Always keep on its page).`;
        else copy = "Not saved on this device yet. It's kept once it has finished downloading.";

        // The server's copy of the reading.
        const offline = !navigator.onLine || isServerDown();
        const unsent = user === null ? null : unsentFor(user, uuid);
        const waiting = unsent ? describe(unsent) : "";
        let sync: string;
        let state: "synced" | "sending" | "offline" | "local";
        if (user === null) {
            sync = "Your place, bookmarks and highlights are kept on this device only. Sign in to have them on every device.";
            state = "local";
        } else if (waiting && offline) {
            sync = `No connection to the server: ${waiting} kept on this device, sent once it's back.`;
            state = "offline";
        } else if (waiting) {
            sync = `Sending ${waiting} to the server…`;
            state = "sending";
        } else if (offline) {
            sync = "No connection to the server. Everything so far is saved there; new changes are kept on this device until it's back.";
            state = "offline";
        } else {
            sync = "Your place, bookmarks and highlights are saved to the server.";
            state = "synced";
        }

        glyph!.setAttribute("href", `#i-sync-${state}`);
        btn!.dataset["state"] = state;
        btn!.classList.toggle("is-kept", !!kept);
        btn!.classList.toggle("is-saving", !kept && progress !== false);
        arc!.setAttribute("stroke-dasharray", `${!kept && progress !== false ? (progress === null ? 12 : Math.round(progress * 100)) : 0} 100`);
        const label = `${kept ? "Saved offline" : progress !== false ? "Saving offline" : "Not saved offline"}. ${
            state === "synced" ? "Synced" : state === "sending" ? "Syncing" : state === "offline" ? "Waiting for a connection" : "On this device only"
        }.`;
        btn!.title = label;
        btn!.setAttribute("aria-label", label);
        if (copyLine!.textContent !== copy) copyLine!.textContent = copy;
        if (syncLine!.textContent !== sync) syncLine!.textContent = sync;
    }
    const refresh = (): void => void paint();

    // The popup.
    const setOpen = (open: boolean): void => {
        pop.classList.toggle("open", open);
        btn.setAttribute("aria-expanded", String(open));
        if (open) refresh();
    };
    btn.addEventListener("click", (e) => {
        e.stopPropagation();
        setOpen(!pop.classList.contains("open"));
    });
    document.addEventListener("click", (e) => {
        if (pop.classList.contains("open") && !pop.contains(e.target as Node)) setOpen(false);
    });
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && pop.classList.contains("open")) {
            setOpen(false);
            btn.focus();
        }
    });

    window.addEventListener("cf:offline-changed", refresh);
    window.addEventListener("online", refresh);
    window.addEventListener("offline", refresh);
    onServerBack(refresh);
    // The sync code writes as it goes; sends follow a moment later.
    setInterval(() => {
        if (document.visibilityState === "visible") refresh();
    }, 2000);
    btn.hidden = false;
    refresh();

    return {
        downloading(p) {
            progress = p;
            refresh();
        },
        setSize(bytes) {
            size = bytes;
        },
        refresh,
    };
}
