const params = new URLSearchParams(window.location.search);
const uuid = params.get("uuid");

if (!uuid) {
    document.body.innerHTML = "<p>No book specified.</p>";
    throw new Error("missing uuid");
}

const book = ePub(`/epub/${uuid}/`);
const rendition = book.renderTo("reader-container", {
    width: "100%",
    height: "100%",
    flow: "paginated",
});

let fontSize = 100;

function applyFontSize() {
    rendition.themes.fontSize(`${fontSize}%`);
    localStorage.setItem(`reader-fontsize`, fontSize);
}

// restore saved position and font size
const savedCfi = localStorage.getItem(`reader-cfi-${uuid}`);
if (savedCfi && savedCfi !== "undefined") {
    rendition.display(savedCfi);
} else {
    localStorage.removeItem(`reader-cfi-${uuid}`); // clean up bad value
    rendition.display();
}
const savedFont = localStorage.getItem(`reader-fontsize`);
if (savedFont) fontSize = parseInt(savedFont);

if (savedCfi) {
    rendition.display(savedCfi);
} else {
    rendition.display();
}

applyFontSize();

// progress
const progressLabel = document.getElementById("progress-label");

book.ready.then(() => {
    book.locations.generate(1024).then(() => {
        rendition.on("relocated", (loc) => {
            if (loc?.start?.cfi) {
                localStorage.setItem(`reader-cfi-${uuid}`, loc.start.cfi);
                const pct = Math.round(book.locations.percentageFromCfi(loc.start.cfi) * 100);
                progressLabel.textContent = `${pct}%`;
            }
        });
    });
    function renderToc(items, el) {
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
                document.getElementById("toc-sidebar").style.display = "none";
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

    book.ready.then(() => {
        const toc = book.navigation.toc;
        const list = document.getElementById("toc-list");
        renderToc(toc, list);

        book.locations.generate(1024).then(() => {
            rendition.on("relocated", (loc) => {
                if (loc?.start?.cfi) {
                    localStorage.setItem(`reader-cfi-${uuid}`, loc.start.cfi);
                    const pct = Math.round(book.locations.percentageFromCfi(loc.start.cfi) * 100);
                    progressLabel.textContent = `${pct}%`;
                }
            });
        });
    });
});

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

// prev/next
// replace existing prev/next button listeners
document.getElementById("prev-btn").addEventListener("click", safePrev);
document.getElementById("next-btn").addEventListener("click", safeNext);

// keyboard nav
window.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft") safePrev();
    if (e.key === "ArrowRight") safeNext();
    if (e.key === "Escape") document.getElementById("toc-sidebar").style.display = "none";
});

// scroll to navigate
rendition.on("rendered", (section, view) => {
    view.document.addEventListener("wheel", (e) => {
        e.preventDefault();
        if (e.deltaY > 0) safeNext(); else safePrev();
    }, { passive: false });
});

// click left/right half
document.getElementById("reader-container").addEventListener("click", (e) => {
    if (e.clientX < window.innerWidth / 2) safePrev(); else safeNext();
});

// TODO: click handler

// font size
document.getElementById("font-up").addEventListener("click", () => {
    fontSize = Math.min(fontSize + 10, 200);
    applyFontSize();
});
document.getElementById("font-down").addEventListener("click", () => {
    fontSize = Math.max(fontSize - 10, 60);
    applyFontSize();
});

// toc toggle
document.getElementById("toc-btn").addEventListener("click", () => {
    const sidebar = document.getElementById("toc-sidebar");
    sidebar.style.display = sidebar.style.display === "none" ? "block" : "none";
});