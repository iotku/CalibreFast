window.addEventListener("DOMContentLoaded", () => {
    const booksDiv = document.getElementById("books");
    const loadMoreEl = document.getElementById("load-more");

    let currentPage = 0;
    let loading = false;
    let done = false;

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

    function attachCoverFallback(img, book) {

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


    function renderBooks(books) {
        for (const book of books) {
            const el = document.createElement("div");

            el.className = "book";


            el.innerHTML = `
    <a href="/book/${book.uuid}">
    <div class="cover-wrapper" style="background:${colorFromBook(book)}">
        <img src="/cover/${book.uuid}" class="book-cover" />
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

            booksDiv.appendChild(el);
        }
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

            renderBooks(books);
            // update URL without reload
            history.replaceState(
                null,
                "",
                `?page=${currentPage}`
            );

            currentPage++;

            // preload next 3 pages
            preloadPages(currentPage, 3);
        } catch (err) {
            console.error(err);
        } finally {
            loading = false;
        }
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
    loadNextPage();
});