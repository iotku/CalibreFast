/**
 * The shared top bar (templates/topbar.html):
 *   - fills the user part: "Sign in", or ★ Favorites, Reading and a user
 *     menu (pages stay identical for everyone and cacheable, so this asks
 *     /api/me)
 *   - opens/closes the ☰ menu (narrow screens), the user menu and the
 *     search box (narrow screens); a click outside or Escape closes them
 */
import { keepOnlyUser, registerWorker } from "./book-cache.js";
import { el, withPageScheme } from "./dom.js";

// Addresses the server wrote for this site (e.g. the device sync server on
// the account page): use the page's scheme, which a proxy may hide from it.
for (const code of document.querySelectorAll<HTMLElement>(".same-origin-url")) {
    code.textContent = withPageScheme(code.textContent ?? "");
}
import type { MeResponse } from "./types.js";


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

function init(bar: HTMLElement): void {
    const user = bar.querySelector<HTMLElement>(".tb-user");
    const active = user?.dataset["active"] ?? "";
    const current = (name: string): Partial<HTMLAnchorElement> =>
        active === name ? ({ ariaCurrent: "page" } as Partial<HTMLAnchorElement>) : {};

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

    // ---- user part ----------------------------------------------------------
    //
    // The last answer from /api/me is kept in localStorage and shown straight
    // away, so on repeat visits the user part doesn't pop in; the request then
    // corrects it if anything changed. First visit: the template's throbber
    // shows until the request finishes.

    if (!user) return;
    const userEl: HTMLElement = user;
    const CACHE_KEY = "cf-me";
    interface MeState {
        username: string | null;
        isAdmin: boolean;
        canEdit: boolean;
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
                if (v === null) localStorage.removeItem(CACHE_KEY);
                else localStorage.setItem(CACHE_KEY, v);
            } catch {
                /* storage blocked: no instant render, the throbber still works */
            }
        },
    };
    // Signing in or out changes the answer: don't show a stale one next.
    if (/^\/(login|register|l\/)/.test(location.pathname)) cache.set(null);

    let userToggle: Toggle | null = null;
    function render(st: MeState): void {
        if (userToggle) toggles = toggles.filter((t) => t !== userToggle);
        userToggle = null;
        if (!st.username) {
            const next = encodeURIComponent(location.pathname + location.search);
            userEl.replaceChildren(el("a", { className: "tb-btn", href: `/login?next=${next}`, textContent: "Sign in" }));
            return;
        }
        const name = st.username;
        const shortcut = (href: string, label: string, path: string, key: string): HTMLAnchorElement => {
            const a = el("a", { className: "tb-icon tb-shortcut", href, title: label, ariaLabel: label, ...current(key) });
            a.append(icon(path));
            return a;
        };

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
            void keepOnlyUser(null).catch(() => {});
        });
        const list = el(
            "div",
            { className: "tb-menu-list", hidden: true },
            el("a", { href: "/me/favorites", textContent: "★ Favorites", ...current("favorites") }),
            el("a", { href: "/me/wanted", textContent: "Want to read", ...current("wanted") }),
            el("a", { href: "/me/reading", textContent: "Reading", ...current("reading") }),
            el("a", { href: "/me/finished", textContent: "Finished", ...current("finished") }),
            el("hr"),
            ...(st.canEdit ? [el("a", { href: "/upload", textContent: "Upload books", ...current("upload") })] : []),
            ...(st.isAdmin ? [el("a", { href: "/admin/users", textContent: "Manage users" })] : []),
            el("a", { href: "/account", textContent: "Account" }),
            logout,
        );
        userToggle = register(btn, (open) => {
            list.hidden = !open;
        });

        userEl.replaceChildren(
            ...(st.canEdit ? [shortcut("/upload", "Upload books", UPLOAD, "upload")] : []),
            shortcut("/me/wanted", "Want to read", BOOKMARK, "wanted"),
            shortcut("/me/favorites", "Favorites", STAR, "favorites"),
            shortcut("/me/reading", "Reading", BOOK, "reading"),
            el("div", { className: "tb-menu" }, btn, list),
        );
    }

    let shown = cache.get();
    if (shown) {
        try {
            render(JSON.parse(shown) as MeState);
        } catch {
            shown = null;
        }
    }

    fetch("/api/me", { credentials: "same-origin" })
        .then(async (r) => ({ ok: r.ok, body: (await r.json()) as MeResponse }))
        .then(({ ok, body }) => {
            const u = ok ? body.user : null;
            const st: MeState = { username: u?.username ?? null, isAdmin: !!u?.is_admin, canEdit: !!body.can_edit_library };
            const json = JSON.stringify(st);
            cache.set(json);
            // Books kept for offline reading belong to whoever read them.
            void keepOnlyUser(st.username).catch(() => {});
            if (json !== shown) render(st);
        })
        .catch(() => {
            if (!shown) userEl.replaceChildren(); // offline, nothing known: drop the throbber
        });
}

// Last, so the constants above are initialized (init may render synchronously).
const bar = document.querySelector<HTMLElement>(".site-bar");
if (bar) init(bar);
registerWorker();
