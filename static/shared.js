export let imageGeneration = 0;
export let imageAbortController = new AbortController();
export const queue = [];
export const inQueue = new Set();
export let active = 0;
export const MAX_CONCURRENT = 8;

export const booksDiv = document.getElementById("books");

const coverObserver = new IntersectionObserver(
  (entries) => {
    for (const entry of entries) {
      if (!entry.isIntersecting) continue;
      const img = entry.target;
      if (img.src) continue;
      enqueue(img);
    }
  },
  { rootMargin: "800px" },
);

export function abortImageQueue() {
  imageGeneration++;
  imageAbortController.abort();
  imageAbortController = new AbortController();
  queue.length = 0;
  inQueue.clear();
}

export function enqueue(img) {
  if (inQueue.has(img)) return;
  inQueue.add(img);
  // NEWEST FIRST
  queue.unshift(img);
  processQueue();
}

export function processQueue() {
  const signal = imageAbortController.signal;
  const generation = imageGeneration;

  while (active < MAX_CONCURRENT && queue.length > 0) {
    const img = queue.shift();
    inQueue.delete(img);
    active++;

    fetch(img.dataset.src, { signal })
      .then((r) => r.blob())
      .then((blob) => {
        if (imageGeneration !== generation) return; // stale, discard
        img.src = URL.createObjectURL(blob);
        img.onload = () => img.classList.add("loaded");
      })
      .catch(() => {})
      .finally(() => {
        active--;
        if (imageGeneration === generation) processQueue();
      });
  }
}

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

export function colorFromBook(book) {
  const h = hashString(book.uuid);
  return `hsl(${h % 360}, 55%, 35%)`;
}

function hashString(str) {
  let h = 0;
  for (let i = 0; i < str.length; i++) {
    h = (h << 5) - h + str.charCodeAt(i);
    h |= 0;
  }
  return Math.abs(h);
}

export function getVisiblePage() {
  const pages = document.querySelectorAll(".page");
  const headerHeight = document.querySelector(".top-bar").offsetHeight;
  let current = null;

  for (const page of pages) {
    const rect = page.getBoundingClientRect();
    if (rect.top <= headerHeight + 5) {
      current = parseInt(page.dataset.page, 10);
    } else {
      break;
    }
  }

  return (
    current ?? parseInt(document.querySelector(".page")?.dataset.page, 10) ?? 1
  );
}

export function renderBooks(books, pageNumber, container = booksDiv) {
  const pageEl = document.createElement("div");
  pageEl.className = "page";
  pageEl.dataset.page = pageNumber;
  for (const book of books) {
    const el = document.createElement("div");
    el.className = "book";
    el.innerHTML = `
    <a href="/book/get/${book.uuid}">
    <div class="cover-wrapper" style="background:${colorFromBook(book)}">
        <img class="book-cover" fetchpriority="low" />
    </div>
    </a>

    <div class="book-info">
        <h2>${book.title}</h2>
        <div class="book-author">
            ${book.author_sort}
        </div>
        <div class="formats"></div>
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
    const coverLink = el.querySelector("a");
    coverLink.removeAttribute("href");
    coverLink.addEventListener("click", (e) => {
      e.preventDefault();
      openBookModal(book.uuid);
    });
    pageEl.appendChild(el);
  }
  container.appendChild(pageEl);
}

// shared.js
const modal = document.getElementById("book-modal");
const modalBody = document.getElementById("modal-body");

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
  console.log("popstate", window.history.state);
  document.body.classList.remove("no-scroll");
  if (modal.classList.contains("open")) {
    modal.classList.remove("open");
  }
});
export async function openBookModal(uuid) {
  document.body.classList.add("no-scroll"); // popstate trigger removes
  modalBody.innerHTML = "Loading...";
  modal.classList.add("open");
  history.pushState({ modal: uuid }, "");

  const res = await fetch(`/book/get/${uuid}`);
  modalBody.innerHTML = await res.text();

  const formatsRes = await fetch(`/formats/${uuid}`);
  const formats = await formatsRes.json(); // TODO: We should just get the formats directly in the template and avoid the extra fetch call

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
}
