/**
 * The page's one GET /api/me, shared by the scripts that want it: nav.ts
 * (the user part of the top bar) and, on the library grids, app-htmx.ts,
 * whose first reading marks come with it (?reading=1) instead of a second
 * round trip to /api/me/reading. Each entry script is bundled on its own, so
 * the request is kept on window, not in this module.
 */
import type { MeResponse } from "./types.js";

export interface MeAnswer {
    ok: boolean;
    status: number;
    body: MeResponse;
}

declare global {
    interface Window {
        cfMe?: Promise<MeAnswer>;
    }
}

/** Whether this page shows the library grid (app-htmx.ts), which wants the marks. */
function wantsReading(): boolean {
    return document.getElementById("books") !== null;
}

/** This page's /api/me answer: requested by whichever script asks first. Rejects when offline. */
export function meRequest(): Promise<MeAnswer> {
    window.cfMe ??= fetch(wantsReading() ? "/api/me?reading=1" : "/api/me", { credentials: "same-origin" }).then(async (r) => ({
        ok: r.ok,
        status: r.status,
        body: (await r.json()) as MeResponse,
    }));
    return window.cfMe;
}
