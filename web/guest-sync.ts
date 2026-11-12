/**
 * Back in the reader after signing in from it (offerSignIn,
 * reader-chrome.ts): add what was read here signed out to the account,
 * from the reader itself, so the user stays in the book. Only when the
 * account's place in the book differs does it go to /account/import for
 * that book, to ask which to keep, and come back.
 *
 * Runs only when the sign-in came from the reader (?sync-guest in the
 * URL, SYNC_PARAM): signing in some other way never adds a guest's reading without
 * the review (a shared device's other reader may have left it).
 */

import { collectGuestData, forgetGuestBooks } from "./guest-data.js";

export const SYNC_PARAM = "sync-guest";

async function post<T>(url: string, body: unknown): Promise<T> {
    const res = await fetch(url, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`${res.status}`);
    return (await res.json()) as T;
}

/** The reader's own address, without the sync marker. */
function cleanReaderURL(): string {
    const url = new URL(location.href);
    url.searchParams.delete(SYNC_PARAM);
    return url.pathname + url.search + url.hash;
}

/**
 * If this load follows a sign-in from the reader, add this book's guest
 * records to the account. Resolves when the reader may go on (true: something
 * was added); never resolves when it's leaving for the choice page.
 */
export async function syncGuestBook(uuid: string, onStatus: (text: string) => void): Promise<boolean> {
    if (!new URLSearchParams(location.search).has(SYNC_PARAM)) return false;
    const here = cleanReaderURL();
    history.replaceState(history.state, "", here); // a reload doesn't do it again
    const book = collectGuestData().find((b) => b.uuid === uuid);
    if (!book) return false;
    try {
        const { books } = await post<{ books: { uuid: string; state: string }[] }>("/api/me/import/preview", { books: [book] });
        const state = books[0]?.state;
        if (state === "same" || state === "missing") {
            forgetGuestBooks([uuid]);
            return false;
        }
        if (state === "conflict") {
            // Ask which place to keep, then come back here.
            location.replace(`/account/import?book=${encodeURIComponent(uuid)}&next=${encodeURIComponent(here)}`);
            return new Promise<boolean>(() => {});
        }
        const { results } = await post<{ results: { uuid: string; error?: string }[] }>("/api/me/import", {
            books: [{ ...book, use_position: true }],
        });
        if (results[0] && !results[0].error) {
            forgetGuestBooks([uuid]);
            onStatus("Your place, bookmarks and highlights here are now saved to your account");
            return true;
        }
    } catch {
        /* offline or refused: the records stay on this device, for the review */
    }
    onStatus("Couldn't save this device's reading to your account yet");
    return false;
}
