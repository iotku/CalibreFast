/**
 * Books downloaded to this device without being opened (book-cache.ts),
 * run from the library pages with books (startBookSync; not admin pages,
 * editors or the category lists):
 *
 *   1. the most recently read keepReadingN Reading books, when the reader
 *      hasn't kept them (a book switched to Reading and left at once, or
 *      read on another device);
 *   2. the first prefetchSavedN books of Discover's "On deck / Saved for
 *      later" row, if this device is set to: the next book of each series
 *      the user finished one of, then the oldest Saved (oldest, so saving
 *      more books later doesn't swap one download for another).
 *
 * One book at a time, in one tab at a time (a Web Lock), shown in the top
 * bar's sync button (sync-state.ts). The user can pause them there. A
 * download cut off (the page left, paused) keeps what arrived and goes on
 * from there next time (HTTP Range with If-Range), so a slow connection
 * never starts a book over. Pinning a book (offline-control.ts) downloads
 * it the same way, at once, paused or not; one cut off is finished here,
 * before the rest (pinDownloads). A download that fails waits before it's
 * tried again (backingOff); a book with no file the readers open, or too
 * big for this device, isn't tried on every page (Skip).
 */

import { isConvertedFormat, readerFormat } from "./formats.js";
import {
    currentUser,
    deletePartial,
    getBook,
    getBookFiles,
    appendPartial,
    getPartial,
    keepReaderPage,
    listBooks,
    listPartials,
    offlinePref,
    pinDownloads,
    startPartial,
    updateBook,
    type KeptFormat,
    type PartialInfo,
} from "./book-cache.js";
import { preferredFormat } from "./last-format.js";
import { localKey } from "./local-keys.js";
import { DECK_ROW_MAX, keepCover, readingSnapshot, refreshOfflineReading, savedListChanged } from "./offline-books.js";
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
import { readSync, SYNC_NOW_EVENT, writeSync, type SyncNowDetail } from "./sync-state.js";

/** "yielded": stopped for the page's own loading (page-busy.ts); goes on from there. */
export type DownloadResult = "ok" | "missing" | "too-big" | "no-room" | "paused" | "yielded";

export interface DownloadOptions {
    onProgress?: (fraction: number) => void;
    /** Automatic (not asked for): stops when paused, skips files over the size limit. */
    auto?: boolean;
    /** Ahead of time (Saved): stop rather than drop anything for room. */
    noEvict?: boolean;
    /** Pinned ("Always keep"): kept whatever its size. */
    pinned?: boolean;
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

interface Shown {
    uuid: string;
    title: string;
    authors: string;
    format: string;
}

/** Tells the top bar of one download's progress, at most every REPORT_MS (each download its own). */
function reporter(book: Shown): (got: number, total: number) => void {
    let at = 0;
    return (got, total) => {
        if (Date.now() - at < REPORT_MS) return;
        at = Date.now();
        writeSync(currentUser(), { active: { ...book, got, total, at } });
    };
}

/** "bytes start-end/size" (Content-Range of a 206); null if it isn't one. */
function contentRange(v: string | null): { start: number; size: number } | null {
    const m = /^bytes (\d+)-(\d+)\/(\d+|\*)$/.exec(v?.trim() ?? "");
    return m ? { start: Number(m[1]), size: m[3] === "*" ? 0 : Number(m[3]) } : null;
}

/**
 * Download a book's file and keep it, going on from a download cut off
 * before when there is one. "missing": the book has no such file.
 *
 * One download of a book at a time, in every tab (a Web Lock per book): a
 * pin and the queue (runJobs) asking for the same book don't both fetch it
 * or write over each other's partial. The second finds it kept.
 */
export async function downloadForOffline(
    uuid: string,
    format: KeptFormat,
    title: string,
    authors: string,
    opts: DownloadOptions = {},
): Promise<DownloadResult> {
    const once = async (): Promise<DownloadResult> => {
        if (await getBook(uuid, format).catch(() => null)) return "ok"; // another download got it first
        return download(uuid, format, title, authors, opts);
    };
    const locked = (): Promise<DownloadResult> =>
        navigator.locks ? navigator.locks.request(`cf-book-download:${uuid}`, once) : once();
    if (opts.auto) return locked(); // the queue clears what it reported (runJobs)
    // Asked for (a pinned book): shown in the top bar while it goes, cleared after, however it ends.
    let ok = false;
    try {
        const result = await locked();
        ok = result === "ok";
        return result;
    } finally {
        writeSync(currentUser(), { active: null, lastResult: { ok, at: Date.now() } });
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
    // A 206 is only the rest of the file kept, from exactly where it stopped.
    const range = res.status === 206 ? contentRange(res.headers.get("content-range")) : null;
    if (res.status === 206 && (!resume || !range || range.start !== resume.got || (range.size && resume.total && range.size !== resume.total))) {
        await res.body.cancel().catch(() => {});
        await deletePartial(uuid, format).catch(() => {});
        throw new Error(`the server sent another part of the file than the rest asked for (${res.headers.get("content-range") ?? "no Content-Range"})`);
    }
    const ranged = resume !== null && range !== null;
    const total = ranged ? range.size || resume.total : Number(res.headers.get("content-length")) || 0;
    const lastModified = ranged ? resume.lastModified : (res.headers.get("last-modified") ?? "");

    if (opts.auto && total) {
        const { used, limit } = await usage();
        const more = total - (part?.got ?? 0); // the part kept is counted already (and replaced, if it starts over)
        const why: DownloadResult | null = !opts.pinned && !fitsBookLimit(total) ? "too-big" : opts.noEvict && limit && used + more > limit ? "no-room" : null;
        if (why) {
            await res.body.cancel().catch(() => {});
            if (why === "too-big") await deletePartial(uuid, format).catch(() => {});
            return why;
        }
    }

    // What arrived: the part kept before (resumed), then a piece per SAVE_EVERY bytes.
    const pieces: Blob[] = ranged ? [resume.bytes] : [];
    let got = ranged ? resume.got : 0;
    // The partial record pieces are added to; none when it can't be resumed safely (no validator), or saving failed.
    let info: PartialInfo | null = null;
    if (ranged) info = resume;
    else if (lastModified) info = await startPartial({ uuid, format, user, title, authors, total, lastModified }).catch(() => null);
    else if (part) await deletePartial(uuid, format).catch(() => {}); // starting over, and this one can't be kept
    let pending: Uint8Array[] = [];
    let pendingBytes = 0;
    const flush = async (): Promise<void> => {
        if (!pendingBytes) return;
        const piece = new Blob(pending as BlobPart[]);
        pieces.push(piece);
        pending = [];
        pendingBytes = 0;
        if (!info) return;
        try {
            info = await appendPartial(info, piece);
        } catch (err) {
            info = null;
            console.warn(`book-sync: couldn't keep the part of "${title}" downloaded so far (storage full, or blocked); it goes on from what was kept before:`, err);
        }
    };
    const report = reporter({ uuid, title, authors, format });
    const reader = res.body.getReader();
    const stop = async (why: "paused" | "yielded"): Promise<DownloadResult> => {
        await reader.cancel().catch(() => {});
        await flush();
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
        let chunk: ReadableStreamReadResult<Uint8Array>;
        try {
            chunk = await reader.read();
        } catch (err) {
            await flush(); // cut off: keep what arrived, to go on from next time
            throw err;
        }
        if (chunk.done) break;
        pending.push(chunk.value);
        pendingBytes += chunk.value.length;
        got += chunk.value.length;
        if (total) opts.onProgress?.(got / total);
        report(got, total);
        if (pendingBytes >= SAVE_EVERY) await flush();
    }
    // The connection closed without an error before the whole file came (a
    // proxy, a server restart): never keep a short file as the book.
    if (total && got !== total) {
        if (got < total) await flush();
        else await deletePartial(uuid, format).catch(() => {}); // more than the file: what's kept is wrong
        throw new Error(`the download ended at ${got} of ${total} bytes`);
    }
    const had = await getBook(uuid, format).catch(() => null);
    await storeBook({
        uuid,
        format,
        user,
        bytes: new Blob([...pieces, ...(pending as BlobPart[])], { type: format === "pdf" ? "application/pdf" : "application/epub+zip" }),
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
    status: "reading" | "wanted" | "pinned";
    /** The status a pinned book was pinned with. */
    statusSeen?: string;
    savedAt?: number;
}

/* ------------------------------------------------------------------ */
/* Downloads that failed: tried again later, less often each time      */
/* ------------------------------------------------------------------ */

type Failures = Record<string, { n: number; at: number }>;
const RETRY_FIRST_MS = 60_000;
const RETRY_MAX_MS = 24 * 3600_000;
const failuresKey = (user: string): string => localKey(user, "download-failures");

function readFailures(user: string): Failures {
    try {
        return (JSON.parse(localStorage.getItem(failuresKey(user)) ?? "null") as Failures | null) ?? {};
    } catch {
        return {};
    }
}

function writeFailures(user: string, f: Failures): void {
    try {
        if (Object.keys(f).length) localStorage.setItem(failuresKey(user), JSON.stringify(f));
        else localStorage.removeItem(failuresKey(user));
    } catch {
        /* storage full or blocked: tried again on every page */
    }
}

/** A book whose download failed recently: not tried again yet (1 minute, then 4 times as long each time, up to a day). */
function backingOff(user: string, uuid: string): boolean {
    const f = readFailures(user)[uuid];
    return !!f && Date.now() - f.at < Math.min(RETRY_FIRST_MS * 4 ** (f.n - 1), RETRY_MAX_MS);
}

function noteResult(user: string, uuid: string, failed: boolean): void {
    const all = readFailures(user);
    if (failed) all[uuid] = { n: (all[uuid]?.n ?? 0) + 1, at: Date.now() };
    else if (all[uuid]) delete all[uuid];
    else return;
    writeFailures(user, all);
}

/* ------------------------------------------------------------------ */
/* Downloads that can't be kept: not asked for again on every page     */
/* ------------------------------------------------------------------ */

/**
 * A book with no file the readers open (only CHM, MOBI, …: every format
 * 404s), or one over this device's size limit. Trying again only fetches
 * the same answer, so it's skipped: a book with no file for a week (one
 * may be added or converted), one too big while the limit is no larger
 * than when it was found too big (and for a month at most, in case the
 * file is replaced by a smaller one).
 */
type Skip = { why: "missing" | "too-big"; at: number; limit?: number };
type Skips = Record<string, Skip>;
const SKIP_MISSING_MS = 7 * 24 * 3600_000;
const SKIP_TOO_BIG_MS = 30 * 24 * 3600_000;
const skipsKey = (user: string): string => localKey(user, "download-skips");

function skipLive(s: Skip, maxBytes: number, now = Date.now()): boolean {
    if (s.why === "missing") return now - s.at < SKIP_MISSING_MS;
    // A limit raised (or turned off) since: tried again.
    return now - s.at < SKIP_TOO_BIG_MS && !!maxBytes && maxBytes <= (s.limit ?? 0);
}

function readSkips(user: string): Skips {
    try {
        return (JSON.parse(localStorage.getItem(skipsKey(user)) ?? "null") as Skips | null) ?? {};
    } catch {
        return {};
    }
}

function writeSkips(user: string, all: Skips): void {
    try {
        if (Object.keys(all).length) localStorage.setItem(skipsKey(user), JSON.stringify(all));
        else localStorage.removeItem(skipsKey(user));
    } catch {
        /* storage full or blocked: tried again on every page */
    }
}

/** Jobs not worth trying (see Skip). A pinned book is kept whatever its size, so only a missing file skips it. */
function skipped(user: string, job: Job): boolean {
    const s = readSkips(user)[job.uuid];
    if (!s || (job.status === "pinned" && s.why === "too-big")) return false;
    return skipLive(s, devicePrefs(user).maxBookBytes);
}

/** Remember (or forget) why a book's download can't be kept; entries no longer live are dropped. */
function noteSkip(user: string, uuid: string, why: Skip["why"] | null): void {
    const all = readSkips(user);
    const maxBytes = devicePrefs(user).maxBookBytes;
    let changed = false;
    for (const [u, s] of Object.entries(all)) {
        if (u !== uuid && !skipLive(s, maxBytes)) {
            delete all[u];
            changed = true;
        }
    }
    if (why) {
        all[uuid] = { why, at: Date.now(), ...(why === "too-big" ? { limit: maxBytes } : {}) };
        changed = true;
    } else if (all[uuid]) {
        delete all[uuid];
        changed = true;
    }
    if (changed) writeSkips(user, all);
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

/**
 * The start of On deck and Saved: the first N to download (n), and at least
 * as many as Discover's row for the offline page (offline-books.ts), in one
 * request. Fetched again at most every SAVED_REFRESH_MS (unchanged: an
 * empty 304). The whole list is kept (retention.ts slices it to N).
 */
async function savedToFetch(user: string, n: number, force: boolean): Promise<SavedBook[] | null> {
    const last = savedList(user);
    const limit = Math.max(DECK_ROW_MAX, n);
    if (!force && last?.limit === limit && Date.now() - last.at < SAVED_REFRESH_MS) return last.books;
    const tag = last?.limit === limit ? last.tag : undefined;
    const res = await quietFetch(`/api/me/saved-books?limit=${limit}`, {
        credentials: "same-origin",
        headers: { accept: "application/json", ...(tag ? { "if-none-match": tag } : {}) },
    }).catch(() => null);
    if (!res) return last?.books ?? null; // offline
    let books: SavedBook[];
    if (res.status === 304 && last) books = last.books;
    else if (!res.ok || !(res.headers.get("content-type") ?? "").includes("json")) return last?.books ?? null;
    else books = (await res.json()) as SavedBook[];
    writeSavedList(user, { at: Date.now(), books, limit, tag: res.headers.get("etag") ?? tag ?? "" });
    if (res.status !== 304) await savedListChanged(user).catch(() => {});
    return books;
}

/** Download the jobs, one at a time, in one tab at a time. */
async function runJobs(user: string, jobs: Job[]): Promise<void> {
    const run = async (): Promise<void> => {
        let left = jobs.length;
        let savedStopped = false;
        let anyFailed = false;
        for (const [i, job] of jobs.entries()) {
            if (paused() || currentUser() !== user) break;
            left--;
            if (savedStopped && job.status === "wanted") continue;
            if (backingOff(user, job.uuid)) continue;
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
                writeSync(user, { active: { uuid: job.uuid, title: job.title, authors: job.authors, format: job.formats[0] ?? "epub", got: 0, total: 0, at: Date.now() } });
                for (const f of job.formats) {
                    try {
                        result = await downloadForOffline(job.uuid, f, job.title, job.authors, {
                            auto: true,
                            noEvict: job.status === "wanted",
                            pinned: job.status === "pinned",
                            savedAt: job.savedAt,
                            statusSeen: job.status === "pinned" ? job.statusSeen : job.status,
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
            // Offline, a failure says nothing about the book: not held back for it.
            if (result === "failed" ? navigator.onLine : result !== "paused" && result !== "yielded") noteResult(user, job.uuid, result === "failed");
            if (result === "ok" || result === "missing" || result === "too-big") noteSkip(user, job.uuid, result === "ok" ? null : result);
            if (result === "ok") void keepCover(job.uuid).catch(() => {});
            if (result === "paused") {
                left++; // that one is still to come
                break;
            }
            if (result === "failed" || result === "no-room") anyFailed = true;
            if (result === "no-room") savedStopped = true; // no room for Saved books without dropping others
        }
        writeSync(user, {
            active: null,
            held: false,
            waiting: paused() ? left : 0,
            next: [],
            // Paused part way: nothing to say yet.
            ...(paused() ? {} : { lastResult: { ok: !anyFailed, at: Date.now() } }),
        });
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
    /** Tidy kept books with statuses fetched now (a run asked for with marks that are no longer fresh). */
    fresh?: boolean;
}

/** Download what this device should have (see the top), then tidy up. One run at a time. */
export function syncBooks(opts: SyncOptions = {}): Promise<void> {
    if (running) {
        // Not the marks themselves: by the time this runs (after a download
        // or a few) they may be out of date, and tidying by an old status
        // could drop a book started since. Fetched fresh then instead.
        again = { force: !!(again?.force || opts.force), fresh: !!(again?.fresh || opts.fresh || opts.marks) };
        return running;
    }
    running = runSync(opts)
        .catch((err: unknown) => console.warn("book-sync: the download run stopped; it's tried again on a later page:", err))
        .finally(() => {
            running = null;
            const next = again;
            again = null;
            if (next) void syncBooks(next);
        });
    return running;
}

async function runSync({ marks, force = false, fresh = false }: SyncOptions): Promise<void> {
    const user = currentUser();
    if (user === null) return;
    const prefs = devicePrefs(user);
    const m = marks ?? cachedMarks();
    // The Reading list, newest first (and the offline page's lists, offline-books.ts):
    // at most every 10 minutes, and an empty 304 when unchanged.
    await refreshOfflineReading().catch(() => {});
    const keptBooks = await listBooks().catch(() => []);
    const kept = new Map(keptBooks.map((b) => [b.uuid, b]));
    const jobs: Job[] = [];

    // Pinned books not here yet (a pin's download cut off): first, asked for as they were.
    for (const p of pinDownloads()) {
        if (kept.has(p.uuid) || offlinePref(p.uuid) !== "keep") continue;
        const formats: KeptFormat[] = p.format === "pdf" ? ["pdf", "epub"] : ["epub", "pdf"];
        jobs.push({ uuid: p.uuid, title: p.title, authors: p.authors, formats, status: "pinned", ...(p.status ? { statusSeen: p.status } : {}) });
    }
    const queued = (uuid: string): boolean => jobs.some((j) => j.uuid === uuid);
    if (prefs.readingKeepDays !== -1 && prefs.keepReadingN) {
        for (const b of (await readingOrder(m)).slice(0, prefs.keepReadingN)) {
            if (kept.has(b.uuid) || queued(b.uuid) || offlinePref(b.uuid) === "never") continue;
            // Read converted from a Kindle format (formats.ts): no file is kept for it.
            if (isConvertedFormat(readerFormat(m.resume.get(b.uuid) ?? ""))) continue;
            jobs.push({ uuid: b.uuid, title: b.title, authors: b.authors, formats: formatsFor(b.uuid), status: "reading" });
        }
    }
    const saved = await savedToFetch(user, prefs.prefetchSavedN, force);
    for (const b of (saved ?? []).slice(0, prefs.prefetchSavedN)) {
        const have = kept.get(b.uuid);
        if (have) {
            if (have.savedAt !== b.saved_at) await updateBook(b.uuid, have.format ?? "epub", { savedAt: b.saved_at }).catch(() => {});
            continue;
        }
        if (queued(b.uuid) || offlinePref(b.uuid) === "never") continue;
        jobs.push({ uuid: b.uuid, title: b.title, authors: b.authors, formats: formatsFor(b.uuid), status: "wanted", savedAt: b.saved_at });
    }

    // Downloads cut off for books no longer wanted here (out of Reading or
    // Saved, a smaller count, never) would hold their space for good: drop
    // them. A pinned book's goes on (it's queued above).
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
    } else if (force || fresh) await sweepFresh();

    // Books found to have no file to keep, or too big: not tried on every page (Skip).
    const todo = jobs.filter((j) => !skipped(user, j));
    if (todo.length && !paused()) await runJobs(user, todo);
    else writeSync(user, { active: null, waiting: todo.length && paused() ? todo.length : 0 });
    if (!pagesChecked() && navigator.onLine) {
        // Once a visit: books kept before their popups (or reader pages) were
        // kept with them get theirs (the worker skips those it has).
        markPagesChecked();
        for (const b of await listBooks().catch(() => [])) {
            keepReaderPage(b.uuid, b.format ?? "epub");
            await keepCover(b.uuid).catch(() => {});
        }
    }
}

/** Once a browser session, not every page load (sessionStorage; every page when it's blocked). */
const PAGES_CHECKED = "cf-book-pages-checked";
let pagesCheckedHere = false;
function pagesChecked(): boolean {
    try {
        return pagesCheckedHere || sessionStorage.getItem(PAGES_CHECKED) === "1";
    } catch {
        return pagesCheckedHere;
    }
}
function markPagesChecked(): void {
    pagesCheckedHere = true;
    try {
        sessionStorage.setItem(PAGES_CHECKED, "1");
    } catch {
        /* blocked */
    }
}


/* ------------------------------------------------------------------ */
/* On library pages                                                   */
/* ------------------------------------------------------------------ */

let started = false;

/**
 * A library page: download what's missing once the page has settled, and
 * again when nav.ts asks (SYNC_NOW_EVENT: Sync now, downloads resumed, the
 * connection back). marks: the page's fresh reading marks, if it has them
 * (the grid).
 */
export function startBookSync(marks?: Marks): void {
    const go = (opts: SyncOptions = {}): void => void syncBooks(opts);
    if (!started) {
        started = true;
        watchPage(); // downloads give way to this page's own loading (page-busy.ts)
        document.addEventListener(SYNC_NOW_EVENT, (e) => {
            e.preventDefault(); // this page's run takes it (nav.ts loads none)
            const { force, done } = (e as CustomEvent<SyncNowDetail>).detail;
            done(syncBooks({ force }));
        });
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
