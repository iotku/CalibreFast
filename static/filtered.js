import { renderBooks, abortImageQueue } from "./shared.js";

// derive category and name from URL e.g. /author/Terry Pratchett
const parts = window.location.pathname.split("/").filter(Boolean);
const category = parts[0];  // "author", "publisher", "tag"
const name = decodeURIComponent(parts.slice(1).join("/"));

document.getElementById("page-title").textContent = name;
document.title = name;

const booksDiv = document.getElementById("books");
const loadMoreEl = document.getElementById("load-more");
const status = document.getElementById("status");

let currentPage = 1;
let loading = false;
let done = false;
let loadAbortController = null;

async function fetchPage(page, signal) {
    const params = new URLSearchParams({ api: "1", page });
    const res = await fetch(`/${category}/${encodeURIComponent(name)}?${params}`, { signal });
    if (!res.ok) return null;
    return res.json();
}

async function loadNextPage(signal) {
    if (loading || done) return;
    loading = true;
    try {
        const books = await fetchPage(currentPage, signal);
        if (signal?.aborted) return;
        if (!books || books.length === 0) {
            done = true;
            status.textContent = currentPage === 1 ? "No books found." : "";
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

loadAbortController = new AbortController();
loadNextPage(loadAbortController.signal);

const observer = new IntersectionObserver(
    (entries) => {
        if (entries[0].isIntersecting) loadNextPage(loadAbortController?.signal);
    },
    { rootMargin: "1500px" }
);
observer.observe(loadMoreEl);