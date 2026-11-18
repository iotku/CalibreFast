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
    getBookFiles,
    getPartial,
    listBooks,
    listPartials,
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
import { pageBusy, quietFetch, untilQuiet, watchPage } from "./page-busy.js";
import { onSyncChange, readSync, writeSync } from "./sync-state.js";

/** "yielded": stopped for the page's own loading (page-busy.ts); goes on from there. */
export type DownloadResult = "ok" | "missing" | "too-big" | "no-room" | "paused" | "yielded";

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
/**
 * A page busy loading (covers, a popup): the download stops reading, and
 * the connection soon stops sending. Still busy after this long, it's cut
 * off (what arrived is kept) to free the connection, and goes on later.
 */
const HOLD_MS = 3000;
const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
/** The top bar is told of progress at most this often. */
const REPORT_MS = 500;

const paused = (): boolean => !!readSync(currentUser()).paused;

let reportedAt = 0;
function report(book: { uuid: string; title: string; authors: string; format: string }, got: number, total: number, force = false): void {
    if (!force && Date.now() - reportedAt < REPORT_MS) return;
    reportedAt = Date.now();
    writeSync(currentUser(), { active: { ...book, got, total, at: reportedAt } });
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
    if (opts.auto) return download(uuid, format, title, authors, opts); // the queue clears what it reported (runJobs)
    // Asked for (a pinned book): shown in the top bar while it goes, cleared after, however it ends.
    try {
        return await download(uuid, format, title, authors, opts);
    } finally {
        writeSync(currentUser(), { active: null });
    }
}

async function download(uuid: string, format: KeptFormat, title: string, authors: string, opts: DownloadOptions): Promise<DownloadResult> {
    const user = currentUser();
    const part = await getPartial(uuid, format).catch(() => null);
    // Only with a validator: the rest of the same file, or (200) the whole new one.
    const resume = part?.lastModified ? part : null;
    // Low priority, and not counted as the page's own loading (page-busy.ts).
    const res = await quietFetch(`/download/${encodeURIComponent(uuid)}/${format}`, {
        credentials: "same-origin",
        headers: resume ? { range: `bytes=${resume.got}-`, "if-range": resume.lastModified } : {},
        ...(opts.auto ? { priority: "low" } : {}),
    } as RequestInit);
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
    const stop = async (why: "paused" | "yielded"): Promise<DownloadResult> => {
        await reader.cancel().catch(() => {});
        kept = new Blob([kept, ...(pending as BlobPart[])]);
        pending = [];
        await savePart();
        return why;
    };
    for (;;) {
        if (opts.auto && paused()) return stop("paused");
        // The page is loading what the reader waits for: hold off, and if it
        // goes on, let go of the connection.
        if (opts.auto && pageBusy()) {
            const since = Date.now();
            while (pageBusy() && !paused() && Date.now() - since < HOLD_MS) await sleep(200);
            if (paused()) return stop("paused");
            if (pageBusy()) return stop("yielded");
        }
        const { done, value } = await reader.read();
        if (done) break;
        pending.push(value);
        pendingBytes += value.length;
        got += value.length;
        if (total) opts.onProgress?.(got / total);
        report({ uuid, title, authors, format }, got, total);
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
 * The most recently read Reading books, as the saved Reading list (offline-
 * books.ts) has them: newest first, with titles and authors. If the marks
 * say a book is being read that the list doesn't have yet (just switched
 * to Reading), the list is fetched again first (cheap: tagged, 304 when
 * unchanged); one it still lacks waits for a later page.
 */
async function readingOrder(marks: Marks): Promise<{ uuid: string; title: string; authors: string }[]> {
    const lacks = (): boolean => {
        const known = new Set((readingSnapshot()?.books ?? []).map((b) => b.uuid));
        return [...marks.reading.keys()].some((u) => !known.has(u));
    };
    if (marks.signedIn && lacks()) await refreshOfflineReading(true).catch(() => {});
    const reading = (u: string): boolean => !marks.signedIn || marks.reading.has(u);
    return (readingSnapshot()?.books ?? []).filter((b) => reading(b.uuid));
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
    const res = await quietFetch(`/api/me/saved-books?limit=${n}`, {
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
        for (const [i, job] of jobs.entries()) {
            if (paused() || currentUser() !== user) break;
            left--;
            if (savedStopped && job.status === "wanted") continue;
            // Another tab may have got it while this one waited for its turn.
            if ((await getBookFiles(job.uuid).catch(() => [])).length) continue;
            const next = jobs.slice(i + 1, i + 4).map((j) => j.title);
            writeSync(user, { waiting: left, next });
            let result: DownloadResult | "failed" = "missing";
            // Waits for quiet pages first, and again each time it gives way to one.
            for (let tries = 0; tries < 50; tries++) {
                if (pageBusy()) writeSync(user, { held: true, active: null, waiting: left + 1 }); // this one too
                await untilQuiet(paused);
                writeSync(user, { held: false, waiting: left });
                if (paused()) {
                    result = "paused";
                    break;
                }
                report({ uuid: job.uuid, title: job.title, authors: job.authors, format: job.formats[0] ?? "epub" }, 0, 0, true);
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
                if (result !== "yielded") break;
            }
            if (result === "ok") void keepCover(job.uuid).catch(() => {});
            if (result === "paused") {
                left++; // that one is still to come
                break;
            }
            if (result === "no-room") savedStopped = true; // no room for Saved books without dropping others
        }
        writeSync(user, { active: null, held: false, waiting: paused() ? left : 0, next: [] });
    };
    // One tab at a time. The others wait their turn (not skip): if the tab
    // downloading closes, the next takes over, finding done what's done.
    if (navigator.locks) await navigator.locks.request("cf-book-downloads", run);
    else await run();
}

let running: Promise<void> | null = null;
/** Asked for while a run was going (a book just saved): run again after it. */
let again: SyncOptions | null = null;

export interface SyncOptions {
    /** The grid's marks, just fetched: also tidy kept books (retention.ts) by them. */
    marks?: Marks;
    /** Fetch the Saved list now, however recently it was (a book just saved, the setting changed). */
    force?: boolean;
}

/** Download what this device should have (see the top), then tidy up. One run at a time. */
export function syncBooks(opts: SyncOptions = {}): Promise<void> {
    if (running) {
        again = { ...again, ...opts, force: !!(again?.force || opts.force) };
        return running;
    }
    running = runSync(opts).finally(() => {
        running = null;
        const next = again;
        again = null;
        if (next) void syncBooks(next).catch(() => {});
    });
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
        for (const b of (await readingOrder(m)).slice(0, prefs.keepReadingN)) {
            if (kept.has(b.uuid) || offlinePref(b.uuid) === "never") continue;
            jobs.push({ uuid: b.uuid, title: b.title, authors: b.authors, formats: formatsFor(b.uuid), status: "reading" });
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

    // Downloads cut off for books no longer wanted here (out of Reading or
    // Saved, a smaller count, never) would hold their space for good: drop
    // them. A pinned book's (offline-control.ts) goes on next time it's pinned.
    // Only when both lists are known (offline, the Saved list may not be).
    const listsKnown = saved !== null && (!prefs.keepReadingN || readingSnapshot() !== null);
    const wanted = new Set(jobs.map((j) => j.uuid));
    for (const p of listsKnown ? await listPartials().catch(() => []) : []) {
        if (!wanted.has(p.uuid) && offlinePref(p.uuid) !== "keep") await deletePartial(p.uuid, p.format).catch(() => {});
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
        watchPage(); // downloads give way to this page's own loading (page-busy.ts)
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
