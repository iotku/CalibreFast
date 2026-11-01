import { initImageQueue, setupCoverObserver } from "/static/shared.js";

window.addEventListener("DOMContentLoaded", () => {
    let totalPages = null;
    let currentPage = 1;

    // Load library info on startup
    async function loadLibraryInfo() {
        const res = await fetch("/library-info");
        const info = await res.json();
        totalPages = info.total_pages;
        document.getElementById("total-pages").textContent = info.total_pages;
        updatePageButtons();
    }

    // Initialize HTMX event listeners
    function setupHTMXListeners() {
        // After HTMX loads new books, process images
        document.body.addEventListener("htmx:afterSwap", (e) => {
            if (e.detail.target.id === "books" || e.detail.target.id === "load-more") {
                // Process new cover images with intersection observer
                setupCoverObserver();
            }
        });
    }

    // Track current page from URL
    function updatePageFromURL() {
        const params = new URLSearchParams(window.location.search);
        const page = parseInt(params.get("page") || "1", 10);
        if (page >= 1 && page <= totalPages) {
            currentPage = page;
            document.getElementById("page-input").value = currentPage;
            updatePageButtons();
        }
    }

    function updatePageButtons() {
        const prevBtn = document.getElementById("prev-page");
        const nextBtn = document.getElementById("next-page");
        if (totalPages) {
            prevBtn.disabled = currentPage <= 1;
            nextBtn.disabled = currentPage >= totalPages;
        }
    }

    // Manual page navigation via input
    document.getElementById("page-input").addEventListener("change", (e) => {
        let target = parseInt(e.target.value, 10);
        if (isNaN(target)) return;
        target = Math.max(1, Math.min(target, totalPages));
        e.target.value = target;

        // Load the page via HTMX
        currentPage = target;
        const booksDiv = document.getElementById("books");
        htmx.ajax("GET", `/api/books?page=${target}`, {
            target: booksDiv,
            swap: "innerHTML",
        });

        // Update URL without reload
        history.replaceState({ page: target }, "", `?page=${target}`);
        updatePageButtons();
    });

    // Next page button
    document.getElementById("next-page").addEventListener("click", () => {
        if (currentPage < totalPages) {
            currentPage++;
            const booksDiv = document.getElementById("books");
            htmx.ajax("GET", `/api/books?page=${currentPage}`, {
                target: booksDiv,
                swap: "innerHTML",
            });
            history.replaceState({ page: currentPage }, "", `?page=${currentPage}`);
            document.getElementById("page-input").value = currentPage;
            updatePageButtons();
        }
    });

    // Previous page button
    document.getElementById("prev-page").addEventListener("click", () => {
        if (currentPage > 1) {
            currentPage--;
            const booksDiv = document.getElementById("books");
            htmx.ajax("GET", `/api/books?page=${currentPage}`, {
                target: booksDiv,
                swap: "innerHTML",
            });
            history.replaceState({ page: currentPage }, "", `?page=${currentPage}`);
            document.getElementById("page-input").value = currentPage;
            updatePageButtons();
        }
    });

    // Modal management
    const modal = document.getElementById("book-modal");
    const modalBody = document.getElementById("modal-body");

    // Open modal handler
    window.openBookModal = function() {
        document.body.classList.add("no-scroll");
        modal.classList.add("open");
        history.pushState({ modal: true }, "");

        // Load formats and add read buttons
        const bookUUID = modalBody.querySelector(".book-page")?.dataset?.uuid;
        if (bookUUID) {
            loadFormatsAndAddButtons(bookUUID);
        }
    };

    async function loadFormatsAndAddButtons(uuid) {
        try {
            const formatsRes = await fetch(`/formats/${uuid}`);
            const formats = await formatsRes.json();

            if (!Array.isArray(formats)) return;

            const btnContainer = document.createElement("div");
            btnContainer.style.cssText = `
                display:flex;
                flex-direction:column;
                gap:8px;
                margin-top:1rem;
                width:100%;
            `;

            for (const format of formats) {
                let href = null;
                let label = null;

                if (format === "epub") {
                    href = `/read?uuid=${uuid}`;
                    label = "Read EPUB";
                } else if (format === "pdf") {
                    href = `/read-pdf?uuid=${uuid}`;
                    label = "Read PDF";
                }

                if (!label) continue;

                const btn = document.createElement("button");
                btn.textContent = label;
                btn.style.cssText = `
                    height:44px;
                    width:100%;
                `;

                btn.addEventListener("click", () => {
                    window.location.href = href;
                });

                btnContainer.appendChild(btn);
            }

            const readButtons = modalBody.querySelector("#read-buttons");
            if (readButtons && btnContainer.children.length > 0) {
                readButtons.appendChild(btnContainer);
            }
        } catch (err) {
            console.error("Failed to load formats:", err);
        }
    }

    // Close modal handlers
    document.getElementById("modal-close").addEventListener("click", () => {
        history.back();
    });

    modal.addEventListener("click", (e) => {
        if (e.target === modal) history.back();
    });

    window.addEventListener("keydown", (e) => {
        if (e.key === "Escape" && modal.classList.contains("open")) history.back();
    });

    window.addEventListener("popstate", () => {
        document.body.classList.remove("no-scroll");
        if (modal.classList.contains("open")) {
            modal.classList.remove("open");
        }
    });

    // Initialize on load
    loadLibraryInfo().then(() => {
        updatePageFromURL();
        setupHTMXListeners();
        // Initial image loading setup
        initImageQueue();
    });
});
