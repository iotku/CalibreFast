import { byId, maybeId } from "./dom.js";
import { createReadingSync, renderBookmarks, type Bookmark, type BookmarkHandlers } from "./reading-sync.js";

const params = new URLSearchParams(window.location.search);
const uuidParam = params.get("uuid");

if (!uuidParam) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}
const uuid: string = uuidParam;

const $ = (id: string): HTMLElement => byId(id);
const progressLabel = $("progress-label");
const syncLabel = maybeId("sync-status");
const setSync = (text: string): void => {
    if (syncLabel) syncLabel.textContent = text;
};

const book = ePub(`/epub/${uuid}/`);
const rendition = book.renderTo("reader-container", {
    width: "100%",
    height: "100%",
    flow: "paginated",
});

// ---- font size -------------------------------------------------------------

let fontSize = parseInt(localStorage.getItem("reader-fontsize") || "100", 10) || 100;
function applyFontSize(): void {
    rendition.themes.fontSize(`${fontSize}%`);
    localStorage.setItem("reader-fontsize", String(fontSize));
}

// ---- position: resume (local or synced), save as the reader moves ----------

const sync = await createReadingSync(uuid, "epub", { legacyKey: `reader-cfi-${uuid}`, onStatus: setSync });
const saved = await sync.loadProgress();
let startCfi = saved?.position || null;
applyFontSize();

let locationsReady = false;
let currentCfi: string | null = null;
let currentPercent = 0;
// The first "relocated" is just the book opening (epub.js may normalise the
// saved CFI to the page start). Only moving away from it is saved — and that
// is what marks the book as being read.
let openingCfi: string | null = null;

function percentFor(loc: EpubJS.Location): number {
    if (loc.atEnd) return 1;
    if (!locationsReady) return 0;
    return book.locations.percentageFromCfi(loc.start.cfi) || 0;
}

rendition.on("relocated", (loc) => {
    if (!loc?.start?.cfi) return;
    currentCfi = loc.start.cfi;
    currentPercent = percentFor(loc);
    progressLabel.textContent = locationsReady ? `${Math.round(currentPercent * 100)}%` : "—";
    if (openingCfi === null) {
        openingCfi = currentCfi;
        return;
    }
    if (currentCfi === openingCfi) return;
    openingCfi = ""; // moved: from now on every change is saved
    sync.saveProgress(currentCfi, currentPercent);
});

// Handler is attached first so the opening position is seen as such.
try {
    await rendition.display(startCfi || undefined);
} catch {
    startCfi = null; // stale/invalid CFI (e.g. the book file changed)
    openingCfi = null;
    await rendition.display();
}

book.ready
    .then(() => book.locations.generate(1024))
    .then(() => {
        locationsReady = true;
        if (currentCfi) {
            currentPercent = book.locations.percentageFromCfi(currentCfi) || 0;
            progressLabel.textContent = `${Math.round(currentPercent * 100)}%`;
        }
    });

// ---- contents --------------------------------------------------------------

function renderToc(items: EpubJS.NavItem[], el: HTMLElement): void {
    for (const item of items) {
        const li = document.createElement("li");
        li.style.padding = "6px 0";
        const a = document.createElement("a");
        a.textContent = item.label.trim();
        a.href = "#";
        a.style.color = "var(--color-text)";
        a.style.textDecoration = "none";
        a.addEventListener("click", (e) => {
            e.preventDefault();
            rendition.display(item.href);
            $("toc-sidebar").style.display = "none";
        });
        li.appendChild(a);
        if (item.subitems?.length) {
            const sub = document.createElement("ul");
            sub.style.paddingLeft = "1rem";
            renderToc(item.subitems, sub);
            li.appendChild(sub);
        }
        el.appendChild(li);
    }
}
book.ready.then(() => renderToc(book.navigation.toc, $("toc-list")));

// ---- bookmarks -------------------------------------------------------------

function chapterLabel(): string {
    try {
        const section = currentCfi ? book.spine.get(currentCfi) : null;
        const href = section?.href || "";
        const find = (items: EpubJS.NavItem[]): string => {
            for (const it of items) {
                if (href && it.href && href.endsWith(it.href.split("#")[0] ?? "")) return it.label.trim();
                const sub = it.subitems?.length ? find(it.subitems) : "";
                if (sub) return sub;
            }
            return "";
        };
        return find(book.navigation?.toc || []);
    } catch {
        return "";
    }
}

const bookmarkHandlers: BookmarkHandlers = {
    onGo: (b) => {
        void rendition.display(b.position);
        $("toc-sidebar").style.display = "none";
    },
    onDelete: (b) => {
        sync.removeBookmark(b.id);
        showBookmarks(sync.bookmarks());
    },
};
const showBookmarks = (items: Bookmark[]): void => renderBookmarks($("bookmark-list"), items, bookmarkHandlers);
sync.loadBookmarks().then(showBookmarks);

$("bookmark-btn")?.addEventListener("click", () => {
    if (!currentCfi) return;
    const pct = locationsReady ? ` — ${Math.round(currentPercent * 100)}%` : "";
    sync.addBookmark(currentCfi, `${chapterLabel() || "Bookmark"}${pct}`);
    showBookmarks(sync.bookmarks());
    setSync("Bookmark added");
});

// ---- navigation ------------------------------------------------------------

function safePrev() {
    rendition.prev().catch((e) => {
        if (!e?.message?.includes("No Section Found")) console.error(e);
    });
}

function safeNext() {
    rendition.next().catch((e) => {
        if (!e?.message?.includes("No Section Found")) console.error(e);
    });
}

$("prev-btn").addEventListener("click", safePrev);
$("next-btn").addEventListener("click", safeNext);

window.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft") safePrev();
    if (e.key === "ArrowRight") safeNext();
    if (e.key === "Escape") $("toc-sidebar").style.display = "none";
});

// scroll to navigate
rendition.on("rendered", (_section, view) => {
    view.document.addEventListener(
        "wheel",
        (e) => {
            e.preventDefault();
            if (e.deltaY > 0) safeNext();
            else safePrev();
        },
        { passive: false },
    );
});

// click left/right half
$("reader-container").addEventListener("click", (e) => {
    if (e.clientX < window.innerWidth / 2) safePrev();
    else safeNext();
});

$("font-up").addEventListener("click", () => {
    fontSize = Math.min(fontSize + 10, 200);
    applyFontSize();
});
$("font-down").addEventListener("click", () => {
    fontSize = Math.max(fontSize - 10, 60);
    applyFontSize();
});

$("toc-btn").addEventListener("click", () => {
    const sidebar = $("toc-sidebar");
    sidebar.style.display = sidebar.style.display === "none" ? "block" : "none";
});

if (!sync.signedIn) setSync("Saved on this device");
