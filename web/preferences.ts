/**
 * /preferences (templates/preferences.html): the account's preferences
 * (prefs.go; a guest's are only in this browser) and this device's
 * (prefs.ts, applied by retention.ts), and what this device stores: a bar
 * of the space used against the limit and the browser's quota, and the
 * books kept here by status, each with a Remove button.
 *
 * Everything is built with DOM calls (el()); titles and authors are text.
 */

import { deleteBook, listBooks, offlinePref, setOfflinePref, type CachedBook } from "./book-cache.js";
import { prefsSettled } from "./account-prefs.js";
import { byId, el, errorMessage } from "./dom.js";
import { lastKnownUser } from "./local-keys.js";
import { meRequest } from "./me.js";
import {
    accountPrefs,
    AUTO_LIMIT,
    DEVICE_CHOICES,
    devicePrefs,
    FORMAT_PREFS,
    setAccountPrefs,
    setDevicePref,
    type DevicePrefs,
    type FormatPref,
} from "./prefs.js";
import { cachedMarks, fetchMarks, statusOf, type Marks } from "./reading.js";
import { syncBooks } from "./book-sync.js";
import { makeRoom, requestPersist, storageLimit } from "./retention.js";
import { onSyncChange, readSync } from "./sync-state.js";

const MB = 1 << 20;

function size(n: number): string {
    if (n >= 1024 * MB) return `${(n / (1024 * MB)).toFixed(n >= 10 * 1024 * MB ? 0 : 1)} GB`;
    if (n >= MB) return `${(n / MB).toFixed(n >= 10 * MB ? 0 : 1)} MB`;
    return `${Math.max(1, Math.round(n / 1024))} KB`;
}

let user: string | null = lastKnownUser();
let marks: Marks = cachedMarks();

/* ------------------------------------------------------------------ */
/* Format (the account's)                                             */
/* ------------------------------------------------------------------ */

const formatStatus = byId("prefs-format-status");

function showFormat(): void {
    const f = accountPrefs(user).format;
    for (const r of document.querySelectorAll<HTMLInputElement>('input[name="format"]')) r.checked = r.value === f;
}

for (const r of document.querySelectorAll<HTMLInputElement>('input[name="format"]')) {
    r.addEventListener("change", () => {
        const format = r.value as FormatPref;
        if (!FORMAT_PREFS.includes(format)) return;
        formatStatus.textContent = "Saving…";
        setAccountPrefs({ format }, user)
            .then(() => (formatStatus.textContent = user ? "Saved to your account." : "Saved in this browser."))
            .catch((err: unknown) => {
                formatStatus.textContent = `Couldn't save: ${errorMessage(err)}`;
                showFormat();
            });
    });
}

/* ------------------------------------------------------------------ */
/* This device's                                                      */
/* ------------------------------------------------------------------ */

const deviceStatus = byId("prefs-device-status");
const selects = [...document.querySelectorAll<HTMLSelectElement>("select[data-pref]")];
const keyOf = (s: HTMLSelectElement): keyof DevicePrefs => s.dataset["pref"] as keyof DevicePrefs;

/** The browser's quota for this site, if it says (bytes). */
let quota = 0;
/** The storage limit in force (bytes; 0: none): the user's, else half of the quota (retention.ts). */
let limitInForce = 0;

function showDevice(): void {
    const p = devicePrefs(user);
    for (const s of selects) {
        const key = keyOf(s);
        s.value = String(p[key]);
        if (key !== "maxBytes") continue;
        for (const o of s.options) {
            const v = Number(o.value);
            // The default says what it comes to here.
            if (v === AUTO_LIMIT) o.textContent = `Half of what the browser allows${quota ? ` (${size(Math.floor(quota / 2))})` : ""}`;
            // A limit the browser wouldn't allow anyway.
            else o.disabled = !!quota && v > quota;
        }
    }
    // Keeping the last N doesn't apply when nothing is kept automatically.
    const keepN = selects.find((s) => keyOf(s) === "keepReadingN");
    if (keepN) keepN.disabled = p.readingKeepDays === -1;
}

async function applied(key: keyof DevicePrefs): Promise<void> {
    if (!navigator.onLine) return; // offline, a kept book may be the only way to read it: tidy up later
    if (key === "maxBytes") await makeRoom(0);
    // What to keep changed: tidy with fresh statuses, and download what's now wanted (book-sync.ts).
    else await syncBooks({ force: true });
    const fresh = user ? await fetchMarks() : null;
    if (fresh) marks = fresh;
}

for (const s of selects) {
    s.addEventListener("change", () => {
        const key = keyOf(s);
        const v = Number(s.value);
        if (!DEVICE_CHOICES[key].includes(v)) return;
        setDevicePref(key, v, user);
        requestPersist();
        showDevice();
        deviceStatus.textContent = "Saved on this device.";
        void applied(key)
            .catch((err: unknown) => (deviceStatus.textContent = `Saved, but couldn't apply it now: ${errorMessage(err)}`))
            .finally(() => void render());
    });
}

/* ------------------------------------------------------------------ */
/* Storage                                                            */
/* ------------------------------------------------------------------ */

type Group = "reading" | "wanted" | "finished" | "other";
const GROUPS: { key: Group; label: string }[] = [
    { key: "reading", label: "Reading" },
    { key: "wanted", label: "Saved" },
    { key: "finished", label: "Finished" },
    { key: "other", label: "Kept on this device" },
];

function groupOf(b: CachedBook): Group {
    if (!marks.signedIn) return "other";
    const s = statusOf(marks, b.uuid);
    return s === "reading" || s === "wanted" || s === "finished" ? s : b.statusSeen ? "other" : "reading";
}

/** The bytes in the covers cache (offline-books.ts). */
async function coversSize(): Promise<number> {
    if (typeof caches === "undefined") return 0;
    try {
        const cache = await caches.open("cf-covers");
        let n = 0;
        for (const req of await cache.keys()) n += (await (await cache.match(req))?.blob())?.size ?? 0;
        return n;
    } catch {
        return 0;
    }
}

interface Segment {
    label: string;
    bytes: number;
    cls: string;
}

/** A stacked bar: segments as shares of whole, with a legend in text. */
function bar(title: string, whole: number, segments: Segment[], note: string): HTMLElement {
    const shown = segments.filter((s) => s.bytes > 0);
    const track = el("div", { className: "prefs-bar" });
    track.setAttribute("role", "img");
    track.setAttribute("aria-label", `${title}: ${shown.map((s) => `${s.label} ${size(s.bytes)}`).join(", ")}`);
    for (const s of shown) {
        const seg = el("span", { className: `prefs-seg ${s.cls}`, title: `${s.label}: ${size(s.bytes)}` });
        // At least a sliver, so a small part still shows.
        seg.style.width = `${Math.max(0.6, (s.bytes / whole) * 100)}%`;
        track.append(seg);
    }
    const legend = el(
        "ul",
        { className: "prefs-legend" },
        ...shown.map((s) => el("li", {}, el("span", { className: `prefs-swatch ${s.cls}` }), el("span", { textContent: `${s.label} ${size(s.bytes)}` }))),
    );
    return el("div", { className: "prefs-meter" }, el("p", { className: "prefs-meter-title", textContent: title }), track, legend, el("p", { className: "auth-muted", textContent: note }));
}

let removeAllArmed = false;
const removeAll = byId("prefs-remove-all", HTMLButtonElement);

async function render(): Promise<void> {
    const books = await listBooks().catch(() => [] as CachedBook[]);
    const covers = await coversSize();
    const p = devicePrefs(user);
    const est = await navigator.storage?.estimate?.().catch(() => null);
    quota = est?.quota ?? 0;
    limitInForce = await storageLimit(p);
    const persisted = await navigator.storage?.persisted?.().catch(() => false);
    showDevice();

    const byGroup = new Map<Group, CachedBook[]>(GROUPS.map((g) => [g.key, []]));
    for (const b of books) byGroup.get(groupOf(b))!.push(b);
    const sum = (bs: CachedBook[]): number => bs.reduce((n, b) => n + b.size, 0);
    const booksTotal = sum(books);
    const segs: Segment[] = GROUPS.map((g) => ({ label: g.label, bytes: sum(byGroup.get(g.key)!), cls: `seg-${g.key}` }));

    // 1. Books against the limit (or, with none, against what the browser allows).
    const limit = limitInForce || quota;
    const free = Math.max(0, limit - booksTotal);
    const over = limitInForce && booksTotal > limitInForce ? booksTotal - limitInForce : 0;
    const meters: HTMLElement[] = [
        bar(
            limitInForce ? `Books: ${size(booksTotal)} of your ${size(limitInForce)} limit` : `Books: ${size(booksTotal)}, no limit set`,
            Math.max(limit, booksTotal) || 1,
            [...segs, { label: "Free", bytes: limit ? free : 0, cls: "seg-free" }],
            over
                ? `Over the limit by ${size(over)}: what's left is books you're reading or chose to always keep.`
                : `${books.length} file${books.length === 1 ? "" : "s"} kept here.`,
        ),
    ];
    // 2. Everything this site stores, against the browser's quota.
    if (est?.usage !== undefined && quota) {
        // The browser's figure lags behind (and may count compressed): never below what we know we keep.
        const used = Math.max(est.usage, booksTotal + covers);
        const other = Math.max(0, used - booksTotal - covers);
        meters.push(
            bar(
                `This browser: ${size(used)} used of ${size(quota)} it allows this site`,
                quota,
                [
                    { label: "Books", bytes: booksTotal, cls: "seg-books" },
                    { label: "Covers", bytes: covers, cls: "seg-covers" },
                    { label: "App and other data", bytes: other, cls: "seg-other-data" },
                    { label: "Available", bytes: Math.max(0, quota - used), cls: "seg-free" },
                ],
                persisted
                    ? "Storage is persistent: the browser won't clear it to free up space."
                    : "The browser may clear this storage if the device runs low on space.",
            ),
        );
    } else if (covers) {
        meters.push(el("p", { className: "auth-muted", textContent: `Covers for the offline page: ${size(covers)}.` }));
    }
    byId("prefs-storage").replaceChildren(...meters);

    // The books, by status.
    const lists: HTMLElement[] = [];
    for (const g of GROUPS) {
        const bs = byGroup.get(g.key)!.sort((a, b) => b.lastOpened - a.lastOpened);
        if (!bs.length) continue;
        lists.push(
            el("h3", { className: "prefs-group", textContent: `${g.label} (${size(sum(bs))})` }),
            el("ul", { className: "prefs-book-list" }, ...bs.map(bookRow)),
        );
    }
    if (!lists.length) lists.push(el("p", { className: "auth-muted", textContent: "No books are kept on this device." }));
    byId("prefs-books").replaceChildren(...lists);
    removeAll.hidden = books.length === 0;
    removeAllArmed = false;
    removeAll.textContent = "Remove all books from this device";
}

function bookRow(b: CachedBook): HTMLElement {
    const pinned = offlinePref(b.uuid) === "keep";
    const remove = el("button", { type: "button", className: "btn-standard", textContent: "Remove" });
    remove.setAttribute("aria-label", `Remove ${b.title || "this book"} (${(b.format ?? "epub").toUpperCase()}) from this device`);
    remove.addEventListener("click", () => {
        remove.disabled = true;
        // Removed by hand: no longer pinned, or it would come back as "kept for good".
        void (pinned ? setOfflinePref(b.uuid, "auto") : Promise.resolve())
            .then(() => deleteBook(b.uuid, b.format))
            .finally(() => void render());
    });
    const details = [(b.format ?? "epub").toUpperCase(), size(b.size), ...(pinned ? ["always kept"] : [])].join(" · ");
    return el(
        "li",
        { className: "prefs-book" },
        el(
            "div",
            { className: "prefs-book-text" },
            el("a", { className: "prefs-book-title", href: `/book/get/${encodeURIComponent(b.uuid)}`, textContent: b.title || "Untitled" }),
            el("span", { className: "prefs-book-line", textContent: [b.authors, details].filter(Boolean).join(" — ") }),
        ),
        remove,
    );
}

removeAll.addEventListener("click", () => {
    if (!removeAllArmed) {
        removeAllArmed = true;
        removeAll.textContent = "Remove all? Click again to confirm";
        return;
    }
    removeAll.disabled = true;
    void (async () => {
        for (const b of await listBooks()) {
            if (offlinePref(b.uuid) === "keep") await setOfflinePref(b.uuid, "auto");
            await deleteBook(b.uuid, b.format);
        }
    })().finally(() => {
        removeAll.disabled = false;
        void render();
    });
});
window.addEventListener("cf:offline-changed", () => void render());

// Downloads in progress (the top bar's sync button shows them too).
function showDownloads(): void {
    const s = readSync(user);
    const a = s.active;
    if (a && Date.now() - a.at < 10_000) {
        const pct = a.total ? ` ${Math.round((a.got / a.total) * 100)}%` : "";
        deviceStatus.textContent = `Downloading “${a.title}”…${pct}${s.waiting ? `, ${s.waiting} more waiting` : ""}`;
    } else if (s.paused && s.waiting) deviceStatus.textContent = `Downloads paused: ${s.waiting} book${s.waiting === 1 ? "" : "s"} waiting.`;
}
onSyncChange(user, showDownloads);

/* ------------------------------------------------------------------ */
/* Start                                                              */
/* ------------------------------------------------------------------ */

function showSignedIn(): void {
    for (const e of document.querySelectorAll<HTMLElement>(".prefs-signed-in")) e.hidden = user === null;
    byId("prefs-who").textContent = user
        ? `Signed in as ${user}. Your reading format follows your account; the rest is for this device only.`
        : "Saved in this browser. If you make an account here, these come with you; signing in to an existing account uses that account's own.";
}

showSignedIn();
showFormat();
showDevice();
void render();
void meRequest()
    .then(async ({ ok, body }) => {
        if (!ok && body.user !== null) return;
        user = body.user?.username ?? null;
        await prefsSettled(); // nav.ts: the account's, or the guest's for an account just made
        showSignedIn();
        showFormat();
        showDevice();
        if (user) {
            const fresh = await fetchMarks();
            if (fresh) marks = fresh;
        }
        await render();
    })
    .catch(() => {}); // offline: what's known here stands
