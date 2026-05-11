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

    function renderBooks(books) {
        for (const book of books) {
            const el = document.createElement("div");

            el.className = "book";


            el.innerHTML = `
    <a href="/book/${book.uuid}">
        <img src="/cover/${book.uuid}" class="book-cover" />
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