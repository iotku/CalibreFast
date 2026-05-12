import { renderBooks, abortImageQueue } from "./shared.js";

let currentPage = 1;
let loading = false;
let done = false;
let loadAbortController = null;
let lastQuery = {};
window.addEventListener("DOMContentLoaded", () => {
    const booksDiv = document.getElementById("books");
    const loadMoreEl = document.getElementById("load-more");
    const status = document.getElementById("search-status");

    async function fetchSearchPage(q, page, signal) {
        const params = new URLSearchParams({ q, page });
        const res = await fetch(`/api/search?${params}`, { signal });
        if (!res.ok) return null;
        return res.json();
    }

    async function loadNextPage(signal) {
        if (loading || done) return;
        loading = true;

        try {
            const books = await fetchSearchPage(lastQuery.q, currentPage, signal);
            if (signal?.aborted) return;
            if (!books || books.length === 0) {
                done = true;
                status.textContent = currentPage === 1 ? "No results found." : "";
                return;
            }

            status.textContent = "";
            renderBooks(books, currentPage, booksDiv);
            currentPage++;
        } catch (err) {
            if (err.name === "AbortError") return;
            console.error(err);
        } finally {
            loading = false;
        }
    }

    document.getElementById("search-form").addEventListener("submit", (e) => {
        e.preventDefault();
    });

    function newSearch(q) {
        if (loadAbortController) loadAbortController.abort();
        loadAbortController = new AbortController();
        abortImageQueue();
        booksDiv.innerHTML = "";
        done = false;
        currentPage = 1;
        lastQuery = { q };
        history.replaceState(null, "", `?q=${encodeURIComponent(q)}`);
        loadNextPage(loadAbortController.signal);
    }

    document.getElementById("search-btn").addEventListener("click", () => {
        const q = document.getElementById("search-input").value.trim(); // 👈 missing this line
        if (!q) return;
        newSearch(q);
    });

    document.getElementById("search-input").addEventListener("keydown", (e) => {
        if (e.key === "Enter") document.getElementById("search-btn").click();
    });

// infinite scroll — same pattern as library
    const observer = new IntersectionObserver(
        (entries) => {
            if (entries[0].isIntersecting) loadNextPage(loadAbortController?.signal);
        },
        {rootMargin: "1500px"}
    );
    observer.observe(loadMoreEl);

    
// restore search from URL on load (so back button works)
    window.addEventListener("DOMContentLoaded", () => {
        const params = new URLSearchParams(window.location.search);
        const q = params.get("q");
        if (q) {
            document.getElementById("search-input").value = q;
            newSearch(q, params.get("from"), params.get("to"));
        }
    });

    const params = new URLSearchParams(window.location.search);
    const q = params.get("q");
    if (q) {
        document.getElementById("search-input").value = q;
        newSearch(q);
    }
});