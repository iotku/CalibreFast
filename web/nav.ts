/**
 * The shared top bar (templates/topbar.html):
 *   - fills the user part: "Sign in", or ★ Favorites, Reading and a user
 *     menu (pages stay identical for everyone and cacheable, so this asks
 *     /api/me)
 *   - opens/closes the ☰ menu (narrow screens), the user menu and the
 *     search box (narrow screens); a click outside or Escape closes them
 */
import { el } from "./dom.js";
import type { MeResponse } from "./types.js";

const bar = document.querySelector<HTMLElement>(".site-bar");
if (bar) init(bar);

function icon(path: string): SVGSVGElement {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("width", "18");
    svg.setAttribute("height", "18");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "2");
    svg.setAttribute("stroke-linejoin", "round");
    svg.setAttribute("aria-hidden", "true");
    const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
    p.setAttribute("d", path);
    svg.append(p);
    return svg;
}
const STAR = "M12 3l2.8 5.7 6.2.9-4.5 4.4 1.1 6.2L12 17.3 6.4 20.2l1.1-6.2L3 9.6l6.2-.9z";
const BOOK = "M2 4h6a4 4 0 0 1 4 4v12a3 3 0 0 0-3-3H2zM22 4h-6a4 4 0 0 0-4 4v12a3 3 0 0 1 3-3h7z";

function init(bar: HTMLElement): void {
    const user = bar.querySelector<HTMLElement>(".tb-user");
    const active = user?.dataset["active"] ?? "";
    const current = (name: string): Partial<HTMLAnchorElement> =>
        active === name ? ({ ariaCurrent: "page" } as Partial<HTMLAnchorElement>) : {};

    // ---- open/close ---------------------------------------------------------

    /** Every toggle: its button, and what "open" means for it. */
    const toggles: { btn: HTMLElement; set(open: boolean): void }[] = [];
    function register(btn: HTMLElement | null, set: (open: boolean) => void): void {
        if (!btn) return;
        const t = {
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

    if (!user) return;
    fetch("/api/me", { credentials: "same-origin" })
        .then(async (r) => ({ ok: r.ok, body: (await r.json()) as MeResponse }))
        .then(({ ok, body }) => {
            const u = ok ? body.user : null;
            if (!u) {
                const next = encodeURIComponent(location.pathname + location.search);
                user.replaceChildren(el("a", { className: "tb-btn", href: `/login?next=${next}`, textContent: "Sign in" }));
                return;
            }
            const shortcut = (href: string, label: string, path: string, name: string): HTMLAnchorElement => {
                const a = el("a", { className: "tb-icon tb-shortcut", href, title: label, ariaLabel: label, ...current(name) });
                a.append(icon(path));
                return a;
            };

            const btn = el("button", { type: "button", className: "tb-user-btn", title: u.username, ariaLabel: `Account menu for ${u.username}` });
            btn.setAttribute("aria-haspopup", "true");
            btn.setAttribute("aria-expanded", "false");
            btn.append(
                el("span", { className: "tb-avatar", textContent: u.username.slice(0, 1) }),
                el("span", { className: "tb-user-name", textContent: u.username }),
            );

            const logout = el("form", { method: "post", action: "/logout" }, el("button", { type: "submit", textContent: "Sign out" }));
            const list = el(
                "div",
                { className: "tb-menu-list", hidden: true },
                el("a", { href: "/me/favorites", textContent: "★ Favorites", ...current("favorites") }),
                el("a", { href: "/me/reading", textContent: "Reading", ...current("reading") }),
                el("a", { href: "/me/finished", textContent: "Finished", ...current("finished") }),
                el("hr"),
                ...(body.can_edit_library ? [el("a", { href: "/upload", textContent: "Upload books" })] : []),
                ...(u.is_admin ? [el("a", { href: "/admin/users", textContent: "Manage users" })] : []),
                el("a", { href: "/account", textContent: "Account" }),
                logout,
            );
            register(btn, (open) => {
                list.hidden = !open;
            });

            user.replaceChildren(
                shortcut("/me/favorites", "Favorites", STAR, "favorites"),
                shortcut("/me/reading", "Reading", BOOK, "reading"),
                el("div", { className: "tb-menu" }, btn, list),
            );
        })
        .catch(() => {}); // offline: leave it empty
}
