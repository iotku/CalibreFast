function syncHeaderOffset() {
    const header = document.querySelector(".top-bar");

    const height = header.offsetHeight + 5;

    document.body.style.paddingTop = `${height}px`;
}

window.addEventListener("DOMContentLoaded", () => {
    syncHeaderOffset();

    let totalPages = null;
    async function loadLibraryInfo() {
        const res = await fetch("/library-info");
        const info = await res.json();

        totalPages = info.total_pages;
        document.getElementById("next-page").disabled =
            currentPage > totalPages;
        document.getElementById("prev-page").disabled =
            currentPage <= 2;
    }
    loadLibraryInfo()

    const booksDiv = document.getElementById("books");
    const loadMoreEl = document.getElementById("load-more");

    const params = new URLSearchParams(window.location.search);

    let currentPage = parseInt(params.get("page") || "1", 10);

    let visiblePage = currentPage;

    if (isNaN(currentPage) || currentPage < 1) {
        currentPage = 1;
    }
    if (totalPages && currentPage > totalPages) {
        currentPage = totalPages;
    }
    let loading = false;
    let done = false;
    // Scroll to top of page on initial load
    window.history.scrollRestoration = "manual";
    window.scrollTo(0, 0);

    const pageLabel = document.getElementById("page-label");

    function updatePageLabel() {
        history.replaceState(null, "", `?page=${visiblePage}`);
        pageLabel.textContent = `Page ${visiblePage}`;
        document.getElementById("next-page").disabled =
            currentPage > totalPages;
        document.getElementById("prev-page").disabled =
            currentPage <= 2;
    }
    document.getElementById("next-page").addEventListener("click", async () => {
        booksDiv.innerHTML = "";
        done = false;

        visiblePage = visiblePage + 1;
        currentPage = visiblePage;

        await loadNextPage();

        updatePageLabel();
    });

    document.getElementById("prev-page").addEventListener("click", async () => {
        if (visiblePage <= 1) return;

        booksDiv.innerHTML = "";
        done = false;

        visiblePage = Math.max(1, visiblePage - 1);
        currentPage = visiblePage;

        await loadNextPage();

        updatePageLabel();
    });

    if (totalPages && currentPage > totalPages) {
        done = true;
        return;
    }
    // prefetched pages cache
    const pageCache = new Map();

    async function fetchPage(page) {
        if (pageCache.has(page)) {
            return pageCache.get(page);
        }

        const res = await fetch(`/pages/page${page}.json`);

        if (!res.ok) {
            return null;
        }

        const books = await res.json();

        pageCache.set(page, books);

        return books;
    }

    async function preloadPages(startPage, count) {
        for (let i = 0; i < count; i++) {
            const page = startPage + i;

            // avoid duplicate fetches
            if (!pageCache.has(page)) {
                fetchPage(page).catch(console.error);
            }
        }
    }

    function generateCoverText(book) {
        const canvas = document.createElement("canvas");
        canvas.className = "cover-text";

        canvas.width = 200;
        canvas.height = 300;

        const ctx = canvas.getContext("2d");

        const scale = Math.min(window.devicePixelRatio || 1, 3);
        canvas.width *= scale;
        canvas.height *= scale;
        ctx.scale(scale, scale);

        ctx.fillStyle = "white";
        ctx.textAlign = "center";

        // title
        ctx.font = "bold 16px sans-serif";
        wrapText(ctx, book.title, 100, 120, 180, 20);

        // author
        ctx.font = "12px sans-serif";
        ctx.fillText(book.author_sort || "", 100, 260);

        return canvas;
    }

    function wrapText(ctx, text, x, y, maxWidth, lineHeight) {
        const words = (text || "").split(" ");
        let line = "";

        for (let i = 0; i < words.length; i++) {
            const test = line + words[i] + " ";
            const width = ctx.measureText(test).width;

            if (width > maxWidth && i > 0) {
                ctx.fillText(line, x, y);
                line = words[i] + " ";
                y += lineHeight;
            } else {
                line = test;
            }
        }

        ctx.fillText(line, x, y);
    }

    function hashString(str) {
        let h = 0;
        for (let i = 0; i < str.length; i++) {
            h = (h << 5) - h + str.charCodeAt(i);
            h |= 0;
        }
        return Math.abs(h);
    }

    function colorFromBook(book) {
        const h = hashString(book.uuid);
        return `hsl(${h % 360}, 55%, 35%)`;
    }


    const queue = [];
    const inQueue = new Set();
    const MAX_CONCURRENT = 50;
    let active = 0;

    function enqueue(img) {
        if (inQueue.has(img)) return;
        inQueue.add(img);
        // NEWEST FIRST
        queue.unshift(img);
        processQueue();
    }

    function processQueue() {
        while (active < MAX_CONCURRENT && queue.length > 0) {
            const img = queue.shift();
            inQueue.delete(img);

            active++;

            const controller = new AbortController();

            fetch(img.dataset.src, { signal: controller.signal })
                .then(r => r.blob())
                .then(blob => {
                    img.src = URL.createObjectURL(blob);
                    img.onload = () => { // TODO: Do we really need to add an onload handler here?
                        img.classList.add("loaded"); // add loaded class for transition
                    };
                })
                .catch(() => {})
                .finally(() => {
                    active--;
                    processQueue();
                });
        }
    }

    window.addEventListener("scroll", () => {
        computeCurrentPage();
    });

    function computeCurrentPage() {
        const pages = document.querySelectorAll(".page");

        let current = visiblePage

        for (const page of pages) {
            const rect = page.getBoundingClientRect();

            // page has crossed into "active region"
            if (rect.top <= 100) {
                console.log("new page" + parseInt(page.dataset.page, 10));
                current = parseInt(page.dataset.page, 10);
            } else {
                // since pages are ordered, we can stop early
                break;
            }
        }

        if (current === visiblePage) return;

        visiblePage = current;

        updatePageLabel();

    }

    const coverObserver = new IntersectionObserver((entries) => {
        for (const entry of entries) {
            if (!entry.isIntersecting) continue;

            const img = entry.target;
            if (img.src) continue;

            enqueue(img);
        }
    }, {
        rootMargin: "800px"
    });


    function renderBooks(books, pageNumber) {
        const pageEl = document.createElement("div");
        pageEl.className = "page";
        pageEl.dataset.page = pageNumber;
        for (const book of books) {
            const el = document.createElement("div");

            el.className = "book";


            el.innerHTML = `
    <a href="/book/${book.uuid}">
    <div class="cover-wrapper" style="background:${colorFromBook(book)}">
        <img class="book-cover" fetchpriority="low" alt="${book.title}"/>
    </div>
    </a>
    
    <div class="book-info">
    <div class="formats"></div>
        <h2>${book.title}</h2>

        <div class="book-author">
            ${book.author_sort}
        </div>

        <div class="book-date">
            ${book.pubdate}
        </div>
    </div>
`;

            const cover = el.querySelector(".book-cover");

            cover.dataset.src = `/cover-thumb/${book.uuid}`;
            cover.dataset.id = book.uuid;
            coverObserver.observe(cover);
            cover.addEventListener("error", () => {
                const canvas = generateCoverText(book);
                const wrapper = cover.parentElement;

                cover.remove();
                wrapper.appendChild(canvas);
            });


            el.addEventListener("mouseenter", async () => {
                if (el.dataset.loaded) return;

                const res = await fetch(`/formats/${book.uuid}`);
                const formats = await res.json();
                if (!Array.isArray(formats)) {
                    return;
                }

                const container = el.querySelector(".formats");

                container.innerHTML = formats.map(f =>
                    `<button onclick="window.location='/download/${book.uuid}/${f}'">
            ${f.toUpperCase()}
        </button>`
                ).join("");

                el.dataset.loaded = "true";
            });
            pageEl.appendChild(el);

        }
        booksDiv.appendChild(pageEl);
    }

    async function loadNextPage() {
        if (loading || done) {
            return;
        }

        loading = true;

        try {
            const books = await fetchPage(currentPage);

            if (!books || books.length === 0) {
                done = true;
                return;
            }

            renderBooks(books, currentPage);
            currentPage++;

            // preload next 3 pages
            await preloadPages(currentPage, 3);
        } catch (err) {
            console.error(err);
        } finally {
            loading = false;
        }

        requestAnimationFrame(() => {
            computeCurrentPage();
        });
    }

    const observer = new IntersectionObserver(
        (entries) => {
            if (entries[0].isIntersecting) {
                loadNextPage();
            }
        },
        {
            rootMargin: "1500px",
        }
    );

    observer.observe(loadMoreEl);

    // initial load
    loadNextPage().then(() => {
        requestAnimationFrame(() => {
            updatePageLabel();      // 👈 key line
            computeCurrentPage();   // optional safety pass
        });
    });
});