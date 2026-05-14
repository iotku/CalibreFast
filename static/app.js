import { renderBooks, abortImageQueue, getVisiblePage, booksDiv } from "/static/shared.js";

window.addEventListener("DOMContentLoaded", () => {
    let totalPages = null;
    async function loadLibraryInfo() {
        const res = await fetch("/library-info");
        const info = await res.json();

        totalPages = info.total_pages;
        document.getElementById("total-pages").textContent = info.total_pages;
        document.getElementById("next-page").disabled =
            currentPage > totalPages;
        document.getElementById("prev-page").disabled =
            currentPage <= 2;
    }
    loadLibraryInfo()

    const loadMoreEl = document.getElementById("load-more");

    const params = new URLSearchParams(window.location.search);

    let currentPage = parseInt(params.get("page") || "1", 10);

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

    function updatePageLabel(overridePage) {
        const page = overridePage ?? getVisiblePage();
        const modalOpen = modal.classList.contains("open");
        const currentState = history.state ?? {};

        history.replaceState(
            { ...currentState, page },
            "",
            `?page=${page}`
        );

        // if modal was open, ensure its state is still on top
        if (modalOpen && !currentState.modal) {
            history.pushState({ ...currentState, modal: currentState.modal, page }, "");
        }
        document.getElementById("page-input").value = page;
        document.getElementById("next-page").disabled = currentPage > totalPages;
        document.getElementById("prev-page").disabled = page <= 1;
    }

    let loadAbortController = null;
    let imageAbortController = new AbortController();

    document.getElementById("page-input").addEventListener("change", async (e) => {
        let target = parseInt(e.target.value, 10);
        if (isNaN(target)) return;
        target = Math.max(1, Math.min(target, totalPages));
        e.target.value = target;

        // cancel any in-flight load
        if (loadAbortController) {
            loadAbortController.abort();
            loadAbortController = new AbortController();
            abortImageQueue();
        }
        loadAbortController = new AbortController();
        const signal = loadAbortController.signal;

        booksDiv.innerHTML = "";
        done = false;
        currentPage = target;

        await loadNextPage(signal);

        if (signal.aborted) return; // a newer load took over, bail out
        updatePageLabel(target);
    });

    let imageGeneration = 0;

    document.getElementById("next-page").addEventListener("click", async () => {
        const page = getVisiblePage();
        if (loadAbortController) loadAbortController.abort();
        loadAbortController = new AbortController();
        abortImageQueue();

        booksDiv.innerHTML = "";
        done = false;
        currentPage = page + 1;
        await loadNextPage(loadAbortController.signal);
        if (loadAbortController.signal.aborted) return;
        updatePageLabel(page + 1);
    });

    document.getElementById("prev-page").addEventListener("click", async () => {
        const page = getVisiblePage();
        if (page <= 1) return;
        if (loadAbortController) loadAbortController.abort();
        loadAbortController = new AbortController();
        abortImageQueue();

        booksDiv.innerHTML = "";
        done = false;
        currentPage = page - 1;
        await loadNextPage(loadAbortController.signal);
        if (loadAbortController.signal.aborted) return;
        updatePageLabel(page - 1);
    });

    if (totalPages && currentPage > totalPages) {
        done = true;
        return;
    }
    // prefetched pages cache
    const pageCache = new Map();

    async function fetchPage(page, signal) {
        if (pageCache.has(page)) return pageCache.get(page);

        const res = await fetch(`/pages/page${page}.json`, { signal });
        if (!res.ok) return null;

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

    window.addEventListener("scroll", () => updatePageLabel());
    window.addEventListener("scrollend", () => updatePageLabel());
    window.addEventListener("keydown", (e) => {
        if (e.key === "Home" || e.key === "End") {
            updatePageLabel();
        }
    });




    async function loadNextPage(signal) {
        if (loading || done) return;
        loading = true;

        try {
            const books = await fetchPage(currentPage, signal);
            if (signal?.aborted) return;

            if (!books || books.length === 0) {
                done = true;
                return;
            }

            renderBooks(books, currentPage);
            currentPage++;

            await preloadPages(currentPage, 3);
        } catch (err) {
            if (err.name === "AbortError") return; // expected, ignore
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
    loadNextPage().then(() => {
            updatePageLabel();
    });

    const modal = document.getElementById("book-modal");
    const modalBody = document.getElementById("modal-body");

    document.getElementById("modal-close").addEventListener("click", () => {
        modal.classList.remove("open");
    });

    modal.addEventListener("click", (e) => {
        if (e.target === modal) modal.classList.remove("open"); // click outside to close
    });

    window.addEventListener("keydown", (e) => {
        if (e.key === "Escape") modal.classList.remove("open");
    });
});