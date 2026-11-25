/**
 * The shared top bar (templates/topbar.html):
 *   - fills the user part: "Sign in", or ★ Favorites, Reading and a user
 *     menu (pages stay identical for everyone and cacheable, so this asks
 *     /api/me)
 *   - opens/closes the ☰ menu (narrow screens), the user menu and the
 *     search box (narrow screens); a click outside or Escape closes them
 */
import { keepOnlyUser, registerWorker } from "./book-cache.js";
import { settleAccountPrefs } from "./account-prefs.js";
import { startSendingPending } from "./send-pending.js";
import { onSyncChange, readSync, STALE_MS, unsentBooks, writeSync } from "./sync-state.js";
import { forgetOfflineReading, refreshOfflineReading } from "./offline-books.js";
import { api, el, withPageScheme } from "./dom.js";
import { forgetSynced } from "./local-keys.js";
import { declineOffer, guestBookCount, offerDeclined, offerDeclinedForGood } from "./guest-data.js";

// Addresses the server wrote for this site (e.g. the device sync server on
// the account page): use the page's scheme, which a proxy may hide from it.
for (const code of document.querySelectorAll<HTMLElement>(".same-origin-url")) {
    code.textContent = withPageScheme(code.textContent ?? "");
}
import { meRequest } from "./me.js";
import type { HardcoverIndicator, HardcoverStatus } from "./types.js";
import { clearable } from "./clearable.js";
import { attachSearchSuggest } from "./search-suggest.js";


function icon(path: string): SVGSVGElement {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("width", "18");
    svg.setAttribute("height", "18");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "2");
    svg.setAttribute("stroke-linejoin", "round");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("aria-hidden", "true");
    const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
    p.setAttribute("d", path);
    svg.append(p);
    return svg;
}
const STAR = "M12 3l2.8 5.7 6.2.9-4.5 4.4 1.1 6.2L12 17.3 6.4 20.2l1.1-6.2L3 9.6l6.2-.9z";
const BOOK = "M2 4h6a4 4 0 0 1 4 4v12a3 3 0 0 0-3-3H2zM22 4h-6a4 4 0 0 0-4 4v12a3 3 0 0 1 3-3h7z";
const BOOKMARK = "M6 3h12a1 1 0 0 1 1 1v17l-7-4.5L5 21V4a1 1 0 0 1 1-1z";
const UPLOAD = "M12 15V4M7 9l5-5 5 5M4 15v4a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-4";
const INBOX = "M3 13l3-8h12l3 8M3 13v5a1 1 0 0 0 1 1h16a1 1 0 0 0 1-1v-5M3 13h5l1 2h6l1-2h5";
const SYNC = "M20 11a8 8 0 0 0-14.3-4.9L4 8M4 4v4h4M4 13a8 8 0 0 0 14.3 4.9L20 16m0 4v-4h-4";
const BELL = "M6 16V11a6 6 0 0 1 12 0v5l2 2H4zM10 21h4";
const CHECK = "M5 12.5l4.5 4.5L19 7.5";
const CLOCK = "M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18zM12 7v5l3 2";
const CROSS = "M6 6l12 12M18 6L6 18";
const ALERT = "M12 7v6M12 17h.01";
const INFO = "M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18zM12 11v5M12 8h.01";
const SLIDERS = "M4 6h9m4 0h3M4 12h3m4 0h9M4 18h11m4 0h1M15 4v4M9 10v4M17 16v4";

/**
 * The offer to add this device's signed-out reading to the account: a strip
 * under the top bar, until the user reviews it, puts it off for this visit
 * (Not now), or says it isn't theirs (never offered to this account again).
 */
function showGuestOffer(user: string, books: number): void {
    document.querySelector(".guest-offer")?.remove();
    if (!books || location.pathname === "/account/import" || offerDeclined(user)) return;
    const strip = el(
        "div",
        { className: "guest-offer", role: "region", ariaLabel: "Reading on this device" },
        el("span", {
            textContent: `You read ${books === 1 ? "a book" : `${books} books`} on this device while signed out. Add ${books === 1 ? "it" : "them"} to your account?`,
        }),
    );
    const later = el("button", { type: "button", className: "btn-standard", textContent: "Not now" });
    const notMine = el("button", { type: "button", className: "btn-standard", textContent: "These aren't mine" });
    later.addEventListener("click", () => {
        declineOffer(user, false);
        strip.remove();
    });
    notMine.addEventListener("click", () => {
        declineOffer(user, true);
        strip.remove();
    });
    strip.append(el("a", { className: "btn-standard btn-primary", href: "/account/import", textContent: "Review" }), later, notMine);
    document.querySelector(".site-bar")?.after(strip);
}

function init(bar: HTMLElement): void {
    const user = bar.querySelector<HTMLElement>(".tb-user");
    const active = user?.dataset["active"] ?? "";
    /** A link to a page of the user part: data-nav names it, for the snapshot (NAV_KEY). */
    const mark = (a: HTMLAnchorElement, name: string): HTMLAnchorElement => {
        a.dataset["nav"] = name;
        if (active === name) a.setAttribute("aria-current", "page");
        return a;
    };

    // ---- open/close ---------------------------------------------------------

    /** Every toggle: its button, and what "open" means for it. */
    interface Toggle {
        btn: HTMLElement;
        set(open: boolean): void;
    }
    let toggles: Toggle[] = [];
    function register(btn: HTMLElement | null, set: (open: boolean) => void): Toggle | null {
        if (!btn) return null;
        const t: Toggle = {
            btn,
            set(open: boolean) {
                btn.setAttribute("aria-expanded", String(open));
                set(open);
            },
        };
        toggles.push(t);
        btn.addEventListener("click", (e) => {
            e.stopPropagation();
            const open = btn.getAttribute("aria-expanded") !== "true";
            closeAll();
            t.set(open);
        });
        return t;
    }
    function closeAll(): void {
        for (const t of toggles) if (t.btn.getAttribute("aria-expanded") === "true") t.set(false);
    }
    // A tap off the bar while one of its menus is open only closes the menu:
    // it isn't passed on, so it doesn't also open the book under it (or
    // follow a link). Taps on the bar itself go through (to another menu).
    document.addEventListener(
        "click",
        (e) => {
            if (!toggles.some((t) => t.btn.getAttribute("aria-expanded") === "true")) return;
            if (e.target instanceof Element && e.target.closest(".site-bar")) return;
            e.preventDefault();
            e.stopPropagation();
            closeAll();
        },
        true,
    );
    document.addEventListener("click", (e) => {
        // Clicks inside an open panel (e.g. the search box) keep it open.
        if (e.target instanceof Element && e.target.closest(".tb-nav, .tb-menu-list, .tb-search")) return;
        closeAll();
    });
    document.addEventListener("keydown", (e) => {
        if (e.key !== "Escape") return;
        const open = toggles.find((t) => t.btn.getAttribute("aria-expanded") === "true");
        if (!open) return;
        open.set(false);
        open.btn.focus();
    });

    register(bar.querySelector(".tb-menu-btn"), (open) => bar.classList.toggle("nav-open", open));

    const searchBtn = bar.querySelector<HTMLElement>(".tb-search-btn");
    const searchInput = bar.querySelector<HTMLInputElement>(".tb-search input");
    register(searchBtn, (open) => {
        bar.classList.toggle("search-open", open);
        if (open) searchInput?.focus();
    });
    bar.querySelector(".tb-search-close")?.addEventListener("click", () => closeAll());
    // In the box: a ✕ that empties it (not .tb-search-close, which closes
    // the box on a phone), and suggestions while typing. The suggestions
    // count as one of the bar's menus, so an outside click or Escape closes
    // them (the box, a combobox, says aria-expanded); opening them closes
    // the other menus, but not the box they hang from.
    const searchForm = bar.querySelector<HTMLFormElement>("form.tb-search");
    if (searchForm && searchInput) {
        clearable(searchInput, "Clear the search");
        const suggest = attachSearchSuggest(searchForm, searchInput, () => {
            for (const t of toggles) if (t.btn !== searchBtn && t.btn !== searchInput && t.btn.getAttribute("aria-expanded") === "true") t.set(false);
        });
        toggles.push({
            btn: searchInput,
            set(open) {
                if (!open) suggest.close();
            },
        });
    }

    // A page's actions: the Manage menu, and when folded your own actions
    // with it (filtered.html); picking one closes it, as any click does.
    const actions = bar.querySelector<HTMLElement>(".tb-actions");
    register(bar.querySelector(".tb-manage-btn"), (open) => actions?.classList.toggle("open", open));

    // Keep the whole bar on screen. The breakpoints in style.css size it for
    // a typical page, but a long title, an admin's shortcuts and a page's
    // tools can still add up to more than the window: then it steps down,
    // a level at a time, until it fits (tb-tight: shortcuts and labels go,
    // as on a phone; tb-tighter: the browse links go into ☰; tb-tightest: the
    // search box becomes its icon; tb-fold: the page's own actions go into
    // its ⋯ menu). Without this the user menu, last in the bar, would be
    // pushed off the edge.
    // Measured by the user part's right edge, not scrollWidth, which an open
    // dropdown would count too; the page title, which shrinks first, counts
    // as crowded once it's cut to less than TITLE_MIN.
    const levels = ["tb-tight", "tb-tighter", "tb-tightest", "tb-fold"];
    const TITLE_MIN = 140;
    const last = user ?? bar.querySelector<HTMLElement>(".tb-tools");
    const title = bar.querySelector<HTMLElement>(".tb-title");
    const overflows = (): boolean => {
        if (title && title.offsetParent && title.clientWidth < Math.min(title.scrollWidth, TITLE_MIN)) return true;
        if (!last) return false;
        const edge = bar.getBoundingClientRect().right - parseFloat(getComputedStyle(bar).paddingRight);
        return last.getBoundingClientRect().right > edge + 1;
    };
    const fit = (): void => {
        bar.classList.remove(...levels);
        for (const level of levels) {
            if (!overflows()) return;
            bar.classList.add(level);
        }
    };
    let fitQueued = false;
    const queueFit = (): void => {
        if (fitQueued) return;
        fitQueued = true;
        requestAnimationFrame(() => {
            fitQueued = false;
            fit();
        });
    };
    new ResizeObserver(queueFit).observe(bar);
    // The user part and the grid's Select are drawn after this runs.
    new MutationObserver(queueFit).observe(bar, { childList: true, subtree: true, characterData: true });
    fit();

    // ---- user part ----------------------------------------------------------
    //
    // The last answer from /api/me is kept in localStorage, and the user part
    // it made as a tree of nodes (NAV_KEY, JSON: never HTML, which would need
    // innerHTML and so Trusted Types): the template's inline script builds it
    // as soon as the bar is parsed, before this module runs (modules wait for
    // the whole page). This then draws it again, live, from the answer, and
    // the request redraws it only if the server's answer changed (promoted,
    // demoted, imports waiting, signed out elsewhere). First visit: the
    // template's throbber shows until the request finishes.

    if (!user) return;
    const userEl: HTMLElement = user;
    const CACHE_KEY = "cf-me";
    /** The user part as a NavNode list, as topbar.html's inline script reads it (keep the name and shape in step). */
    const NAV_KEY = "cf-nav-tree";
    try {
        localStorage.removeItem("cf-nav"); // its old form, as markup
    } catch {
        /* storage blocked */
    }
    interface MeState {
        username: string | null;
        isAdmin: boolean;
        canEdit: boolean;
        canAdmin: boolean;
        /** Books waiting in the import queue; null when there is no queue to show. */
        imports?: number | null;
        /** Unseen notifications (notifications.go). */
        notifications?: number;
        /** Hardcover sync; absent for users who never linked. */
        hardcover?: HardcoverIndicator | null;
    }
    const cache = {
        get(): string | null {
            try {
                return localStorage.getItem(CACHE_KEY);
            } catch {
                return null;
            }
        },
        set(v: string | null): void {
            try {
                if (v === null) {
                    localStorage.removeItem(CACHE_KEY);
                    localStorage.removeItem(NAV_KEY);
                } else localStorage.setItem(CACHE_KEY, v);
            } catch {
                /* storage blocked: no instant render, the throbber still works */
            }
        },
    };
    // Signing in or out changes the answer: don't show a stale one next.
    if (/^\/(login|register|l\/)/.test(location.pathname)) cache.set(null);

    let userToggle: Toggle | null = null;
    let syncToggle: Toggle | null = null;

    // ---- sync and notifications button ------------------------------------------
    // Its menu starts with whether reading changes have reached the server
    // and whether the books kept for offline reading are (book-sync.ts,
    // sync-state.ts), each with a check when done; then the notifications not
    // dismissed yet (up to NOTICES_SHOWN, notifications.go) and a link to all
    // of them; then a switch to pause downloads on a slow connection.
    // The button is a bell, with a count while any notifications are unseen
    // (opening the menu marks them seen). While a book downloads it's the
    // sync icon with its progress ring (the count still on it), and stays so
    // for SYNC_LINGER_MS after, so a quick download isn't missed, with how it
    // went: a green check, or a "!" when a download failed. A dot when
    // something waits (unsent changes, books paused or left for the next
    // library page) and nothing's unseen.

    const megabytes = (n: number): string => `${(n / 1048576).toFixed(1)} MB`;
    const plural = (n: number, one: string): string => `${n} ${one}${n === 1 ? "" : "s"}`;
    const NOTICES_SHOWN = 5;
    const SYNC_LINGER_MS = 3000;
    const HC_POLL_MS = 4000;
    interface SyncButton {
        node: HTMLElement;
        /** How many notifications are unseen. */
        setUnseen(n: number): void;
        /** The user's Hardcover sync, as /api/me has it (null: never linked). */
        setHardcover(hc: HardcoverIndicator | null): void;
    }
    let syncFor: (SyncButton & { name: string }) | null = null;
    /** The sync button, made once per user (render runs again when the server's answer differs). */
    function syncButton(name: string, unseen: number, hc: HardcoverIndicator | null): HTMLElement {
        if (syncFor?.name !== name) syncFor = { name, ...makeSyncButton(name) };
        syncFor.setUnseen(unseen);
        syncFor.setHardcover(hc);
        return syncFor.node;
    }
    /**
     * A line of the menu's top: its name, a check when done or a clock and a
     * word or two while waiting, and an (i) that shows what it covers and
     * where it stands in full.
     */
    function stateLine(label: string): { line: HTMLElement; set(done: boolean, short: string, detail: string, alert?: boolean): void } {
        const status = el("span", { className: "tb-sync-status" });
        status.setAttribute("role", "status");
        const info = el("button", { type: "button", className: "tb-sync-info-btn", title: `About ${label}`, ariaLabel: `About ${label}` });
        info.setAttribute("aria-expanded", "false");
        info.append(icon(INFO));
        const detail = el("p", { className: "tb-sync-info", hidden: true });
        info.addEventListener("click", (e) => {
            e.stopPropagation();
            detail.hidden = !detail.hidden;
            info.setAttribute("aria-expanded", String(!detail.hidden));
        });
        const head = el("p", { className: "tb-sync-line tb-sync-state" }, el("span", { className: "tb-sync-label", textContent: label }), status, info);
        const line = el("div", {}, head, detail);
        let was: string | null = null;
        return {
            line,
            set(done, short, d, alert = false) {
                const mark = alert ? "alert" : done ? "done" : "wait";
                if (was !== mark) {
                    was = mark;
                    const glyph = icon(alert ? ALERT : done ? CHECK : CLOCK);
                    glyph.classList.add("tb-sync-mark");
                    head.classList.toggle("is-done", done);
                    const old = head.querySelector(".tb-sync-mark");
                    if (old) old.replaceWith(glyph);
                    else head.prepend(glyph);
                }
                if (status.textContent !== short) status.textContent = short;
                if (detail.textContent !== d) detail.textContent = d;
            },
        };
    }
    interface Notice {
        id: number;
        /** placeholder_added, favorite_author, favorite_series or hardcover_relink (notifications.go). */
        kind: string;
        title: string;
        on_deck: boolean;
        seen: boolean;
        href: string;
        /** favorite_*: the author or series, and how many of its books are new. */
        subject?: string;
        count: number;
        /** placeholder_added: the saved book's authors ("A & B"), if known. */
        authors?: string;
    }
    /** A notice's headline and the line under it (may be empty). */
    function noticeText(n: Notice): [headline: string, note: string] {
        const many = n.count > 1;
        const subject = n.subject ?? "";
        switch (n.kind) {
            case "favorite_author":
                return many ? [`${n.count} new books by ${subject}`, "An author you follow"] : [n.title, `New, by ${subject}`];
            case "hardcover_relink":
                return ["Link Hardcover again", `To ${n.title}`];
            case "favorite_series":
                return many ? [`${n.count} new books in ${subject}`, "A series you follow"] : [n.title, `New in ${subject}`];
            default:
                // As notifications.html words it.
                return [`Saved book added to library: ${n.title}${n.authors ? ` by ${n.authors}` : ""}${n.on_deck ? " · On deck" : ""}`, ""];
        }
    }
    function makeSyncButton(name: string): SyncButton {
        const ns = "http://www.w3.org/2000/svg";
        const ring = document.createElementNS(ns, "svg");
        ring.setAttribute("viewBox", "0 0 36 36");
        ring.setAttribute("class", "tb-sync-ring");
        ring.setAttribute("aria-hidden", "true");
        const arc = document.createElementNS(ns, "circle");
        for (const [k, v] of [["cx", "18"], ["cy", "18"], ["r", "16"], ["pathLength", "100"], ["stroke-dasharray", "0 100"]]) arc.setAttribute(k!, v!);
        ring.append(arc);
        const btn = el("button", { type: "button", className: "tb-icon tb-sync-btn", title: "Notifications and sync", ariaLabel: "Notifications and sync" });
        btn.setAttribute("aria-haspopup", "true");
        btn.setAttribute("aria-expanded", "false");
        const syncIcon = icon(SYNC);
        const bellIcon = icon(BELL);
        const badge = el("span", { className: "tb-badge" });
        btn.append(syncIcon, ring);
        let unseen = 0;
        let busyAt = 0; // when a download was last seen going
        let busySince = 0; // when the downloads now ending began
        let lingerTimer = 0;
        const result = el("span", { className: "tb-sync-result" });
        // Built once and updated in place: rebuilding it under the pointer
        // (or the keyboard focus) would lose a click.
        const readingLine = stateLine("Reading Sync");
        const booksLine = stateLine("Book Sync");
        // Hardcover sync (internal/hardcoversync), for users with a link:
        // passive, never a pop-up. While the menu is open and something is
        // being sent, /api/me/hardcover is asked again every HC_POLL_MS.
        const hcLine = stateLine("Hardcover Sync");
        hcLine.line.hidden = true;
        const hcRelink = el("a", { href: "/account#hardcover", className: "tb-sync-line", textContent: "Link Hardcover again", hidden: true });
        hcLine.line.append(hcRelink);
        let hc: HardcoverIndicator | null = null;
        let hcError = ""; // the latest failure's message, from /api/me/hardcover
        let hcTimer = 0;
        let hcAsk = 0;
        // Something to send now (not only failures waiting out their back-off).
        const hcActive = (h: HardcoverIndicator | null): boolean => !!h && h.state === "linked" && (h.busy || h.waiting > h.failing);
        const stopHcPoll = (): void => {
            window.clearTimeout(hcTimer);
            hcTimer = 0;
            hcAsk++; // an answer still on its way is dropped
        };
        const askHardcover = (): void => {
            const ask = ++hcAsk;
            api<HardcoverStatus>("GET", "/api/me/hardcover")
                .then((st) => {
                    if (ask !== hcAsk || panel.hidden) return;
                    if (st.state === "linked" && st.summary) {
                        hc = { state: "linked", busy: !!st.busy, waiting: st.summary.waiting, failing: st.summary.failing, ...(st.summary.last_sent ? { last_sent: st.summary.last_sent } : {}) };
                        hcError = st.summary.last_error ?? "";
                    } else hc = st.state === "broken" ? { state: "broken", busy: false, waiting: 0, failing: 0 } : null;
                    paint();
                    pollHardcover();
                })
                .catch(() => {
                    if (ask === hcAsk && !panel.hidden) pollHardcover(); // offline for a moment: keep asking while it's open
                });
        };
        /** Ask again in HC_POLL_MS while the menu is open and something's being sent; else stop. */
        const pollHardcover = (): void => {
            window.clearTimeout(hcTimer);
            hcTimer = 0;
            if (panel.hidden || !hcActive(hc)) return;
            hcTimer = window.setTimeout(() => {
                hcTimer = 0;
                askHardcover();
            }, HC_POLL_MS);
        };
        // The book downloading: cover, title, authors, a progress bar, sizes.
        const cover = el("img", { className: "tb-sync-cover", alt: "", width: 40, height: 56, loading: "lazy" });
        const bookTitle = el("span", { className: "tb-sync-title" });
        const bookAuthors = el("span", { className: "tb-sync-authors" });
        const fill = el("span", { className: "tb-sync-fill" });
        const bar = el("span", { className: "tb-sync-bar" }, fill);
        bar.setAttribute("role", "progressbar");
        bar.setAttribute("aria-valuemin", "0");
        bar.setAttribute("aria-valuemax", "100");
        const bookDetail = el("span", { className: "tb-sync-detail" });
        const card = el("div", { className: "tb-sync-book", hidden: true }, cover, el("span", { className: "tb-sync-book-text" }, bookTitle, bookAuthors, bar, bookDetail));
        const nextLine = el("p", { className: "tb-sync-line tb-sync-next", hidden: true });
        const pause = el("button", { type: "button" });
        pause.addEventListener("click", (e) => {
            e.stopPropagation();
            writeSync(name, { paused: !readSync(name).paused });
        });
        // The notifications not dismissed yet, newest first, fetched when the menu opens.
        const notices = el("ul", { className: "tb-notices", hidden: true });
        const allNotices = mark(el("a", { href: "/me/notifications", className: "tb-notices-all", textContent: "All notifications" }), "notifications");
        let noticesWant = 0;
        const loadNotices = (): void => {
            const want = ++noticesWant;
            api<Notice[]>("GET", "/api/me/notifications")
                .then((ns) => {
                    if (want === noticesWant) showNotices(ns);
                })
                .catch(() => {}); // offline: the link still goes to them
        };
        const showNotices = (ns: Notice[]): void => {
            notices.replaceChildren(
                ...ns.slice(0, NOTICES_SHOWN).map((n) => {
                    // href is a path the server built (/book/get/{uuid}, an author's or series' page); checked all the same.
                    const href = /^\/(?![\/\\])/.test(n.href) ? n.href : "";
                    const [headline, note] = noticeText(n);
                    const title = href ? el("a", { href, className: "tb-notice-title", textContent: headline }) : el("span", { className: "tb-notice-title", textContent: headline });
                    const dismiss = el("button", { type: "button", className: "tb-notice-dismiss", title: "Dismiss", ariaLabel: `Dismiss: ${headline}` });
                    dismiss.append(icon(CROSS));
                    const item = el(
                        "li",
                        { className: n.seen ? "tb-notice" : "tb-notice is-new" },
                        el("span", { className: "tb-notice-text" }, title, el("span", { className: "tb-notice-note", textContent: note })),
                        dismiss,
                    );
                    dismiss.addEventListener("click", (e) => {
                        e.stopPropagation();
                        dismiss.disabled = true;
                        api("DELETE", `/api/me/notifications/${encodeURIComponent(String(n.id))}`)
                            .then(() => {
                                item.remove();
                                loadNotices(); // the next one up, if any
                            })
                            .catch(() => {
                                dismiss.disabled = false;
                            });
                    });
                    return item;
                }),
            );
            notices.hidden = !notices.childElementCount;
            allNotices.textContent = ns.length > NOTICES_SHOWN ? `All notifications (${ns.length})` : "All notifications";
        };
        const panel = el(
            "div",
            { className: "tb-menu-list tb-sync-panel", hidden: true },
            readingLine.line,
            booksLine.line,
            hcLine.line,
            card,
            nextLine,
            el("hr"),
            notices,
            allNotices,
            el("hr"),
            pause,
            mark(el("a", { href: "/preferences", textContent: "Storage and preferences" }), "preferences"),
        );

        const paint = (): void => {
            const s = readSync(name);
            const a = s.active && Date.now() - s.active.at < STALE_MS ? s.active : null;
            const waiting = (s.waiting ?? 0) + (s.active && !a ? 1 : 0);
            const unsent = unsentBooks(name);
            arc.setAttribute("stroke-dasharray", `${a?.total ? Math.round((a.got / a.total) * 100) : a ? 8 : 0} 100`);
            btn.classList.toggle("is-busy", !!a);
            btn.classList.toggle("has-note", !a && !unseen && (!!s.paused || waiting > 0 || unsent > 0 || !navigator.onLine));
            // The sync icon while a download goes and a moment after; else the bell.
            if (a) {
                if (Date.now() - busyAt >= SYNC_LINGER_MS) busySince = Date.now();
                busyAt = Date.now();
            }
            const lingering = !a && Date.now() - busyAt < SYNC_LINGER_MS;
            // How it went, once it's over (not held for a busy page, nor paused): the run's
            // own word, or a download cut off (its report gone stale) as a failure.
            const over = lingering && !s.held && !s.paused;
            const r = s.lastResult && s.lastResult.at >= busySince ? s.lastResult : null;
            const outcome = !over ? null : s.active ? "failed" : r ? (r.ok ? "ok" : "failed") : null;
            if (outcome) {
                if (result.dataset["outcome"] !== outcome) {
                    result.dataset["outcome"] = outcome;
                    result.replaceChildren(icon(outcome === "ok" ? CHECK : ALERT));
                    result.title = outcome === "ok" ? "Synced" : "A download failed";
                }
                if (!result.isConnected) btn.append(result);
            } else {
                result.remove();
                delete result.dataset["outcome"];
            }
            if (lingering && !lingerTimer) {
                lingerTimer = window.setTimeout(() => {
                    lingerTimer = 0;
                    paint();
                }, SYNC_LINGER_MS - (Date.now() - busyAt) + 50);
            }
            const glyph = a || lingering ? syncIcon : bellIcon;
            if (btn.firstElementChild !== glyph) btn.firstElementChild?.replaceWith(glyph);
            btn.classList.toggle("tb-has-badge", unseen > 0);
            if (unseen) {
                badge.textContent = unseen > 99 ? "99+" : String(unseen);
                if (!badge.isConnected) btn.append(badge);
            } else badge.remove();
            const label = unseen ? `Notifications and sync, ${unseen} new notification${unseen === 1 ? "" : "s"}` : "Notifications and sync";
            if (btn.getAttribute("aria-label") !== label) {
                btn.setAttribute("aria-label", label);
                btn.title = unseen ? `${unseen} new notification${unseen === 1 ? "" : "s"}` : "Notifications and sync";
            }
            if (panel.hidden) return;
            const readingDone = navigator.onLine && !unsent;
            const readingShort = !navigator.onLine ? "Offline" : unsent ? `${unsent} waiting` : "";
            const reading =
                "Your reading progress, bookmarks and highlights, saved to the server so they're on every device you sign in on. " +
                (!navigator.onLine
                    ? `You're offline: ${unsent ? `changes to ${plural(unsent, "book")} are` : "your reading is"} kept here and sent when you're back online.`
                    : unsent
                      ? `Changes to ${plural(unsent, "book")} waiting to be sent.`
                      : "All sent.");
            const booksDone = !a && !s.paused && !waiting;
            const booksShort = a ? "Downloading" : s.paused ? "Paused" : waiting ? `${waiting} waiting` : "";
            const books =
                "Books kept on this device for reading offline: what you're reading and have saved, as set in Storage and preferences. " +
                (a
                ? "One is downloading now."
                : s.paused
                  ? `Book downloads are paused${waiting ? `: ${plural(waiting, "book")} waiting` : ""}.`
                  : s.held && waiting
                    ? `${plural(waiting, "book")} to download once the page has finished loading.`
                  : waiting
                    ? `${plural(waiting, "book")} waiting to download. Downloads go on while a library page is open.`
                    : "All up to date.");
            readingLine.set(readingDone, readingShort, reading);
            booksLine.set(booksDone, booksShort, books);
            hcLine.line.hidden = !hc;
            hcRelink.hidden = hc?.state !== "broken";
            if (hc) {
                const about = "Your reading, sent to your Hardcover account for the books you share with it. ";
                if (hc.state === "broken") {
                    hcLine.set(false, "Link broken", about + "Hardcover no longer accepts this server's sign-in: link your account again to go on sending.", true);
                } else if (hc.failing > 0) {
                    hcLine.set(
                        false,
                        `${hc.failing} failing`,
                        about + `${plural(hc.failing, "book")} couldn't be sent; trying again later.` + (hcError ? ` Last error: ${hcError}` : ""),
                        true,
                    );
                } else if (hc.busy || hc.waiting > 0) {
                    hcLine.set(false, "Syncing…", about + (hc.waiting ? `${plural(hc.waiting, "book")} waiting to be sent.` : "Sending now."));
                } else {
                    hcLine.set(true, "", about + "All sent." + (hc.last_sent ? ` Last sent ${hc.last_sent}.` : ""));
                }
            }
            card.hidden = !a;
            if (a) {
                // A path we build, not a URL from data.
                if (cover.dataset["uuid"] !== a.uuid) {
                    cover.dataset["uuid"] = a.uuid;
                    cover.src = `/cover-thumb/${encodeURIComponent(a.uuid)}`;
                }
                bookTitle.textContent = a.title;
                bookAuthors.textContent = a.authors;
                const pct = a.total ? Math.min(100, Math.round((a.got / a.total) * 100)) : null;
                fill.style.width = `${pct ?? 0}%`;
                bar.classList.toggle("is-unknown", pct === null);
                if (pct === null) bar.removeAttribute("aria-valuenow");
                else bar.setAttribute("aria-valuenow", String(pct));
                bar.setAttribute("aria-label", `${a.title}: ${pct === null ? "starting" : `${pct}%`}`);
                bookDetail.textContent = [
                    a.total ? `${megabytes(a.got)} of ${megabytes(a.total)}` : a.got ? megabytes(a.got) : "Starting…",
                    a.format.toUpperCase(),
                    ...(pct === null ? [] : [`${pct}%`]),
                ].join(" · ");
            }
            const next = a ? (s.next ?? []) : [];
            const more = (s.waiting ?? 0) - next.length;
            nextLine.hidden = !next.length;
            nextLine.textContent = next.length ? `Then: ${next.join(", ")}${more > 0 ? `, and ${plural(more, "more book")}` : ""}.` : "";
            pause.textContent = s.paused ? "Resume book downloads" : "Pause book downloads";
        };
        if (syncToggle) toggles = toggles.filter((t) => t !== syncToggle);
        syncToggle = register(btn, (open) => {
            panel.hidden = !open;
            paint();
            if (!open) {
                stopHcPoll();
                return;
            }
            if (hc && navigator.onLine) askHardcover(); // where it stands now, and the last error
            loadNotices();
            // Shown, so seen: the bell goes.
            if (unseen) {
                api("POST", "/api/me/notifications/seen")
                    .then(() => document.dispatchEvent(new CustomEvent("cf:notifications-seen")))
                    .catch(() => {});
            }
        });
        onSyncChange(name, paint);
        window.addEventListener("online", paint);
        window.addEventListener("offline", paint);
        // A download whose page closed stops reporting: notice it went stale.
        setInterval(() => {
            if (readSync(name).active || !panel.hidden) paint();
        }, 3000);
        paint();
        return {
            node: el("div", { className: "tb-menu tb-sync" }, btn, panel),
            setUnseen(n) {
                if (n === unseen) return;
                unseen = n;
                paint();
            },
            setHardcover(h) {
                if (JSON.stringify(h) === JSON.stringify(hc)) return;
                hc = h;
                paint();
                pollHardcover();
            },
        };
    }
    // Seen (the menu opened, or the notifications page shown, notifications.ts):
    // the count goes from the bar and from what the next page draws first.
    document.addEventListener("cf:notifications-seen", () => {
        syncFor?.setUnseen(0);
        const c = cache.get();
        if (!c) return;
        try {
            const st = JSON.parse(c) as MeState;
            if (!st.notifications) return;
            st.notifications = 0;
            shown = JSON.stringify(st);
            cache.set(shown);
        } catch {
            return;
        }
        snapshot();
    });
    function render(st: MeState): void {
        // Other scripts on the page (the grid's Select) follow this.
        if (st.canEdit) document.documentElement.dataset["canEdit"] = "1";
        else delete document.documentElement.dataset["canEdit"];
        document.dispatchEvent(new CustomEvent("cf:me"));
        if (userToggle) toggles = toggles.filter((t) => t !== userToggle);
        userToggle = null;
        if (!st.username) {
            const next = encodeURIComponent(location.pathname + location.search);
            const signIn = el("a", { className: "tb-btn", href: `/login?next=${next}`, textContent: "Sign in" });
            signIn.dataset["next"] = ""; // the inline script points it back at its own page
            const prefs = mark(el("a", { className: "tb-icon tb-shortcut", href: "/preferences", title: "Preferences", ariaLabel: "Preferences" }), "preferences");
            prefs.append(icon(SLIDERS));
            userEl.replaceChildren(prefs, signIn);
            snapshot();
            return;
        }
        const name = st.username;
        // Reading done here signed out, not offered to this account yet or
        // put off: say so, under the bar (guest-data.ts, /account/import).
        const guestBooks = offerDeclinedForGood(name) ? 0 : guestBookCount();
        showGuestOffer(name, guestBooks);
        const shortcut = (href: string, label: string, path: string, key: string): HTMLAnchorElement => {
            const a = mark(el("a", { className: "tb-icon tb-shortcut", href, title: label, ariaLabel: label }), key);
            a.append(icon(path));
            return a;
        };

        // Books from the watch folder to review: in the bar while any wait.
        const imports = st.canEdit ? (st.imports ?? null) : null;
        let importsShortcut: HTMLAnchorElement | null = null;
        if (imports) {
            importsShortcut = shortcut("/import", `${imports} new book${imports === 1 ? "" : "s"} to review`, INBOX, "imports");
            importsShortcut.classList.add("tb-has-badge");
            importsShortcut.append(el("span", { className: "tb-badge", textContent: imports > 99 ? "99+" : String(imports) }));
        }

        const btn = el("button", { type: "button", className: "tb-user-btn", title: name, ariaLabel: `Account menu for ${name}` });
        btn.setAttribute("aria-haspopup", "true");
        btn.setAttribute("aria-expanded", "false");
        btn.append(
            el("span", { className: "tb-avatar", textContent: name.slice(0, 1) }),
            el("span", { className: "tb-user-name", textContent: name }),
        );

        const logout = el("form", { method: "post", action: "/logout" }, el("button", { type: "submit", textContent: "Sign out" }));
        logout.addEventListener("submit", () => {
            cache.set(null);
            forgetSynced(name);
            void keepOnlyUser(null).catch(() => {});
            void forgetOfflineReading();
        });
        // Sections, with a rule between those that have anything in them:
        // reading, library tools, admin, then the account itself.
        const sections: HTMLElement[][] = [
            [
                mark(el("a", { href: "/me/favorites", textContent: "★ Favorites" }), "favorites"),
                mark(el("a", { href: "/me/saved", textContent: "Saved" }), "saved"),
                mark(el("a", { href: "/me/reading", textContent: "Reading" }), "reading"),
                mark(el("a", { href: "/me/paused", textContent: "Paused" }), "paused"),
                mark(el("a", { href: "/me/finished", textContent: "Finished" }), "finished"),
                mark(el("a", { href: "/me/dropped", textContent: "Dropped" }), "dropped"),
                mark(el("a", { href: "/me/stats", textContent: "Reading stats" }), "stats"),
            ],
            [
                ...(st.canEdit ? [mark(el("a", { href: "/upload", textContent: "Upload books" }), "upload")] : []),
                ...(imports !== null ? [mark(el("a", { href: "/import", textContent: imports ? `Review imports (${imports})` : "Review imports" }), "imports")] : []),
                ...(st.canEdit ? [mark(el("a", { href: "/library/edit", textContent: "Bulk edit" }), "library-edit")] : []),
                ...(st.canEdit ? [mark(el("a", { href: "/recycle-bin", textContent: "Recycling Bin" }), "recycle-bin")] : []),
                ...(st.canEdit ? [mark(el("a", { href: "/missing", textContent: "Missing books" }), "missing")] : []),
            ],
            [
                ...(st.canAdmin ? [mark(el("a", { href: "/admin/repairs", textContent: "Library repairs" }), "library-repairs")] : []),
                ...(st.isAdmin ? [el("a", { href: "/admin/users", textContent: "Manage users" })] : []),
                ...(st.isAdmin ? [el("a", { href: "/admin/activity", textContent: "Activity" })] : []),
                ...(st.isAdmin ? [mark(el("a", { href: "/admin/discover", textContent: "Home page tags" }), "admin-discover")] : []),
                ...(st.isAdmin ? [mark(el("a", { href: "/admin/server", textContent: "Server" }), "admin-server")] : []),
            ],
            [
                ...(guestBooks ? [el("a", { href: "/account/import", textContent: "Reading on this device" })] : []),
                el("a", { href: "/account", textContent: "Account" }),
                mark(el("a", { href: "/preferences", textContent: "Preferences" }), "preferences"),
                logout,
            ],
        ];
        const list = el(
            "div",
            { className: "tb-menu-list", hidden: true },
            ...sections.filter((s) => s.length).flatMap((s, i) => (i ? [el("hr"), ...s] : s)),
        );
        userToggle = register(btn, (open) => {
            list.hidden = !open;
        });

        userEl.replaceChildren(
            ...(importsShortcut ? [importsShortcut] : []),
            ...(st.canEdit ? [shortcut("/upload", "Upload books", UPLOAD, "upload")] : []),
            shortcut("/me/saved", "Saved", BOOKMARK, "saved"),
            shortcut("/me/favorites", "Favorites", STAR, "favorites"),
            shortcut("/me/reading", "Reading", BOOK, "reading"),
            syncButton(name, st.notifications ?? 0, st.hardcover ?? null),
            el("div", { className: "tb-menu" }, btn, list),
        );
        snapshot();
    }

    /**
     * A node of the snapshot: text, or an element (t its tag, s: 1 for SVG,
     * a its attributes, c its children). topbar.html's inline script builds
     * it back with createElement, only from the tags and attributes nav.ts
     * itself makes.
     */
    type NavNode = string | { t: string; s?: 1; a: [string, string][]; c: NavNode[] };
    function navTree(n: Node): NavNode[] {
        if (n.nodeType === Node.TEXT_NODE) return [n.textContent ?? ""];
        if (!(n instanceof Element)) return [];
        return [
            {
                t: n.localName,
                ...(n.namespaceURI === "http://www.w3.org/2000/svg" ? { s: 1 as const } : {}),
                a: [...n.attributes].map((a): [string, string] => [a.name, a.value]),
                c: [...n.childNodes].flatMap(navTree),
            },
        ];
    }

    /** Keep the user part for the next page's inline script, without this page's highlight. */
    function snapshot(): void {
        const copy = userEl.cloneNode(true) as HTMLElement;
        for (const a of copy.querySelectorAll("[aria-current]")) a.removeAttribute("aria-current");
        // Not the sync menu's insides (filled again once drawn; notification
        // titles needn't sit in storage) nor its ring: only the tags
        // topbar.html's script rebuilds.
        for (const p of copy.querySelectorAll(".tb-sync-panel")) p.replaceChildren();
        for (const r of copy.querySelectorAll(".tb-sync-ring")) r.remove();
        try {
            localStorage.setItem(NAV_KEY, JSON.stringify([...copy.childNodes].flatMap(navTree)));
        } catch {
            /* storage blocked */
        }
    }

    let shown = cache.get();
    if (shown) {
        try {
            render(JSON.parse(shown) as MeState);
        } catch {
            shown = null;
        }
    }

    meRequest()
        .then(({ ok, body }) => {
            const u = ok ? body.user : null;
            const st: MeState = {
                username: u?.username ?? null,
                isAdmin: !!u?.is_admin,
                canEdit: !!body.can_edit_library,
                canAdmin: !!body.can_admin_library,
                imports: body.pending_imports ?? null,
                notifications: body.notifications ?? 0,
                hardcover: body.hardcover ?? null,
            };
            const json = JSON.stringify(st);
            cache.set(json);
            // Books kept for offline reading belong to whoever read them
            // (only on a real answer: an error isn't a sign-out).
            if (ok) void keepOnlyUser(st.username).catch(() => {});
            // The Reading list and covers for the offline page (offline-books.ts):
            // on the Reading page every time, elsewhere now and then.
            if (ok && st.username) {
                const onReading = location.pathname === "/me/reading";
                // The account's preferences (prefs.ts), or the guest's for an account just made here.
                void settleAccountPrefs(st.username, body.prefs);
                // Reading changes made where the server couldn't be reached: sent now, not when the book is next opened.
                startSendingPending(st.username);
                whenIdle(() => void refreshOfflineReading(onReading).catch(() => {}));
            } else if (ok) void forgetOfflineReading();
            if (json !== shown) render(st);
        })
        .catch(() => {
            if (!shown) userEl.replaceChildren(); // offline, nothing known: drop the throbber
        });
}

/** Run fn once the page has settled (it fetches, and shouldn't compete with the page's own requests). */
function whenIdle(fn: () => void): void {
    if ("requestIdleCallback" in window) requestIdleCallback(fn, { timeout: 5000 });
    else setTimeout(fn, 2000);
}

// Last, so the constants above are initialized (init may render synchronously).
const bar = document.querySelector<HTMLElement>(".site-bar");
if (bar) init(bar);
registerWorker();
