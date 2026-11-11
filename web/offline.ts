/** The offline page (templates/offline.html): the books kept on this device. */

import { listBooks } from "./book-cache.js";
import { byId, el } from "./dom.js";

const list = byId("offline-books");
const books = await listBooks().catch(() => []);
books.sort((a, b) => b.lastOpened - a.lastOpened);
for (const b of books) {
    const a = el("a", { href: `/read?uuid=${encodeURIComponent(b.uuid)}` }, el("span", { className: "offline-title", textContent: b.title || "Untitled" }));
    if (b.authors) a.append(el("span", { className: "offline-author", textContent: b.authors }));
    list.append(el("li", {}, a));
}
byId("offline-none").hidden = books.length > 0;
byId("offline-intro").hidden = books.length === 0;
