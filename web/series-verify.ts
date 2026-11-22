// A series page's Manage sidebar (templates/filtered.html, manage-panel.ts),
// for editors: say the series' highest number, its number of books, or both
// (series_complete.go). Saved in the app's database; no online service is
// involved.
import { api, byId, errorMessage, maybeId } from "./dom.js";

const section = maybeId("series-verify");
if (section) init(section);

function init(section: HTMLElement): void {
    const form = section.querySelector("form");
    const total = byId("series-verify-total", HTMLInputElement);
    const books = byId("series-verify-books", HTMLInputElement);
    const status = byId("series-verify-status");
    const series = section.dataset["series"] ?? "";

    function fail(err: unknown): void {
        status.textContent = errorMessage(err);
        status.hidden = false;
    }

    form?.addEventListener("input", () => (status.hidden = true));
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
}
