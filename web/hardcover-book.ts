/**
 * The book page's "Track on Hardcover" (book-user-actions.html): sharing
 * the book with the user's Hardcover account, how its sync stands, and,
 * when Hardcover couldn't find it or found the wrong book, choosing the
 * right one. The choice is the user's own, for their sync only
 * (hardcover_book.go). Built with el()/textContent: titles and errors
 * come from Hardcover and the library.
 */
import { api, el, errorMessage } from "./dom.js";

interface Match {
    book_id: number;
    slug?: string;
    title?: string;
    chosen: boolean;
}

interface Status {
    shared: boolean;
    all: boolean;
    state: "off" | "no_status" | "waiting" | "sent" | "failing" | "not_found";
    error?: string;
    retry_at?: number;
    sent_at?: number;
    match?: Match;
    query: string;
}

interface Found {
    book_id: number;
    slug?: string;
    title: string;
    authors?: string;
    year?: string;
}

/** A book's page on Hardcover, from its slug (checked on the server). */
function bookLink(slug: string | undefined, title: string): HTMLElement {
    if (!slug || !/^[a-z0-9][a-z0-9-]*$/.test(slug)) return el("b", { textContent: title });
    return el("a", {
        href: `https://hardcover.app/books/${encodeURIComponent(slug)}`,
        target: "_blank",
        rel: "noopener noreferrer",
        className: "meta-link",
        textContent: title,
    });
}

function time(ms: number): string {
    return new Date(ms).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

/** Wires the controls; the answer refreshes the status (after a change elsewhere on the page). */
export function wireHardcoverBook(controls: HTMLElement, uuid: string): (() => void) | null {
    const box = controls.querySelector<HTMLInputElement>("input[data-hardcover-share]");
    const out = controls.querySelector<HTMLElement>("[data-hardcover-status]");
    if (!box || !out) return null;
    const base = `/api/me/books/${encodeURIComponent(uuid)}/hardcover`;
    let picking = false;

    const paint = (st: Status): void => {
        box.checked = st.shared;
        box.disabled = st.all;
        const line = el("span", { className: "book-hardcover-note" });
        const parts: (Node | string)[] = [];
        const matchText = (): (Node | string)[] =>
            st.match ? [" as ", bookLink(st.match.slug, st.match.title || `book ${st.match.book_id}`)] : [];
        switch (st.state) {
            case "off":
                parts.push("Nothing about this book is sent to Hardcover");
                break;
            case "no_status":
                parts.push("Shared. Set want to read, reading or read to send it");
                break;
            case "waiting":
                parts.push("Waiting to be sent", ...matchText());
                break;
            case "sent":
                parts.push("On your Hardcover", ...matchText());
                break;
            case "failing":
                parts.push(`Couldn't send: ${st.error ?? "unknown error"}.`);
                if (st.retry_at) parts.push(` Trying again at ${time(st.retry_at)}.`);
                break;
            case "not_found":
                line.classList.add("book-hardcover-problem");
                parts.push("Hardcover couldn't find this book from its ISBN or Hardcover link, so nothing was sent.");
                break;
        }
        if (st.all && st.state !== "off") parts.push(" (you share every book: ", el("a", { href: "/account#hardcover", className: "meta-link", textContent: "change" }), ")");
        line.append(...parts);
        const kids: Node[] = [line];
        if (st.shared) {
            const label =
                st.state === "not_found" ? "Find it on Hardcover" : st.match ? "Wrong book?" : "Choose the book on Hardcover";
            const toggle = el("button", { type: "button", className: "link-button", textContent: label });
            toggle.setAttribute("aria-expanded", String(picking));
            toggle.addEventListener("click", () => {
                picking = !picking;
                paint(st);
            });
            kids.push(toggle);
            if (st.match?.chosen) {
                const reset = el("button", { type: "button", className: "link-button", textContent: "Use the library's match" });
                reset.addEventListener("click", () => void change(() => api<Status>("DELETE", `${base}/match`)));
                kids.push(el("span", { className: "book-hardcover-note" }, "You chose this match. "), reset);
            }
            if (picking) kids.push(picker(st));
        }
        out.replaceChildren(...kids);
    };

    const picker = (st: Status): HTMLElement => {
        const input = el("input", {
            type: "search",
            value: st.query,
            placeholder: "Title, or a hardcover.app/books/… link",
            maxLength: 200,
            className: "book-hardcover-q",
        });
        input.setAttribute("aria-label", "Search Hardcover");
        const go = el("button", { type: "submit", className: "btn-standard", textContent: "Search" });
        const results = el("ul", { className: "book-hardcover-results" });
        const msg = el("p", { className: "book-hardcover-note" });
        msg.setAttribute("role", "status");
        const form = el("form", { className: "book-hardcover-picker" }, input, go);
        form.addEventListener("submit", async (e) => {
            e.preventDefault();
            go.disabled = true;
            msg.textContent = "Searching Hardcover…";
            results.replaceChildren();
            try {
                const found = await api<Found[]>("GET", `${base}/search?q=${encodeURIComponent(input.value)}`);
                msg.textContent = found.length ? "Pick the book this is:" : "Nothing found. Try fewer words, or paste the book's link from hardcover.app.";
                results.replaceChildren(
                    ...found.map((f) => {
                        const pick = el("button", { type: "button", className: "btn-standard", textContent: "This one" });
                        pick.addEventListener("click", () => {
                            picking = false;
                            void change(() => api<Status>("PUT", `${base}/match`, { book_id: f.book_id }));
                        });
                        const meta = [f.authors, f.year].filter(Boolean).join(", ");
                        return el("li", {}, bookLink(f.slug, f.title), meta ? el("span", { className: "auth-muted", textContent: ` ${meta}` }) : "", " ", pick);
                    }),
                );
            } catch (err) {
                msg.textContent = errorMessage(err);
            } finally {
                go.disabled = false;
            }
        });
        const note = el("p", {
            className: "book-hardcover-note",
            textContent:
                "Searching sends what you type to Hardcover. The match is yours: it changes what's sent to your account only, not the library. If the wrong book was already sent, remove it on Hardcover.",
        });
        return el("div", { className: "book-hardcover-pick" }, form, msg, results, note);
    };

    const change = async (call: () => Promise<Status>): Promise<void> => {
        try {
            paint(await call());
            setTimeout(refresh, 3000); // the worker sends it in a moment
        } catch (err) {
            out.prepend(el("span", { className: "book-hardcover-note book-hardcover-problem", textContent: errorMessage(err) }));
        }
    };

    const refresh = (): void => {
        api<Status>("GET", base).then(paint, () => {
            /* keep what the server rendered */
        });
    };

    box.addEventListener("change", async () => {
        const share = box.checked;
        box.disabled = true;
        try {
            await api("PUT", base, { share });
            if (!share) picking = false;
        } catch (err) {
            box.checked = !share;
            out.replaceChildren(el("span", { className: "book-hardcover-note book-hardcover-problem", textContent: errorMessage(err) }));
        } finally {
            box.disabled = false;
        }
        refresh();
        if (share) setTimeout(refresh, 3000);
    });
    refresh();
    return refresh;
}
