/**
 * CalibreFast Utilities
 * Functions for transforming book data and generating UI elements
 */

/**
 * Generate a hash from a string
 * @param {string} str - String to hash
 * @returns {number} Hash value
 */
export function hashString(str) {
  let h = 0;
  for (let i = 0; i < str.length; i++) {
    h = (h << 5) - h + str.charCodeAt(i);
    h |= 0;
  }
  return Math.abs(h);
}

/**
 * Generate a color from a book's UUID
 * @param {Object} book - Book object with uuid property
 * @returns {string} HSL color string
 */
export function colorFromBook(book) {
  const h = hashString(book.uuid);
  return `hsl(${h % 360}, 55%, 35%)`;
}

/**
 * Generate a text-based cover canvas for books without cover images
 * @param {Object} book - Book object with title and author_sort
 * @returns {HTMLCanvasElement} Canvas element with book info
 */
export function generateCoverText(book) {
  const canvas = document.createElement("canvas");
  canvas.className = "cover-text";
  canvas.width = 200;
  canvas.height = 300;

  const ctx = canvas.getContext("2d");
  const scale = Math.min(window.devicePixelRatio || 1, 3);
  canvas.width *= scale;
  canvas.height *= scale;
  ctx.scale(scale, scale);

  ctx.fillStyle = colorFromBook(book);
  ctx.fillRect(0, 0, 200, 300);

  ctx.fillStyle = "white";
  ctx.textAlign = "center";

  ctx.font = "bold 16px sans-serif";
  wrapText(ctx, book.title, 100, 120, 180, 20);

  ctx.font = "12px sans-serif";
  ctx.fillText(book.author_sort || "", 100, 260);

  return canvas;
}

/**
 * Wrap text to fit within a maximum width
 */
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

/**
 * Escape HTML special characters
 * @param {string} str - String to escape
 * @returns {string} Escaped string
 */
export function escapeHtml(str) {
  const div = document.createElement("div");
  div.textContent = str;
  return div.innerHTML;
}

/**
 * Render a single book card HTML
 * Plain link to #book/<uuid>; modal logic lives in setupModal().
 * @param {Object} book - Book object
 * @returns {string} HTML string for book card
 */
export function renderBookCard(book) {
  const title = escapeHtml(book.title || "Untitled");
  const author = escapeHtml(book.author_sort || "Unknown Author");
  const coverColor = colorFromBook(book);
  const uuid = escapeHtml(book.uuid);

  return `
<div class="book">
    <a href="#book/${uuid}" class="book-link">
        <div class="cover-wrapper" style="background:${coverColor}">
            <img class="book-cover"
                 data-src="/cover-thumb/${uuid}"
                 data-id="${uuid}"
                 fetchpriority="low" />
        </div>
    </a>
    <div class="book-info">
        <h2>${title}</h2>
        <div class="book-author">${author}</div>
        <div class="formats"></div>
    </div>
</div>`;
}

/**
 * Render a page of books
 * @param {Array} books - Array of book objects
 * @param {number} pageNumber - Page number
 * @returns {string} HTML string for page container with books
 */
export function renderBooksPage(books, pageNumber) {
  if (!books || !Array.isArray(books) || books.length === 0) {
    return "";
  }

  const booksHtml = books.map((book) => renderBookCard(book)).join("\n");

  return `
<div class="page" data-page="${pageNumber}">
    ${booksHtml}
</div>`;
}

/* ------------------------------------------------------------------ */
/* Modal (hash-driven)                                                 */
/* ------------------------------------------------------------------ */

const MODAL_ID = "book-modal";
const BODY_ID = "modal-body";
const HASH_PREFIX = "#book/";
const CLOSE_SELECTOR = ".modal-close, [data-modal-close]";

let modalInitialized = false;
let openedViaNav = false; // true if the current modal entry was pushed by a click

// htmx restores history on popstate (state.htmx) by re-fetching the current
// URL. The modal is a pure overlay driven by the hash, so htmx history is
// unused: swallow popstate before htmx sees it. Registered at module load,
// in the capture phase, so it runs ahead of htmx's own handler.
window.addEventListener('popstate', (e) => {
    e.stopImmediatePropagation();
}, true);

function modalEl() {
  return document.getElementById(MODAL_ID);
}
function bodyEl() {
  return document.getElementById(BODY_ID);
}
function isModalOpen() {
  return modalEl()?.classList.contains("open");
}

function hashUuid() {
  if (!location.hash.startsWith(HASH_PREFIX)) return null;
  const raw = location.hash.slice(HASH_PREFIX.length);
  if (!raw) return null;
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

function showModal() {
  bodyEl().innerHTML = "Loading...";
  document.body.classList.add("no-scroll");
  modalEl().classList.add("open");
}

function hideModal() {
  document.body.classList.remove("no-scroll");
  modalEl()?.classList.remove("open");
  const body = bodyEl();
  if (body) body.innerHTML = "";
}

function loadBook(uuid) {
  showModal();
  if (typeof htmx === "undefined") {
    bodyEl().innerHTML = "htmx not loaded.";
    return;
  }
  htmx.ajax("GET", `/book/get/${encodeURIComponent(uuid)}`, {
    target: "#" + BODY_ID,
    swap: "innerHTML",
  });
}

/**
 * Sync modal state to the URL hash.
 */
function syncFromHash() {
  const uuid = hashUuid();
  if (uuid) {
    loadBook(uuid);
  } else if (isModalOpen()) {
    hideModal();
    openedViaNav = false;
  }
}

/**
 * Close the modal. Goes back if the click pushed an entry,
 * otherwise strips the hash in place (deep-link case).
 */
export function closeModal() {
  if (!isModalOpen()) return;
  if (openedViaNav) {
    history.back(); // hashchange -> syncFromHash -> hideModal
  } else {
    history.replaceState(null, "", location.pathname + location.search);
    hideModal();
  }
}

/**
 * One-time modal setup. Safe to call more than once.
 * Expects #book-modal (overlay containing content) and #modal-body.
 * Close buttons: .modal-close or [data-modal-close].
 */
export function setupModal() {
  if (modalInitialized || !modalEl()) return;
  modalInitialized = true;

  // Mark that the next hash change came from a card click
  document.addEventListener(
    "click",
    (e) => {
      if (e.target.closest?.(".book-link")) openedViaNav = true;
    },
    true,
  );

  window.addEventListener("hashchange", syncFromHash);

  // Close: X button or backdrop click
  modalEl().addEventListener("click", (e) => {
    if (e.target === modalEl() || e.target.closest(CLOSE_SELECTOR)) {
      e.preventDefault();
      closeModal();
    }
  });

  // Close: Escape
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") closeModal();
  });

  // Failed fetch
  document.body.addEventListener("htmx:responseError", (e) => {
    if (e.detail.target?.id === BODY_ID) bodyEl().innerHTML = "Failed to load book.";
  });
  document.body.addEventListener("htmx:sendError", (e) => {
    if (e.detail.target?.id === BODY_ID) bodyEl().innerHTML = "Failed to load book.";
  });

  // Deep link on initial load
  if (hashUuid()) {
    openedViaNav = false;
    syncFromHash();
  }
}

/**
 * Setup book card click handlers for modal opening
 * @param {HTMLElement} container - Container element with book cards
 */
export function setupBookClickHandlers(container) {
  setupModal();
}

/* ------------------------------------------------------------------ */
/* Image loading queue                                                 */
/* ------------------------------------------------------------------ */

class ImageQueue {
  constructor(maxConcurrent = 8) {
    this.maxConcurrent = maxConcurrent;
    this.queue = [];
    this.inQueue = new Set();
    this.active = 0;
    this.generation = 0;
    this.abortController = new AbortController();
  }

  abort() {
    this.generation++;
    this.abortController.abort();
    this.abortController = new AbortController();
    this.queue = [];
    this.inQueue.clear();
  }

  enqueue(img, priority = 0) {
    if (this.inQueue.has(img)) {
      const item = this.queue.find((q) => q.img === img);
      if (item && priority > item.priority) {
        item.priority = priority;
        this.queue.sort((a, b) => b.priority - a.priority);
      }
      return;
    }

    this.inQueue.add(img);
    this.queue.push({ img, priority, generation: this.generation });
    this.queue.sort((a, b) => b.priority - a.priority);

    this.processQueue();
  }

  processQueue() {
    const signal = this.abortController.signal;
    const generation = this.generation;

    while (this.active < this.maxConcurrent && this.queue.length > 0) {
      const item = this.queue.shift();
      const { img } = item;

      this.inQueue.delete(img);
      this.active++;

      const src = img.dataset.src;
      if (!src) {
        this.active--;
        continue;
      }

      fetch(src, { signal })
        .then((r) => r.blob())
        .then((blob) => {
          if (generation !== this.generation) return;

          img.src = URL.createObjectURL(blob);
          img.onload = () => img.classList.add("loaded");
        })
        .catch((err) => {
          if (err.name === "AbortError") return;
          if (generation !== this.generation) return;

          const bookCard = img.closest(".book");
          if (bookCard) {
            const wrapper = img.parentElement;
            const bookData = {
              title: bookCard.querySelector("h2")?.textContent || "Unknown",
              author_sort: bookCard.querySelector(".book-author")?.textContent || "",
              uuid: img.dataset.id,
            };
            const canvas = generateCoverText(bookData);
            img.remove();
            wrapper.appendChild(canvas);
          }
        })
        .finally(() => {
          this.active--;
          if (generation === this.generation) {
            this.processQueue();
          }
        });
    }
  }

  cancelFarImages(visiblePage, keepRange = 2) {
    const oldLength = this.queue.length;
    this.queue = this.queue.filter((item) => {
      const img = item.img;
      const page = img.closest(".page");
      if (!page) return true;

      const pageNum = parseInt(page.dataset.page, 10);
      if (isNaN(pageNum)) return true;

      const distance = Math.abs(pageNum - visiblePage);
      if (distance > keepRange) {
        this.inQueue.delete(img);
        return false;
      }
      return true;
    });

    if (oldLength !== this.queue.length) {
      this.queue.sort((a, b) => b.priority - a.priority);
    }
  }
}

const imageQueue = new ImageQueue(8);

/**
 * Setup cover image lazy loading with IntersectionObserver
 * @param {HTMLElement} container - Container element with book cards
 */
export function setupCoverLazyLoading(container) {
  const images = container.querySelectorAll(".book-cover[data-src]");

  const observer = new IntersectionObserver(
    (entries) => {
      entries.forEach((entry) => {
        const img = entry.target;

        if (entry.isIntersecting) {
          if (!img.src && img.dataset.src) {
            imageQueue.enqueue(img, 100);
          }
        } else if (entry.intersectionRatio === 0) {
          if (!img.src && img.dataset.src) {
            imageQueue.enqueue(img, 1);
          }
        }
      });
    },
    {
      rootMargin: "800px",
      threshold: [0, 0.1],
    },
  );

  images.forEach((img) => {
    observer.observe(img);

    const rect = img.getBoundingClientRect();
    const inViewport = rect.top < window.innerHeight && rect.bottom > 0;

    if (inViewport && !img.src && img.dataset.src) {
      imageQueue.enqueue(img, 100);
    }
  });
}

/**
 * Cancel loading of images far from current viewport
 */
export function cancelFarImages(visiblePage, keepRange = 2) {
  imageQueue.cancelFarImages(visiblePage, keepRange);
}

/**
 * Abort all pending image loads
 */
export function abortImageQueue() {
  imageQueue.abort();
}
