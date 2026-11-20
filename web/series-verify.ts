// A series page (templates/filtered.html), for editors: say the series'
// highest number, its number of books, or both (series_complete.go). Saved in the app's
// database; no online service is involved.
import { api, byId, errorMessage, maybeId } from "./dom.js";

const dialog = byId("series-verify", HTMLDialogElement);
const form = dialog.querySelector("form");
const total = byId("series-verify-total", HTMLInputElement);
const books = byId("series-verify-books", HTMLInputElement);
const status = byId("series-verify-status");
const series = dialog.dataset["series"] ?? "";

function fail(err: unknown): void {
    status.textContent = errorMessage(err);
    status.hidden = false;
}

byId("series-verify-btn", HTMLButtonElement).addEventListener("click", () => {
    status.hidden = true;
    dialog.showModal();
    (total.value || !books.value ? total : books).select();
});

byId("series-verify-cancel", HTMLButtonElement).addEventListener("click", () => dialog.close());

form?.addEventListener("submit", (e) => {
    e.preventDefault();
    const n = Number(total.value || "0");
    const b = Number(books.value || "0");
    const ok = (x: number): boolean => Number.isInteger(x) && x >= 0 && x <= 10000;
    if (!ok(n) || !ok(b) || n + b === 0) {
        fail("Give the highest number, the number of books, or both: whole numbers from 1 to 10000.");
        return;
    }
    api("POST", "/api/series/verified", { series, total: n, books: b })
        .then(() => location.reload())
        .catch(fail);
});

maybeId("series-verify-remove", HTMLButtonElement)?.addEventListener("click", () => {
    api("DELETE", `/api/series/verified?series=${encodeURIComponent(series)}`)
        .then(() => location.reload())
        .catch(fail);
});
