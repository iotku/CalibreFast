// Fills #user-nav in the top bar: "Sign in", or the signed-in user's links.
// Pages stay identical for everyone (cacheable); this asks /api/me.
const nav = document.getElementById("user-nav");

if (nav) {
    fetch("/api/me", { credentials: "same-origin" })
        .then((r) => r.json().then((body) => ({ ok: r.ok, body })))
        .then(({ ok, body }) => {
            const link = (href: string, text: string, cls = "meta-link"): HTMLAnchorElement =>
                Object.assign(document.createElement("a"), { href, textContent: text, className: cls });
            if (!ok || !body.user) {
                const next = encodeURIComponent(location.pathname + location.search);
                nav.replaceChildren(link("/shelves", "Shelves"), link(`/login?next=${next}`, "Sign in"));
                return;
            }
            const items = [
                link("/me/favorites", "★ Favorites"),
                link("/me/reading", "Reading"),
                link("/shelves", "Shelves"),
                link("/account", body.user.username, "meta-link user-nav-name"),
            ];
            nav.replaceChildren(...items);
        })
        .catch(() => {}); // offline: leave the menu empty
}
