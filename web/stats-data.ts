/**
 * The user's reading stats (stats.go) as /api/me/stats sends them, and how
 * they're put in words: for the stats page (stats.ts) and the home page's
 * card (discover.ts).
 */

import { localDay } from "./activity.js";
import { api, el, svgEl } from "./dom.js";
import type { Book } from "./types.js";

export interface Day {
    day: string;
    pages: number;
    seconds: number;
}

/** Go: statsAnswer. */
export interface Stats {
    tracking: boolean;
    today: string;
    from: string;
    days: Day[];
    streak: { current: number; longest: number };
    today_pages: number;
    today_seconds: number;
    week_pages: number;
    week_seconds: number;
    goal: { daily_pages: number; yearly_books: number };
    year: { year: number; started: number; finished: number; books: Book[] };
    totals: { pages: number; seconds: number; finished: number };
}

/** /api/me/stats as of today here. */
export function fetchStats(): Promise<Stats> {
    const q = new URLSearchParams({ today: localDay(), tz: String(new Date().getTimezoneOffset()) });
    return api<Stats>("GET", `/api/me/stats?${q}`);
}

export const plural = (n: number, one: string, many = `${one}s`): string => `${n.toLocaleString()} ${n === 1 ? one : many}`;

/** "1 h 5 min", "12 min", "under a minute". */
export function duration(seconds: number): string {
    const m = Math.round(seconds / 60);
    if (m < 1) return seconds > 0 ? "under a minute" : "0 min";
    const h = Math.floor(m / 60);
    return h ? `${h} h${m % 60 ? ` ${m % 60} min` : ""}` : `${m} min`;
}

/**
 * The home page's card: a ring for today's pages against the daily goal (a
 * book without one), the streak, and a link to the stats page. Null when
 * there's nothing to show yet (no reading recorded and no goal).
 */
export function statsCard(s: Stats): HTMLElement | null {
    if (!s.tracking || (s.totals.pages === 0 && s.totals.seconds === 0 && !s.goal.daily_pages && !s.goal.yearly_books)) return null;
    const goal = s.goal.daily_pages;
    const R = 15;
    const C = 2 * Math.PI * R;
    const done = goal ? Math.min(1, s.today_pages / goal) : 0;
    const ring = svgEl(
        "svg",
        { viewBox: "0 0 36 36", width: 36, height: 36, class: "stats-ring", "aria-hidden": "true" },
        svgEl("circle", { cx: 18, cy: 18, r: R, class: "stats-ring-track" }),
        svgEl("circle", {
            cx: 18,
            cy: 18,
            r: R,
            class: `stats-ring-fill${done >= 1 ? " stats-ring-done" : ""}`,
            "stroke-dasharray": `${(done * C).toFixed(1)} ${C.toFixed(1)}`,
            transform: "rotate(-90 18 18)",
        }),
    );
    const today = goal
        ? s.today_pages >= goal
            ? `Daily goal reached: ${plural(s.today_pages, "page")} today`
            : `${s.today_pages} of ${plural(goal, "page")} today`
        : `${plural(s.today_pages, "page")} today`;
    const streak = s.streak.current ? `🔥 ${plural(s.streak.current, "day")} in a row` : "Read today to start a streak";
    return el(
        "a",
        { className: "greeting-stats", href: "/me/stats" },
        ring,
        el("span", { className: "greeting-stats-text" }, el("strong", {}, today), el("span", {}, streak)),
    );
}
