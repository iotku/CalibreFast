// The metadata editor's cover dialog, opened from the cover in the banner:
// every cover the book could have, large enough to compare quality.
//   - Choices: the current cover and the first pages of the book's PDF and
//     EPUB (loaded when the dialog first opens), an uploaded image, and Open
//     Library's cover for the ISBN in the lookup box. That one goes to another
//     site (through the server), so only on a click.
//   - Clicking a choice shows it large; a second click on another shows the
//     two side by side. "Actual pixels" shows them pixel for pixel.
//   - "Use this cover" saves at once (covers don't wait for Apply). Revert
//     brings back the cover before the last change.

import {
    type CoverChoice, currentCover, epubPages, fetchCoverStatus, fileChoice, formatBytes, imageChoice, pdfPages, revertCover,
} from "./cover-sources.js";
import { api, closeOnBackdropClick, el, errorMessage, maybeId } from "./dom.js";
import type { CoverCandidate, LookupResult } from "./types.js";

export interface CoverDialogOptions {
    uuid: string;
    /** The banner's cover button: it opens the dialog. */
    opener: HTMLElement;
    /** The editor's ISBN lookup box; null with -no-lookups (no online covers). */
    isbnInput: HTMLInputElement | null;
    /** After the cover changed (saved or reverted), with a message to show. */
    onChanged(message: string): void;
}

/** At most this many choices are shown large at once (side by side). */
const COMPARE = 2;

export function mountCoverDialog({ uuid, opener, isbnInput, onChanged }: CoverDialogOptions): void {
    const dialogEl = maybeId("cover-dialog", HTMLDialogElement);
    const listEl = maybeId("cover-choices");
    const viewEl = maybeId("cover-view");
    const viewItemsEl = maybeId("cover-view-items");
    const statusEl = maybeId("cover-status");
    const fileEl = maybeId("cover-file", HTMLInputElement);
    const onlineEl = maybeId("cover-online", HTMLButtonElement);
    const revertEl = maybeId("cover-revert", HTMLButtonElement);
    const actualEl = maybeId("cover-actual", HTMLButtonElement);
    if (!dialogEl || !listEl || !viewEl || !viewItemsEl || !statusEl || !fileEl || !onlineEl || !revertEl || !actualEl) return;
    // Non-null from here on (also inside the functions below).
    const dialog: HTMLDialogElement = dialogEl;
    const list: HTMLElement = listEl;
    const view: HTMLElement = viewEl;
    const viewItems: HTMLElement = viewItemsEl;
    const status: HTMLElement = statusEl;
    const onlineBtn: HTMLButtonElement = onlineEl;
    const revertBtn: HTMLButtonElement = revertEl;
    const actualBtn: HTMLButtonElement = actualEl;

    const say = (text: string, isError = false): void => {
        status.textContent = text;
        status.classList.toggle("error", isError);
    };
    const describe = (c: CoverChoice): string => `${c.width} × ${c.height} px · ${formatBytes(c.bytes)}`;

    // ---- Choices and the large view -------------------------------------------

    const cards = new Map<CoverChoice, HTMLButtonElement>();
    let selected: CoverChoice[] = [];
    let actual = false; // pixel for pixel
    let busy = false;

    function add(choice: CoverChoice | null, { first = false } = {}): void {
        if (!choice) return;
        const card = el(
            "button",
            { type: "button", className: "cover-card", title: `Show ${choice.label} large` },
            el("img", { src: choice.src, alt: choice.label }),
            el("span", { className: "cover-card-label" }, choice.label),
            el("span", { className: "cover-card-size" }, describe(choice)),
        );
        card.setAttribute("aria-pressed", "false");
        card.addEventListener("click", () => toggle(choice));
        cards.set(choice, card);
        const item = el("li", {}, card);
        if (first) list.prepend(item);
        else list.append(item);
    }

    function remove(choice: CoverChoice): void {
        cards.get(choice)?.parentElement?.remove();
        cards.delete(choice);
        selected = selected.filter((c) => c !== choice);
    }

    /** Shows or hides a choice large; a third drops the first shown. */
    function toggle(choice: CoverChoice): void {
        selected = selected.includes(choice) ? selected.filter((c) => c !== choice) : [...selected, choice].slice(-COMPARE);
        renderView();
    }

    function renderView(): void {
        for (const [choice, card] of cards) card.setAttribute("aria-pressed", String(selected.includes(choice)));
        view.hidden = !selected.length;
        view.classList.toggle("actual", actual);
        actualBtn.setAttribute("aria-pressed", String(actual));
        viewItems.replaceChildren(...selected.map((c) => {
            const img = el("img", { src: c.src, alt: c.label });
            // Pixel for pixel: one image pixel per screen pixel.
            if (actual) img.style.width = `${c.width / window.devicePixelRatio}px`;
            const pick = c.save
                ? el("button", { type: "button", className: "btn-primary" }, "Use this cover")
                : el("span", { className: "cover-view-note" }, "The book's cover now");
            if (pick instanceof HTMLButtonElement) pick.addEventListener("click", () => void use(c));
            return el(
                "figure",
                { className: "cover-view-item" },
                el("div", { className: "cover-view-frame" }, img),
                el("figcaption", {}, el("strong", {}, c.label), ` · ${describe(c)}`),
                pick,
            );
        }));
    }

    actualBtn.addEventListener("click", () => {
        actual = !actual;
        renderView();
    });
    maybeId("cover-view-close", HTMLButtonElement)?.addEventListener("click", () => {
        selected = [];
        renderView();
    });

    // ---- Saving -------------------------------------------------------------------

    let current: CoverChoice | null = null;
    let currentStale = true;

    /** One save or revert at a time. */
    async function run(work: () => Promise<string>): Promise<void> {
        if (busy) return;
        busy = true;
        dialog.classList.add("busy");
        try {
            const message = await work();
            currentStale = true; // the "current" choice is reloaded next time
            dialog.close();
            onChanged(message);
        } catch (err) {
            say(`Couldn't change the cover: ${errorMessage(err)}`, true);
        } finally {
            busy = false;
            dialog.classList.remove("busy");
        }
    }

    const use = (choice: CoverChoice): Promise<void> => run(async () => {
        say(`Saving ${choice.label} as the cover…`);
        const res = await choice.save!();
        revertBtn.hidden = !res.hasBackup;
        return `✓ Cover saved (${choice.label}). Covers are saved right away; Apply Changes is for the fields.`;
    });

    revertBtn.addEventListener("click", () => void run(async () => {
        say("Bringing back the previous cover…");
        const res = await revertCover(uuid);
        revertBtn.hidden = !res.hasBackup;
        return "✓ The previous cover is back.";
    }));

    // ---- Sources --------------------------------------------------------------------

    /** The current cover, first in the list (reloaded after a change). */
    async function loadCurrent(): Promise<void> {
        currentStale = false;
        const fresh = await currentCover(uuid);
        if (current) remove(current);
        current = fresh;
        add(current, { first: true });
        renderView();
    }

    let filesLoaded = false;
    async function loadBookFiles(): Promise<void> {
        filesLoaded = true;
        let formats: string[] = [];
        try {
            formats = await api<string[]>("GET", `/formats/${encodeURIComponent(uuid)}`);
        } catch {
            /* the current cover and an upload still work */
        }
        const notes: string[] = [];
        const sources: [string, () => AsyncGenerator<CoverChoice>][] = [];
        if (formats.includes("pdf")) sources.push(["PDF", () => pdfPages(uuid)]);
        if (formats.includes("epub")) sources.push(["EPUB", () => epubPages(uuid)]);
        for (const [name, source] of sources) {
            say(`Loading the ${name}'s first pages…`);
            try {
                for await (const choice of source()) add(choice);
            } catch (err) {
                notes.push(`Couldn't read the ${name}: ${errorMessage(err)}`);
            }
        }
        say([hint(), ...notes].join(" "), notes.length > 0);
    }

    const hint = (): string =>
        cards.size ? "Click a cover to see it large; click another to compare the two." : "No covers to show: upload one.";

    fileEl.addEventListener("change", async () => {
        const file = fileEl.files?.[0];
        fileEl.value = "";
        if (!file) return;
        try {
            const choice = await fileChoice(uuid, file);
            add(choice, { first: true });
            selected = current ? [current, choice] : [choice]; // against the cover it would replace
            renderView();
            say(`${choice.label}: compare it, then click "Use this cover" to save it.`);
        } catch (err) {
            say(`Couldn't read ${file.name}: ${errorMessage(err)}`, true);
        }
    });

    // ---- Online: Open Library's cover for the ISBN -----------------------------------

    const isbn = (): string => isbnInput?.value.replace(/[^0-9Xx]/g, "") ?? "";
    let kept: CoverCandidate[] = []; // from a lookup the admin kept (web/isbn-tools.ts)
    const online = new Set<string>(); // cover URLs already shown

    const updateOnline = (): void => {
        const n = isbn();
        onlineBtn.hidden = !isbnInput || (!n && !kept.some((k) => !online.has(k.url)));
        onlineBtn.title = n ? `Asks Open Library for the cover of ISBN ${n}` : "The cover from the Open Library lookup";
    };
    isbnInput?.addEventListener("input", updateOnline);
    document.addEventListener("cf:cover-candidates", (e) => {
        kept = (e as CustomEvent<CoverCandidate[]>).detail;
        updateOnline();
    });

    onlineBtn.addEventListener("click", async () => {
        onlineBtn.disabled = true;
        const n = isbn();
        const urls = kept.map((k) => k.url);
        let note = "";
        try {
            if (n) {
                say(`Looking up ISBN ${n} on Open Library…`);
                const r = await api<LookupResult>("GET", `/metadata/lookup?${new URLSearchParams({ source: "openlibrary", isbn: n })}`);
                if (r.cover) urls.unshift(r.cover);
                else note = r.option ? `Open Library has no cover for ISBN ${n}.` : `Open Library: ${r.error || "no match"}.`;
            }
            const added: CoverChoice[] = [];
            for (const url of urls) {
                if (online.has(url)) continue;
                online.add(url);
                const choice = await imageChoice(uuid, url, "Open Library");
                if (choice) {
                    add(choice, { first: true });
                    added.push(choice);
                }
            }
            if (added[0]) {
                selected = current ? [current, added[0]] : [added[0]];
                renderView();
            }
            say(added.length ? "Open Library's cover is next to the current one." : note || "No new cover found online.");
        } catch (err) {
            say(`Lookup failed: ${errorMessage(err)}`, true);
        } finally {
            onlineBtn.disabled = false;
            updateOnline();
        }
    });
    updateOnline();

    // ---- Opening ---------------------------------------------------------------------

    opener.addEventListener("click", () => {
        if (!dialog.open) dialog.showModal();
        say(hint());
        void fetchCoverStatus(uuid).then((s) => (revertBtn.hidden = !s.hasBackup));
        void (async () => {
            if (currentStale) await loadCurrent();
            if (!filesLoaded) await loadBookFiles();
            else say(hint());
        })();
    });
    closeOnBackdropClick(dialog);
}
