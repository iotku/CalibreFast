/**
 * What a signed-out visitor has read on this device (docs/notes/Discover.md,
 * section 5a): an index of the guest's positions, so the Discover page can
 * show "Continue reading on this device" without scanning every key.
 *
 * The positions themselves stay where the readers keep them
 * (cf-progress-<uuid>-<format>, guest records: local-keys.ts). The readers
 * update this index as they save a guest position; the first time it's
 * read, it's built from the positions already there.
 */

export type LocalFormat = "epub" | "pdf";

export interface LocalReadingEntry {
    format: LocalFormat;
    percent: number; // 0..1
    updatedAt: number; // ms
}

/** At or past this, a book counts as finished on this device. */
export const LOCAL_FINISHED = 0.98;

const KEY = "cf-local-reading";
const PROGRESS = /^cf-progress-(.+)-(epub|pdf)$/;

type Index = Record<string, LocalReadingEntry>;

function save(index: Index): void {
    try {
        localStorage.setItem(KEY, JSON.stringify(index));
    } catch {
        /* storage full or blocked */
    }
}

/** Builds the index from the guest positions already saved. */
function build(): Index {
    const index: Index = {};
    try {
        for (const key of Object.keys(localStorage)) {
            const m = PROGRESS.exec(key);
            if (!m) continue;
            const v = JSON.parse(localStorage.getItem(key) ?? "null") as { percent?: number; updatedAt?: number } | null;
            if (!v) continue;
            const entry: LocalReadingEntry = { format: m[2] as LocalFormat, percent: v.percent ?? 0, updatedAt: v.updatedAt ?? 0 };
            const cur = index[m[1]!];
            if (!cur || entry.updatedAt > cur.updatedAt) index[m[1]!] = entry;
        }
    } catch {
        /* blocked or unreadable: what was read so far */
    }
    save(index);
    return index;
}

/** The guest's books read on this device: uuid -> latest position. */
export function localReading(): Index {
    try {
        const v = localStorage.getItem(KEY);
        if (v !== null) return (JSON.parse(v) as Index | null) ?? {};
    } catch {
        return {};
    }
    return build();
}

/** A guest position was saved (reading-sync.ts, signed out). */
export function noteLocalReading(uuid: string, format: LocalFormat, percent: number, updatedAt: number): void {
    const index = localReading();
    const cur = index[uuid];
    if (cur && cur.updatedAt > updatedAt) return;
    index[uuid] = { format, percent, updatedAt };
    save(index);
}

/** Books no longer in the library. */
export function forgetLocalReading(uuids: string[]): void {
    if (!uuids.length) return;
    const index = localReading();
    for (const u of uuids) delete index[u];
    save(index);
}
