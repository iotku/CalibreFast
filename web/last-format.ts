/**
 * Which format of a book to open by default: the one last read.
 *
 * A book with both an EPUB and a PDF opens (its cover on the book's page and
 * popup, templates/book.html) in whichever the reader last chose. What this
 * device knows comes first:
 *   - the format last opened here (the readers note it, noteFormatOpened);
 *   - else the newer of the positions kept here for each format (books read
 *     before this was noted);
 *   - else where the server says to resume (the Reading marks, reading.ts:
 *     the format last read on any device).
 * With none of these, the page's own choice (EPUB) stands.
 *
 * Kept in localStorage under the reader's name (local-keys.ts), like the
 * positions, so a guest's choices aren't another user's.
 */

import type { KeptFormat } from "./book-cache.js";
import { lastKnownUser, localKey, type Owner } from "./local-keys.js";
import { cachedMarks } from "./reading.js";

/** uuid -> the format last opened and when (ms). */
type Opened = Record<string, { f: KeptFormat; at: number }>;

/** Books remembered, at most (the oldest go first). */
const MAX_BOOKS = 500;

const FORMATS: readonly KeptFormat[] = ["epub", "pdf"];

function read<T>(key: string): T | null {
    try {
        return JSON.parse(localStorage.getItem(key) ?? "null") as T | null;
    } catch {
        return null;
    }
}

/** The readers, as a book opens (not for a preview). */
export function noteFormatOpened(owner: Owner, uuid: string, format: KeptFormat): void {
    const key = localKey(owner, "last-format");
    const opened = read<Opened>(key) ?? {};
    opened[uuid] = { f: format, at: Date.now() };
    const entries = Object.entries(opened);
    const kept = entries.length > MAX_BOOKS ? entries.sort((a, b) => b[1].at - a[1].at).slice(0, MAX_BOOKS) : entries;
    try {
        localStorage.setItem(key, JSON.stringify(Object.fromEntries(kept)));
    } catch {
        /* storage full or blocked */
    }
}

/** The format to open the book in, among those it has; null to leave the page's choice. */
export function preferredFormat(uuid: string, has: readonly string[], owner: Owner = lastKnownUser()): KeptFormat | null {
    const available = FORMATS.filter((f) => has.includes(f));
    if (available.length < 2) return null;

    const opened = read<Opened>(localKey(owner, "last-format"))?.[uuid]?.f;
    if (opened && available.includes(opened)) return opened;

    let newest: { f: KeptFormat; at: number } | null = null;
    for (const f of available) {
        const at = read<{ updatedAt?: number }>(localKey(owner, `progress-${uuid}-${f}`))?.updatedAt ?? 0;
        if (at > (newest?.at ?? 0)) newest = { f, at };
    }
    if (newest) return newest.f;

    const resume = cachedMarks().resume.get(uuid);
    if (resume?.startsWith("/read-pdf?")) return "pdf";
    if (resume?.startsWith("/read?")) return "epub";
    return null;
}
