export let imageGeneration = 0;
export let imageAbortController = new AbortController();
export const queue = [];
export const inQueue = new Set();
export let active = 0;
export const MAX_CONCURRENT = 8;

export const coverObserver = new IntersectionObserver(
  (entries) => {
    console.log('[IntersectionObserver] Callback fired with', entries.length, 'entries');
    for (const entry of entries) {
      console.log('[IntersectionObserver] Entry:', entry.target.dataset.id, 'isIntersecting:', entry.isIntersecting, 'has src:', !!entry.target.src, 'boundingClientRect.top:', entry.boundingClientRect.top);
      if (!entry.isIntersecting) continue;
      const img = entry.target;
      if (img.src) {
        console.log('[IntersectionObserver] Image already has src, skipping');
        continue;
      }
      console.log('[IntersectionObserver] Enqueueing image:', img.dataset.id);
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
  console.log('[Enqueue] Called for image:', img.dataset.id, 'Already in queue:', inQueue.has(img));
  if (inQueue.has(img)) return;
  inQueue.add(img);
  // NEWEST FIRST
  queue.unshift(img);
  console.log('[Enqueue] Added to queue. Queue length:', queue.length);
  processQueue();
}

export function processQueue() {
  const signal = imageAbortController.signal;
  const generation = imageGeneration;
  
  console.log('[ProcessQueue] Starting. Active:', active, 'Queue length:', queue.length, 'Max concurrent:', MAX_CONCURRENT);

  while (active < MAX_CONCURRENT && queue.length > 0) {
    const img = queue.shift();
    inQueue.delete(img);
    active++;
    
    console.log('[ProcessQueue] Fetching image:', img.dataset.id, 'from:', img.dataset.src);

    fetch(img.dataset.src, { signal })
      .then((r) => {
        console.log('[Fetch] Response received for:', img.dataset.id, 'status:', r.status);
        return r.blob();
      })
      .then((blob) => {
        if (imageGeneration !== generation) {
          console.log('[Fetch] Stale generation, discarding:', img.dataset.id);
          return;
        }
        img.src = URL.createObjectURL(blob);
        img.onload = () => {
          console.log('[Fetch] Image loaded successfully:', img.dataset.id);
          img.classList.add("loaded");
        };
      })
      .catch((err) => {
        console.error('[Fetch] Error loading image:', img.dataset.id, err);
      })
      .finally(() => {
        active--;
        console.log('[ProcessQueue] Fetch complete. Active:', active);
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

// Modal handling - initialize on DOMContentLoaded
let modal = null;
let modalBody = null;

document.addEventListener('DOMContentLoaded', () => {
    modal = document.getElementById("book-modal");
    modalBody = document.getElementById("modal-body");
    
    window.addEventListener("popstate", () => {
        document.body.classList.remove("no-scroll");
        if (modal?.classList.contains("open")) {
            modal.classList.remove("open");
        }
    });
});

export async function openBookModal(uuid) {
    console.log('[Modal] Opening modal for UUID:', uuid);
    if (!modal || !modalBody) {
        console.error('[Modal] Modal elements not found!');
        return;
    }
    
    document.body.classList.add("no-scroll"); // popstate trigger removes
    modalBody.innerHTML = "Loading...";
    modal.classList.add("open");
    history.pushState({ modal: uuid }, "");

    try {
        const res = await fetch(`/book/get/${uuid}`);
        modalBody.innerHTML = await res.text();

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
        console.error('[Modal] Error loading modal content:', err);
        modalBody.innerHTML = "Error loading book details";
    }
}
