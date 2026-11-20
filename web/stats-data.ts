/**
 * The user's reading stats (stats.go) as /api/me/stats sends them, and how
 * they're put in words: for the stats page (stats.ts) and the home page's
 * card (discover.ts).
 */

import { localDay } from "./activity.js";
import { api, el, svgEl } from "./dom.js";
import { localKey } from "./local-keys.js";
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
    goal: { daily_pages: number; yearly_books: number; notice: boolean };
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

/** The local day n days before day (YYYY-MM-DD). */
function daysBefore(day: string, n: number): string {
    const [y, m, d] = day.split("-").map(Number);
    return localDay(new Date(y!, m! - 1, d! - n));
}

/**
 * The home page's summary, beside the quote of the day: today's pages
 * (a ring against the daily goal), the streak, the last 7 days, the books
 * finished this year (against the yearly goal), and the last two weeks as
 * a strip of the stats page's heatmap. A link to the stats page. Null
 * when the user doesn't keep stats.
 */
export function statsSummary(s: Stats): HTMLElement | null {
    if (!s.tracking) return null;
    const goal = s.goal.daily_pages;
    const R = 15;
    const C = 2 * Math.PI * R;
    const done = goal ? Math.min(1, s.today_pages / goal) : s.today_pages > 0 ? 1 : 0;
    const ring = svgEl(
        "svg",
        { viewBox: "0 0 36 36", width: 34, height: 34, class: "stats-ring", "aria-hidden": "true" },
        svgEl("circle", { cx: 18, cy: 18, r: R, class: "stats-ring-track" }),
        svgEl("circle", {
            cx: 18,
            cy: 18,
            r: R,
            class: `stats-ring-fill${goal && done >= 1 ? " stats-ring-done" : ""}`,
            "stroke-dasharray": `${(done * C).toFixed(1)} ${C.toFixed(1)}`,
            transform: "rotate(-90 18 18)",
            "data-len": (done * C).toFixed(1), // how far the reveal winds it (revealStats)
        }),
    );
    const today = goal
        ? s.today_pages >= goal
            ? `Daily goal reached: ${plural(s.today_pages, "page")}`
            : `${s.today_pages} of ${plural(goal, "page")} today`
        : `${plural(s.today_pages, "page")} today`;
    const streak = s.streak.current ? `🔥 ${plural(s.streak.current, "day")} in a row` : s.streak.longest ? `Longest streak: ${plural(s.streak.longest, "day")}` : "Read a page to start a streak";

    // The last 14 days, oldest first, shaded like the heatmap (by pages, against the busiest of them).
    const byDay = new Map(s.days.map((d) => [d.day, d]));
    const recent = Array.from({ length: 14 }, (_, i) => daysBefore(s.today, 13 - i));
    const most = Math.max(1, ...recent.map((d) => byDay.get(d)?.pages ?? 0));
    const strip = svgEl("svg", { viewBox: `0 0 ${14 * 12 - 2} 10`, class: "stats-strip", "aria-hidden": "true" });
    recent.forEach((day, i) => {
        const p = byDay.get(day)?.pages ?? 0;
        const level = p <= 0 ? 0 : Math.min(4, 1 + Math.floor(((p - 1) / most) * 4));
        const cell = svgEl("rect", { x: i * 12, y: 0, width: 10, height: 10, rx: 2, class: `stats-heat-cell stats-heat-${level}` });
        cell.style.setProperty("--i", String(i)); // the reveal's stagger (revealStats)
        strip.append(cell);
    });

    const yearly = s.goal.yearly_books;
    const stat = (value: string, label: string): HTMLElement =>
        el("span", { className: "greeting-stat" }, el("strong", {}, value), el("span", {}, label));
    const card = el(
        "a",
        { className: "greeting-stats", href: "/me/stats" },
        el("span", { className: "greeting-stats-today" }, ring, el("span", { className: "greeting-stats-text" }, el("strong", {}, today), el("span", {}, streak))),
        el(
            "span",
            { className: "greeting-stats-row" },
            stat(plural(s.week_pages, "page"), "last 7 days"),
            stat(yearly ? `${s.year.finished} / ${yearly}` : String(s.year.finished), `${s.year.finished === 1 && !yearly ? "book" : "books"} in ${s.year.year}`),
        ),
        strip,
    );
    card.setAttribute("aria-label", `Reading stats: ${today}. ${streak}. ${s.week_pages} pages in the last 7 days, ${s.year.finished} books finished this year.`);
    return card;
}

/*
 * The home page draws the summary's frame at once, as a skeleton, and fills
 * it in when /api/me/stats answers, so nothing moves when it does. The
 * skeleton is the last answer's card (kept per user, so it's the same
 * size), with its text and marks blanked; the first time, a typical one.
 */

const CACHE = "stats-summary";

/** The parts of an answer the summary uses: today, the last two weeks. */
export function keepStats(user: string, s: Stats): void {
    const recent = s.tracking ? daysBefore(s.today, 13) : s.today;
    const kept: Stats = { ...s, days: s.days.filter((d) => d.day >= recent), year: { ...s.year, books: [] } };
    try {
        localStorage.setItem(localKey(user, CACHE), JSON.stringify(kept));
    } catch {
        /* storage full or blocked: the typical skeleton next time */
    }
}

function keptStats(user: string): Stats | null {
    try {
        const s = JSON.parse(localStorage.getItem(localKey(user, CACHE)) ?? "null") as Stats | null;
        return s && typeof s === "object" && Array.isArray(s.days) && s.goal && s.streak && s.year ? s : null;
    } catch {
        return null;
    }
}

/** What a summary usually says, for its size before the first answer. */
function typicalStats(): Stats {
    const today = localDay();
    return {
        tracking: true,
        today,
        from: today,
        days: [],
        streak: { current: 3, longest: 3 },
        today_pages: 10,
        today_seconds: 0,
        week_pages: 120,
        week_seconds: 0,
        goal: { daily_pages: 20, yearly_books: 0, notice: false },
        year: { year: new Date().getFullYear(), started: 0, finished: 5, books: [] },
        totals: { pages: 0, seconds: 0, finished: 0 },
    };
}

/**
 * The summary's skeleton for user: the last card's shape with nothing in
 * it. Null when they had stats turned off last time (no box to take away).
 */
export function statsSkeleton(user: string): HTMLElement | null {
    const card = statsSummary(keptStats(user) ?? typicalStats());
    if (!card) return null;
    card.removeAttribute("href");
    card.removeAttribute("aria-label");
    card.setAttribute("aria-hidden", "true");
    card.classList.add("greeting-stats-skeleton");
    return card;
}

/** The fresh card, just put where the skeleton was: its text fades in, the ring winds round, the days light up (style.css; not with reduced motion). */
export function revealStats(card: HTMLElement): void {
    const fill = card.querySelector<SVGElement>(".stats-ring-fill");
    fill?.style.setProperty("--len", fill.getAttribute("data-len") ?? "0");
    card.classList.add("greeting-stats-reveal");
}
