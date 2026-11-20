/**
 * /me/stats (templates/stats.html): the user's reading stats from
 * /api/me/stats (stats.go): streaks, today and this week, a year of pages
 * read day by day (a heatmap, one column a week), the books finished this
 * year against the yearly goal, and the goals themselves.
 *
 * Everything is built with DOM calls (el(), svgEl()); titles are text.
 */

import { localDay, sendActivity } from "./activity.js";
import { byId, el, errorMessage, svgEl } from "./dom.js";
import { lastKnownUser } from "./local-keys.js";
import { setAccountPrefs } from "./prefs.js";
import { duration, fetchStats, plural, type Day, type Stats } from "./stats-data.js";
import { coverUrl } from "./utils.js";

/* ------------------------------------------------------------------ */
/* Tiles                                                              */
/* ------------------------------------------------------------------ */

/** A bar of done out of goal (no bar without a goal). */
function goalBar(done: number, goal: number, label: string): HTMLElement | null {
    if (goal <= 0) return null;
    const bar = el("div", { className: "stats-bar" }, el("span", { className: "stats-bar-fill" }));
    (bar.firstElementChild as HTMLElement).style.width = `${Math.min(100, (done / goal) * 100)}%`;
    bar.setAttribute("role", "progressbar");
    bar.setAttribute("aria-label", label);
    bar.setAttribute("aria-valuemin", "0");
    bar.setAttribute("aria-valuemax", String(goal));
    bar.setAttribute("aria-valuenow", String(Math.min(done, goal)));
    if (done >= goal) bar.classList.add("stats-bar-done");
    return bar;
}

function tile(label: string, value: string, detail: string, extra?: HTMLElement | null): HTMLElement {
    return el(
        "div",
        { className: "stats-tile" },
        el("div", { className: "stats-tile-label" }, label),
        el("div", { className: "stats-tile-value" }, value),
        el("div", { className: "stats-tile-detail" }, detail),
        ...(extra ? [extra] : []),
    );
}

function showTiles(s: Stats): void {
    const daily = s.goal.daily_pages;
    const yearly = s.goal.yearly_books;
    const streakNote = s.streak.current === 0 ? "Read today to start one" : s.today_pages || s.today_seconds >= 60 ? "Read today ✓" : "Read today to keep it going";
    byId("stats-tiles").replaceChildren(
        tile("Streak", `${s.streak.current ? "🔥 " : ""}${plural(s.streak.current, "day")}`, `${streakNote} · longest ${plural(s.streak.longest, "day")}`),
        tile(
            "Today",
            daily ? `${s.today_pages} / ${plural(daily, "page")}` : plural(s.today_pages, "page"),
            daily && s.today_pages >= daily ? `Goal reached · ${duration(s.today_seconds)}` : duration(s.today_seconds),
            goalBar(s.today_pages, daily, "Today's pages against the daily goal"),
        ),
        tile("Last 7 days", plural(s.week_pages, "page"), duration(s.week_seconds)),
        tile(
            String(s.year.year),
            yearly ? `${s.year.finished} / ${plural(yearly, "book")}` : `${plural(s.year.finished, "book")}`,
            `finished · ${plural(s.year.started, "book")} started`,
            goalBar(s.year.finished, yearly, "Books finished this year against the yearly goal"),
        ),
    );
}

/* ------------------------------------------------------------------ */
/* Heatmap                                                            */
/* ------------------------------------------------------------------ */

const CELL = 11;
const GAP = 3;
const STEP = CELL + GAP;
const LEFT = 28; // weekday labels
const TOP = 16; // month labels
const LEVELS = 5; // 0: none, 1-4: more and more

const parseDay = (d: string): Date => {
    const [y, m, dd] = d.split("-").map(Number);
    return new Date(y!, m! - 1, dd!);
};

/** The level (0-4) of pages, against the user's own busier days. */
function leveller(days: Day[]): (pages: number) => number {
    const sorted = days.map((d) => d.pages).filter((p) => p > 0).sort((a, b) => a - b);
    // The 95th percentile tops the scale: one marathon day doesn't wash out the rest.
    const top = sorted[Math.floor((sorted.length - 1) * 0.95)] ?? 1;
    return (pages) => (pages <= 0 ? 0 : Math.min(LEVELS - 1, 1 + Math.floor(((pages - 1) / Math.max(top, 1)) * (LEVELS - 1))));
}

function cellTitle(d: Date, a: Day | undefined): string {
    const when = d.toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "short", year: "numeric" });
    if (!a || (a.pages === 0 && a.seconds === 0)) return `No reading · ${when}`;
    return `${plural(a.pages, "page")} · ${duration(a.seconds)} · ${when}`;
}

function showHeatmap(s: Stats): void {
    const byDay = new Map(s.days.map((d) => [d.day, d]));
    const level = leveller(s.days);
    const from = parseDay(s.from);
    const today = parseDay(s.today);
    const weeks = Math.floor((today.getTime() - from.getTime()) / (7 * 86_400_000)) + 1;
    const width = LEFT + weeks * STEP;
    const height = TOP + 7 * STEP;
    const svg = svgEl("svg", { viewBox: `0 0 ${width} ${height}`, class: "stats-heatmap", role: "img" });
    svg.setAttribute("aria-label", `Pages read each day, from ${from.toLocaleDateString()} to today`);

    for (const [row, name] of [
        [1, "Mon"],
        [3, "Wed"],
        [5, "Fri"],
    ] as const) {
        const t = svgEl("text", { x: 0, y: TOP + row * STEP + CELL - 2, class: "stats-heat-label" });
        t.textContent = name;
        svg.append(t);
    }

    for (let d = new Date(from), i = 0; d <= today; d.setDate(d.getDate() + 1), i++) {
        const col = Math.floor(i / 7);
        const row = d.getDay();
        // Each month's name over the week it starts in (room permitting at the ends).
        if (d.getDate() === 1 && col < weeks - 1) {
            const t = svgEl("text", { x: LEFT + col * STEP, y: TOP - 5, class: "stats-heat-label" });
            t.textContent = d.toLocaleDateString(undefined, { month: "short" });
            svg.append(t);
        }
        const a = byDay.get(localDay(d));
        const rect = svgEl("rect", {
            x: LEFT + col * STEP,
            y: TOP + row * STEP,
            width: CELL,
            height: CELL,
            rx: 2,
            class: `stats-heat-cell stats-heat-${level(a?.pages ?? 0)}`,
        });
        const title = svgEl("title");
        title.textContent = cellTitle(d, a);
        rect.append(title);
        svg.append(rect);
    }
    const box = byId("stats-heat");
    box.replaceChildren(svg);
    box.scrollLeft = box.scrollWidth; // the latest weeks first on a narrow screen

    const read = s.days.filter((d) => d.pages > 0 || d.seconds >= 60).length;
    const pages = s.days.reduce((n, d) => n + d.pages, 0);
    const secs = s.days.reduce((n, d) => n + d.seconds, 0);
    byId("stats-heat-sum").textContent = pages || secs ? `${plural(pages, "page")} and ${duration(secs)} over ${plural(read, "day")} in the last year.` : "Nothing yet: open a book and turn a page.";

    const legend = byId("stats-heat-legend");
    legend.replaceChildren(
        el("span", {}, "Less"),
        ...Array.from({ length: LEVELS }, (_, i) =>
            svgEl("svg", { width: CELL, height: CELL, "aria-hidden": "true" }, svgEl("rect", { width: CELL, height: CELL, rx: 2, class: `stats-heat-cell stats-heat-${i}` })),
        ),
        el("span", {}, "More"),
    );
}

/* ------------------------------------------------------------------ */
/* The year's books                                                   */
/* ------------------------------------------------------------------ */

function showYear(s: Stats): void {
    const goal = s.goal.yearly_books;
    const left = goal - s.year.finished;
    const text =
        goal <= 0
            ? `${plural(s.year.finished, "book")} finished in ${s.year.year}. Set a goal below to aim for more.`
            : left > 0
              ? `${s.year.finished} of ${plural(goal, "book")}: ${plural(left, "book")} to go.`
              : `Goal reached: ${plural(s.year.finished, "book")} finished, ${goal} planned. 🎉`;
    byId("stats-year").replaceChildren(el("p", { className: "auth-muted" }, text), ...[goalBar(s.year.finished, goal, "Books finished this year")].filter((b) => b !== null));
    byId("stats-covers").replaceChildren(
        ...s.year.books.map((b) => {
            const title = b.title || "Untitled";
            const img = el("img", { src: coverUrl(b), alt: title, loading: "lazy", className: "stats-cover" });
            const link = el("a", { href: `/#book/${encodeURIComponent(b.uuid)}`, className: "stats-cover-link" }, img);
            link.title = b.authors ? `${title} · ${b.authors}` : title;
            return link;
        }),
    );
}

/* ------------------------------------------------------------------ */
/* Goals                                                              */
/* ------------------------------------------------------------------ */

const form = byId<HTMLFormElement>("stats-goals");
const goalsStatus = byId("stats-goals-status");
const input = (name: string): HTMLInputElement => form.elements.namedItem(name) as HTMLInputElement;
const goalValue = (name: string): number | null => {
    const v = input(name).value.trim();
    if (v === "") return 0;
    const n = Number(v);
    return Number.isInteger(n) && n >= 0 && n <= 1000 ? n : null;
};

form.addEventListener("submit", (e) => {
    e.preventDefault();
    const daily = goalValue("daily_pages");
    const yearly = goalValue("yearly_books");
    if (daily === null || yearly === null) {
        goalsStatus.textContent = "Goals are whole numbers from 0 (none) to 1000.";
        return;
    }
    goalsStatus.textContent = "Saving…";
    setAccountPrefs({ daily_pages: daily, yearly_books: yearly }, lastKnownUser())
        .then(async () => {
            goalsStatus.textContent = "Saved.";
            await load();
        })
        .catch((err: unknown) => (goalsStatus.textContent = `Couldn't save: ${errorMessage(err)}`));
});

/* ------------------------------------------------------------------ */
/* Start                                                              */
/* ------------------------------------------------------------------ */

async function load(): Promise<void> {
    const note = byId("stats-note");
    try {
        const s = await fetchStats();
        note.textContent = s.tracking
            ? `${plural(s.totals.pages, "page")} and ${duration(s.totals.seconds)} read in all · ${plural(s.totals.finished, "book")} finished.`
            : "Reading stats are turned off in Preferences: nothing new is recorded.";
        input("daily_pages").value = s.goal.daily_pages ? String(s.goal.daily_pages) : "";
        input("yearly_books").value = s.goal.yearly_books ? String(s.goal.yearly_books) : "";
        showTiles(s);
        showHeatmap(s);
        showYear(s);
    } catch (err) {
        note.textContent = `Couldn't load your stats: ${errorMessage(err)}`;
    }
}

// What this browser read and hasn't sent yet goes first, so it's counted.
const user = lastKnownUser();
void (user ? sendActivity(user).catch(() => {}) : Promise.resolve()).then(load);
