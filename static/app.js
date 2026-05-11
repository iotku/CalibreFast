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
                <h2>${book.title}</h2>
                <p>${book.author_sort}</p>
                <p>${book.pubdate}</p>
            `;

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