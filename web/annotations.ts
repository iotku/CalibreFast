/**
 * Highlights and notes for the built-in readers: the data and its sync.
 * (web/annotation-ui.ts is what the reader sees.)
 *
 * Kept in localStorage, so they work offline and when not signed in. When
 * signed in they sync with the server (annotations.go): every change carries
 * this device's clock and the newest wins, and a delete is a change like any
 * other (a tombstone), so it reaches the other devices.
 */

import { api } from "./dom.js";
import type { BookFormat } from "./reading-sync.js";

/** Colours the readers offer, by KOReader's names for them. */
export const COLORS = ["yellow", "green", "cyan", "purple", "red", "orange"] as const;
export type AnnotationColor = (typeof COLORS)[number];

/** An annotation (Go: annotationRec), plus whether the server has this version. */
export interface Annotation {
    id: string;
    format: BookFormat;
    /**
     * Where it is, in a form that survives zoom and reflow:
     *   epub: an EPUB CFI range
     *   pdf:  JSON of a PdfAnchor
     */
    anchor: string;
    /** The highlighted text. */
    text: string;
    note: string;
    color: string;
    /** The chapter's title and the page number shown when it was made. */
    chapter: string;
    page: number;
    created_at: number;
    updated_at: number;
    deleted?: boolean;
    synced?: boolean;
}

/** A PDF annotation's place: rectangles in PDF points from the page's top left. */
export interface PdfAnchor {
    page: number;
    rects: [number, number, number, number][];
}

export function parsePdfAnchor(anchor: string): PdfAnchor | null {
    try {
        const a = JSON.parse(anchor) as PdfAnchor;
        return typeof a.page === "number" && Array.isArray(a.rects) ? a : null;
    } catch {
        return null;
    }
}

export interface AnnotationStore {
    /** Syncs with the server (when signed in), then calls onChange. */
    load(): Promise<void>;
    /** This format's annotations, in the order they were made. */
    list(): Annotation[];
    get(id: string): Annotation | undefined;
    add(a: Pick<Annotation, "anchor" | "text" | "color" | "chapter" | "page"> & { note?: string }): Annotation;
    update(id: string, patch: { note?: string; color?: string }): void;
    remove(id: string): void;
}

const newId = (): string =>
    (crypto.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`).slice(0, 64);

export function createAnnotationStore(uuid: string, format: BookFormat, signedIn: boolean, onChange: () => void): AnnotationStore {
    const key = `cf-annotations-${uuid}`;
    // Both formats' annotations, tombstones included until the server has them.
    let all: Annotation[] = [];
    try {
        all = (JSON.parse(localStorage.getItem(key) || "[]") ?? []) as Annotation[];
    } catch {
        /* unreadable: start empty */
    }
    const save = (): void => {
        try {
            localStorage.setItem(key, JSON.stringify(all));
        } catch {
            /* storage full or blocked: this visit only */
        }
    };

    let flushing: Promise<void> | null = null;
    /** Sends what the server doesn't have yet, oldest change first. */
    function flush(): Promise<void> {
        if (!signedIn) return Promise.resolve();
        flushing ??= (async () => {
            for (const a of all.filter((x) => !x.synced).sort((x, y) => x.updated_at - y.updated_at)) {
                const sent = a.updated_at;
                try {
                    const stored = await api<Annotation>("PUT", `/api/me/annotations/${uuid}/${encodeURIComponent(a.id)}`, { ...a, synced: undefined });
                    if (a.updated_at !== sent) continue; // changed again meanwhile: the next round sends it
                    // The server may hold a newer change from another device.
                    Object.assign(a, stored, { deleted: !!stored.deleted, synced: true });
                } catch (e) {
                    const status = (e as { status?: number }).status;
                    if (status === 400 || status === 404 || status === 409) a.synced = true; // refused for good: don't retry forever
                    else break; // offline or the server is down: try again later
                }
            }
            // A delete the server knows about needs no copy here.
            all = all.filter((a) => !(a.deleted && a.synced));
            save();
        })().finally(() => {
            flushing = null;
        });
        return flushing;
    }

    async function load(): Promise<void> {
        if (signedIn) {
            try {
                await flush();
                const { annotations: server } = await api<{ annotations: Annotation[] }>("GET", `/api/me/annotations/${uuid}`);
                const mine = new Map(all.map((a) => [a.id, a]));
                const merged: Annotation[] = [];
                for (const s of server) {
                    const local = mine.get(s.id);
                    mine.delete(s.id);
                    // A local change newer than the server's is still waiting to be sent.
                    if (local && !local.synced && local.updated_at > s.updated_at) merged.push(local);
                    else if (!s.deleted) merged.push({ ...s, synced: true });
                }
                // Not on the server: made here and not sent yet. (One that was sent
                // and is gone was removed there for good.)
                for (const local of mine.values()) if (!local.synced) merged.push(local);
                all = merged.sort((a, b) => a.created_at - b.created_at);
                save();
            } catch {
                /* offline: the local copy */
            }
        }
        onChange();
    }

    const changed = (): void => {
        save();
        onChange();
        void flush().then(onChange);
    };

    window.addEventListener("online", () => void flush());

    return {
        load,
        list: () => all.filter((a) => a.format === format && !a.deleted),
        get: (id) => all.find((a) => a.id === id && !a.deleted),
        add(a) {
            const now = Date.now();
            const made: Annotation = {
                id: newId(),
                format,
                anchor: a.anchor,
                text: a.text.slice(0, 4000),
                note: (a.note ?? "").slice(0, 4000),
                color: a.color,
                chapter: a.chapter.slice(0, 200),
                page: a.page,
                created_at: now,
                updated_at: now,
                synced: false,
            };
            all.push(made);
            changed();
            return made;
        },
        update(id, patch) {
            const a = all.find((x) => x.id === id);
            if (!a) return;
            if (patch.note !== undefined) a.note = patch.note.slice(0, 4000);
            if (patch.color !== undefined) a.color = patch.color;
            a.updated_at = Math.max(Date.now(), a.updated_at + 1);
            a.synced = false;
            changed();
        },
        remove(id) {
            const a = all.find((x) => x.id === id);
            if (!a) return;
            if (!signedIn) {
                all = all.filter((x) => x !== a); // nothing to tell
            } else {
                a.deleted = true;
                a.text = "";
                a.note = "";
                a.updated_at = Math.max(Date.now(), a.updated_at + 1);
                a.synced = false;
            }
            changed();
        },
    };
}
