/**
 * The book page's "Sync to Hardcover" split button (book-user-actions.html,
 * sync-split.ts): sharing the book with the user's Hardcover account, how
 * its sync stands, and, when Hardcover couldn't find it or found the wrong
 * book, choosing the right one (from the menu, or the button itself when
 * it wasn't found). The choice is the user's own, for their sync only
 * (hardcover_book.go). A placeholder's page has it too
 * (placeholder-hardcover.ts): Saved, it's sent as want to read, and its
 * Hardcover book is the library's, not the user's: the first to choose
 * it names it for everyone, and after that only an editor changes it
 * (hardcover_placeholder.go). Built with el()/textContent: titles and
 * errors come from Hardcover and the library.
 */
import { api, el, errorMessage } from "./dom.js";
import { paintSync, SYNC_ICONS, type SyncLook } from "./sync-split.js";

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
    /** A placeholder: a book not in the library yet. */
    placeholder?: boolean;
    /** The user may choose its Hardcover book (false: a placeholder's is set, and they're no editor). */
    can_choose: boolean;
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

/** The toggle's look for how the sync stands. */
function look(st: Status): SyncLook {
    const pressed = st.shared;
    const off = st.all ? "Leave this book out of sharing every book with Hardcover" : "Stop sending this book to Hardcover";
    switch (st.state) {
        case "off":
            return { icon: SYNC_ICONS.sync, label: "Sync to Hardcover", title: "Send this book's reading status to your Hardcover account", on: false, pressed };
        case "not_found":
            if (!st.can_choose)
                return { icon: SYNC_ICONS.problem, label: "Not found on Hardcover", title: off, on: true, pressed, problem: true };
            return { icon: SYNC_ICONS.problem, label: "Find on Hardcover", title: "Hardcover couldn't find this book: choose it", on: false, problem: true };
        case "failing":
            return { icon: SYNC_ICONS.problem, label: "Hardcover sync failing", title: off, on: true, pressed, problem: true };
        case "waiting":
            return { icon: SYNC_ICONS.sync, label: "Syncing to Hardcover", title: off, on: true, pressed };
        case "no_status":
            return { icon: SYNC_ICONS.sync, label: "Sharing with Hardcover", title: off, on: true, pressed };
        case "sent":
            if (st.placeholder) return { icon: SYNC_ICONS.sync, label: "On your Want to Read", title: off, on: true, pressed };
            return { icon: SYNC_ICONS.sync, label: "Synced to Hardcover", title: off, on: true, pressed };
        default:
            return { icon: SYNC_ICONS.sync, label: "Synced to Hardcover", title: off, on: true, pressed };
    }
}

/** The menu: share or not, then what can be done about the match. */
function menuOptions(st: Status, picking: boolean): HTMLOptionElement[] {
    const opt = (value: string, label: string, extra: Partial<HTMLOptionElement> = {}): HTMLOptionElement =>
        el("option", { value, textContent: label, ...extra });
    const opts = [
        opt("on", "Sync to Hardcover", { selected: st.shared }),
        opt("off", "Don't sync to Hardcover", { selected: !st.shared }),
    ];
    if (st.shared && st.can_choose) {
        const label = picking
            ? "Close the Hardcover search"
            : st.state === "not_found" || (st.placeholder && !st.match)
              ? "Find it on Hardcover…"
              : st.match
                ? st.placeholder
                    ? "Change its Hardcover book…"
                    : "Wrong book? Choose another…"
                : "Choose the book on Hardcover…";
        opts.push(opt("pick", label));
        if (st.match?.chosen && !st.placeholder) opts.push(opt("reset", "Use the library's match"));
    }
    if (st.all) opts.push(opt("account", "Sharing for every book…"));
    return opts;
}

/** Wires the controls; the answer refreshes the status (after a change elsewhere on the page). */
export function wireHardcoverBook(controls: HTMLElement, uuid: string): (() => void) | null {
    const btn = controls.querySelector<HTMLButtonElement>("button[data-hardcover-toggle]");
    const menu = controls.querySelector<HTMLSelectElement>("select[data-hardcover-menu]");
    const out = controls.querySelector<HTMLElement>("[data-hardcover-status]");
    if (!btn || !menu || !out) return null;
    const base = `/api/me/books/${encodeURIComponent(uuid)}/hardcover`;
    let picking = false;
    /**
     * Just shared a placeholder: once it's known whether Hardcover found it
     * from its ISBN, search for it if it didn't (`final`: the last look).
     */
    let autoFind = false;
    /** How it stands, as last painted: what the button and menu act on. */
    let current: Status | null = null;
    /** Before the first answer: as the server rendered it. */
    const known = (): Status =>
        current ?? {
            shared: btn.getAttribute("aria-pressed") === "true",
            all: !!menu.querySelector('option[value="account"]'),
            state: btn.classList.contains("is-on") ? "waiting" : "off",
            query: "",
            can_choose: true,
        };

    const paint = (st: Status, final = false): void => {
        current = st;
        let search = false;
        if (autoFind && st.placeholder && st.shared && (final || st.state !== "waiting")) {
            autoFind = false;
            if (!st.match && st.can_choose) picking = search = true;
        }
        paintSync(btn, look(st));
        menu.replaceChildren(...menuOptions(st, picking));
        menu.value = st.shared ? "on" : "off";
        const line = el("span", { className: "book-hardcover-note" });
        const parts: (Node | string)[] = [];
        const matchText = (): (Node | string)[] =>
            st.match ? [" as ", bookLink(st.match.slug, st.match.title || `book ${st.match.book_id}`)] : [];
        switch (st.state) {
            case "off":
                parts.push(st.all ? "Left out: you share every book but this one" : "Nothing about this book is sent to Hardcover");
                break;
            case "no_status":
                parts.push(st.placeholder ? "Shared. Save it to send it as want to read" : "Shared. Set want to read, reading or read to send it");
                if (st.placeholder && st.match) parts.push(...matchText());
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
                if (st.placeholder && !st.can_choose) parts.push(" An editor can set its Hardcover book.");
                break;
        }
        if (st.all && st.state !== "off") parts.push(" (you share every book: ", el("a", { href: "/account#hardcover", className: "meta-link", textContent: "change" }), ")");
        if (st.shared && st.match?.chosen) parts.push(". You chose this match");
        line.append(...parts);
        const kids: Node[] = [line];
        if (st.shared && picking) kids.push(picker(st));
        out.replaceChildren(...kids);
        if (search) out.querySelector<HTMLFormElement>(".book-hardcover-picker")?.requestSubmit();
    };

    const share = async (on: boolean): Promise<void> => {
        btn.disabled = true;
        menu.disabled = true;
        try {
            await api("PUT", base, { share: on });
            if (!on) picking = false;
            autoFind = on && !!current?.placeholder;
        } catch (err) {
            out.replaceChildren(el("span", { className: "book-hardcover-note book-hardcover-problem", textContent: errorMessage(err) }));
        } finally {
            btn.disabled = false;
            menu.disabled = false;
        }
        refresh();
        if (on) setTimeout(() => refresh(true), 3000);
    };

    const togglePicker = (): void => {
        if (!current) return;
        picking = !picking;
        paint(current);
        if (picking) out.querySelector<HTMLInputElement>(".book-hardcover-q")?.focus();
    };

    btn.addEventListener("click", () => {
        const st = known();
        if (st.state === "not_found" && st.can_choose) return togglePicker();
        void share(!st.shared);
    });

    menu.addEventListener("change", () => {
        const st = known();
        const choice = menu.value;
        menu.value = st.shared ? "on" : "off"; // the menu shows how it stands; a choice only acts
        switch (choice) {
            case "on":
            case "off":
                if ((choice === "on") !== st.shared) void share(choice === "on");
                break;
            case "pick":
                togglePicker();
                break;
            case "reset":
                void change(() => api<Status>("DELETE", `${base}/match`));
                break;
            case "account":
                location.assign("/account#hardcover");
                break;
        }
    });

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
            textContent: st.placeholder
                ? `Searching sends what you type to Hardcover. The book you pick becomes this placeholder's Hardcover book, for everyone who syncs it${st.match ? "" : "; after that, only an editor can change it"}. If the wrong book was already sent, remove it on Hardcover.`
                : "Searching sends what you type to Hardcover. The match is yours: it changes what's sent to your account only, not the library. If the wrong book was already sent, remove it on Hardcover.",
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

    const refresh = (final = false): void => {
        api<Status>("GET", base).then(
            (st) => paint(st, final),
            () => {
                /* keep what the server rendered */
            },
        );
    };

    refresh();
    return refresh;
}
