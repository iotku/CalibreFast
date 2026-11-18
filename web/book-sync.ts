/**
 * Books downloaded to this device without being opened (book-cache.ts),
 * run from the library pages (startBookSync; not admin pages or editors):
 *
 *   1. the most recently read keepReadingN Reading books, when the reader
 *      hasn't kept them (a book switched to Reading and left at once, or
 *      read on another device);
 *   2. the oldest prefetchSavedN Saved books, if this device is set to
 *      (oldest, so saving more books later doesn't swap one download for
 *      another).
 *
 * One book at a time, in one tab at a time (a Web Lock), shown in the top
 * bar's sync button (sync-state.ts). The user can pause them there. A
 * download cut off (the page left, paused) keeps what arrived and goes on
 * from there next time (HTTP Range with If-Range), so a slow connection
 * never starts a book over. Pinning a book (offline-control.ts) downloads
 * it the same way, at once, paused or not.
 */

import {
    currentUser,
    deletePartial,
    getBook,
    getPartial,
    listBooks,
    offlinePref,
    putPartial,
    updateBook,
    type KeptFormat,
} from "./book-cache.js";
import { preferredFormat } from "./last-format.js";
import { keepCover, readingSnapshot, refreshOfflineReading } from "./offline-books.js";
import { devicePrefs, formatPref } from "./prefs.js";
import { cachedMarks, statusOf, type Marks } from "./reading.js";
import {
    fitsBookLimit,
    SAVED_REFRESH_MS,
    savedList,
    storeBook,
    sweep,
    sweepFresh,
    usage,
    writeSavedList,
    type SavedBook,
} from "./retention.js";
import { onSyncChange, readSync, writeSync } from "./sync-state.js";

export type DownloadResult = "ok" | "missing" | "too-big" | "no-room" | "paused";

export interface DownloadOptions {
    onProgress?: (fraction: number) => void;
    /** Automatic (not asked for): stops when paused, skips files over the size limit. */
    auto?: boolean;
    /** Ahead of time (Saved): stop rather than drop anything for room. */
    noEvict?: boolean;
    savedAt?: number;
    statusSeen?: string;
}

/** What arrived is saved this often (bytes), to go on from there if cut off. */
const SAVE_EVERY = 1 << 20;
/** The top bar is told of progress at most this often. */
const REPORT_MS = 500;

const paused = (): boolean => !!readSync(currentUser()).paused;

let reportedAt = 0;
function report(title: string, got: number, total: number, force = false): void {
    if (!force && Date.now() - reportedAt < REPORT_MS) return;
    reportedAt = Date.now();
    writeSync(currentUser(), { active: { title, got, total, at: reportedAt } });
}

/**
 * Download a book's file and keep it, going on from a download cut off
 * before when there is one. "missing": the book has no such file.
 */
export async function downloadForOffline(
    uuid: string,
    format: KeptFormat,
    title: string,
    authors: string,
    opts: DownloadOptions = {},
): Promise<DownloadResult> {
    const user = currentUser();
    const part = await getPartial(uuid, format).catch(() => null);
    // Only with a validator: the rest of the same file, or (200) the whole new one.
    const resume = part?.lastModified ? part : null;
    const res = await fetch(`/download/${encodeURIComponent(uuid)}/${format}`, {
        credentials: "same-origin",
        headers: resume ? { range: `bytes=${resume.got}-`, "if-range": resume.lastModified } : {},
    });
    if (res.status === 404) return "missing";
    if (res.status === 416 && resume) {
        await deletePartial(uuid, format); // what was kept doesn't fit the file any more: from the start next time
        throw new Error("the kept part no longer matches the file");
    }
    if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
    const ranged = res.status === 206 && resume !== null;
    let kept: Blob = ranged ? resume.bytes : new Blob([]);
    let got = ranged ? resume.got : 0;
    const total = ranged ? resume.total : Number(res.headers.get("content-length")) || 0;
    const lastModified = ranged ? resume.lastModified : (res.headers.get("last-modified") ?? "");

    if (opts.auto && total) {
        const { used, limit } = await usage();
        const more = total - (resume?.got ?? 0); // the part kept is counted already
        const why: DownloadResult | null = !fitsBookLimit(total) ? "too-big" : opts.noEvict && limit && used + more > limit ? "no-room" : null;
        if (why) {
            await res.body.cancel().catch(() => {});
            if (why === "too-big") await deletePartial(uuid, format).catch(() => {});
            return why;
        }
    }

    const savePart = async (): Promise<void> => {
        if (!lastModified) return; // couldn't be resumed safely: nothing to keep
        await putPartial({ uuid, format, user, bytes: kept, got, total, lastModified }).catch((err: unknown) =>
            console.warn(`book-sync: couldn't keep the part of "${title}" downloaded so far (storage full, or blocked); it starts over next time:`, err),
        );
    };
    let pending: Uint8Array[] = [];
    let pendingBytes = 0;
    const reader = res.body.getReader();
    for (;;) {
        if (opts.auto && paused()) {
            await reader.cancel().catch(() => {});
            kept = new Blob([kept, ...(pending as BlobPart[])]);
            await savePart();
            return "paused";
        }
        const { done, value } = await reader.read();
        if (done) break;
        pending.push(value);
        pendingBytes += value.length;
        got += value.length;
        if (total) opts.onProgress?.(got / total);
        report(title, got, total);
        if (pendingBytes >= SAVE_EVERY) {
            kept = new Blob([kept, ...(pending as BlobPart[])]);
            pending = [];
            pendingBytes = 0;
            await savePart();
        }
    }
    const had = await getBook(uuid, format).catch(() => null);
    await storeBook({
        uuid,
        format,
        user,
        bytes: new Blob([kept, ...(pending as BlobPart[])], { type: format === "pdf" ? "application/pdf" : "application/epub+zip" }),
        lastModified,
        size: got,
        title,
        authors,
        locations: had?.locations,
        // Not opened (yet): retention.ts tells it from a book read here.
        lastOpened: had?.lastOpened ?? (opts.auto ? 0 : Date.now()),
        statusSeen: opts.statusSeen ?? had?.statusSeen,
        savedAt: opts.savedAt ?? had?.savedAt,
    });
    return "ok";
}

/* ------------------------------------------------------------------ */
/* What to download                                                   */
/* ------------------------------------------------------------------ */

interface Job {
    uuid: string;
    title: string;
    authors: string;
    formats: KeptFormat[];
    status: "reading" | "wanted";
    savedAt?: number;
}

/** The formats to try for a book, the one it's read in (or the preferred) first. */
function formatsFor(uuid: string): KeptFormat[] {
    const first = preferredFormat(uuid, ["epub", "pdf"]) ?? (formatPref() === "pdf" ? "pdf" : "epub");
    return first === "pdf" ? ["pdf", "epub"] : ["epub", "pdf"];
}

/**
 * The most recently read Reading books: the saved Reading list (offline-
 * books.ts, newest first), after any the marks say are being read that it
 * doesn't have yet (just switched to Reading: the newest of all).
 */
function readingOrder(marks: Marks): { uuid: string; title: string; authors: string }[] {
    const listed = readingSnapshot()?.books ?? [];
    const known = new Set(listed.map((b) => b.uuid));
    const fresh = [...marks.reading.keys()].filter((u) => !known.has(u)).map((uuid) => ({ uuid, title: "", authors: "" }));
    const reading = (u: string): boolean => !marks.signedIn || marks.reading.has(u);
    return [...fresh, ...listed.filter((b) => reading(b.uuid))];
}

/** The oldest N Saved books, fetched again at most every SAVED_REFRESH_MS (unchanged: an empty 304). */
async function savedToFetch(user: string, n: number, force: boolean): Promise<SavedBook[] | null> {
    const last = savedList(user);
    if (!n) {
        if (last?.books.length) writeSavedList(user, { at: Date.now(), books: [] }); // off: none kept
        return [];
    }
    if (!force && last?.limit === n && Date.now() - last.at < SAVED_REFRESH_MS) return last.books;
    const tag = last?.limit === n ? last.tag : undefined;
    const res = await fetch(`/api/me/saved-books?limit=${n}`, {
        credentials: "same-origin",
        headers: { accept: "application/json", ...(tag ? { "if-none-match": tag } : {}) },
    }).catch(() => null);
    if (!res) return last?.books ?? null; // offline
    let books: SavedBook[];
    if (res.status === 304 && last) books = last.books;
    else if (!res.ok || !(res.headers.get("content-type") ?? "").includes("json")) return last?.books ?? null;
    else books = (await res.json()) as SavedBook[];
    writeSavedList(user, { at: Date.now(), books, limit: n, tag: res.headers.get("etag") ?? tag ?? "" });
    return books;
}

/** Download the jobs, one at a time, in one tab at a time. */
async function runJobs(user: string, jobs: Job[]): Promise<void> {
    const run = async (): Promise<void> => {
        let left = jobs.length;
        let savedStopped = false;
        for (const job of jobs) {
            if (paused() || currentUser() !== user) break;
            left--;
            if (savedStopped && job.status === "wanted") continue;
            writeSync(user, { waiting: left });
            report(job.title, 0, 0, true);
            let result: DownloadResult | "failed" = "missing";
            for (const f of job.formats) {
                try {
                    result = await downloadForOffline(job.uuid, f, job.title, job.authors, {
                        auto: true,
                        noEvict: job.status === "wanted",
                        savedAt: job.savedAt,
                        statusSeen: job.status,
                    });
                } catch (err) {
                    console.warn(`book-sync: downloading "${job.title}" failed; it's tried again on a later page:`, err);
                    result = "failed";
                    break;
                }
                if (result !== "missing") break;
            }
            if (result === "ok") void keepCover(job.uuid).catch(() => {});
            if (result === "paused") {
                left++; // that one is still to come
                break;
            }
            if (result === "no-room") savedStopped = true; // no room for Saved books without dropping others
        }
        writeSync(user, { active: null, waiting: paused() ? left : 0 });
    };
    // Another tab downloading: it does these too.
    if (navigator.locks) await navigator.locks.request("cf-book-downloads", { ifAvailable: true }, async (lock) => (lock ? run() : undefined));
    else await run();
}

let running: Promise<void> | null = null;

export interface SyncOptions {
    /** The grid's marks, just fetched: also tidy kept books (retention.ts) by them. */
    marks?: Marks;
    /** Fetch the Saved list now, however recently it was (a book just saved, the setting changed). */
    force?: boolean;
}

/** Download what this device should have (see the top), then tidy up. One run at a time. */
export function syncBooks(opts: SyncOptions = {}): Promise<void> {
    running ??= runSync(opts).finally(() => (running = null));
    return running;
}

async function runSync({ marks, force = false }: SyncOptions): Promise<void> {
    const user = currentUser();
    if (user === null) return;
    const prefs = devicePrefs(user);
    const m = marks ?? cachedMarks();
    // The Reading list, newest first: at most every 10 minutes, and an empty 304 when unchanged.
    if (prefs.keepReadingN) await refreshOfflineReading().catch(() => {});
    const kept = new Set((await listBooks().catch(() => [])).map((b) => b.uuid));
    const jobs: Job[] = [];

    if (prefs.readingKeepDays !== -1 && prefs.keepReadingN) {
        for (const b of readingOrder(m).slice(0, prefs.keepReadingN)) {
            if (kept.has(b.uuid) || offlinePref(b.uuid) === "never") continue;
            jobs.push({ ...b, title: b.title || "a book you're reading", formats: formatsFor(b.uuid), status: "reading" });
        }
    }
    const saved = await savedToFetch(user, prefs.prefetchSavedN, force);
    for (const b of saved ?? []) {
        if (kept.has(b.uuid)) {
            const have = (await listBooks()).find((k) => k.uuid === b.uuid);
            if (have && have.savedAt !== b.saved_at) await updateBook(b.uuid, have.format ?? "epub", { savedAt: b.saved_at });
            continue;
        }
        if (offlinePref(b.uuid) === "never") continue;
        jobs.push({ uuid: b.uuid, title: b.title, authors: b.authors, formats: formatsFor(b.uuid), status: "wanted", savedAt: b.saved_at });
    }

    // Tidy up first (it may free room), only with statuses fresh from the
    // server: the grid's, or asked for when forced.
    if (marks?.signedIn) {
        // The marks may predate a book saved since: the list just fetched says it's Saved.
        const listed = new Set((saved ?? []).map((b) => b.uuid));
        await sweep((u) => (listed.has(u) ? "wanted" : statusOf(marks, u)));
    } else if (force) await sweepFresh();

    if (jobs.length && !paused()) await runJobs(user, jobs);
    else writeSync(user, { active: null, waiting: jobs.length && paused() ? jobs.length : 0 });
}

/* ------------------------------------------------------------------ */
/* On library pages                                                   */
/* ------------------------------------------------------------------ */

let started = false;

/**
 * A library page: download what's missing once the page has settled, and
 * again when downloads are resumed or the connection comes back. marks:
 * the page's fresh reading marks, if it has them (the grid).
 */
export function startBookSync(marks?: Marks): void {
    const go = (opts: SyncOptions = {}): void => void syncBooks(opts).catch(() => {});
    if (!started) {
        started = true;
        let wasPaused = paused();
        onSyncChange(currentUser(), () => {
            const now = paused();
            if (wasPaused && !now) go();
            wasPaused = now;
        });
        window.addEventListener("online", () => go());
    }
    if (marks) {
        go({ marks }); // tidying with fresh statuses shouldn't wait; downloads go one at a time anyway
        return;
    }
    // After the page's own requests (covers, the next page of books).
    const idle = (window as { requestIdleCallback?: (cb: () => void, o?: { timeout: number }) => void }).requestIdleCallback;
    if (idle) idle(() => go(), { timeout: 5000 });
    else setTimeout(() => go(), 1500);
}
