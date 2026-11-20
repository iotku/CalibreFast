// A series page (templates/filtered.html), for editors: say how many
// numbered books the series has (series_complete.go). Saved in the app's
// database; no online service is involved.
import { api, byId, errorMessage, maybeId } from "./dom.js";

const dialog = byId("series-verify", HTMLDialogElement);
const form = dialog.querySelector("form");
const total = byId("series-verify-total", HTMLInputElement);
const status = byId("series-verify-status");
const series = dialog.dataset["series"] ?? "";

function fail(err: unknown): void {
    status.textContent = errorMessage(err);
    status.hidden = false;
}

byId("series-verify-btn", HTMLButtonElement).addEventListener("click", () => {
    status.hidden = true;
    dialog.showModal();
    total.select();
});

byId("series-verify-cancel", HTMLButtonElement).addEventListener("click", () => dialog.close());

form?.addEventListener("submit", (e) => {
    e.preventDefault();
    const n = Number(total.value);
    if (!Number.isInteger(n) || n < 1 || n > 10000) {
        fail("Give a whole number from 1 to 10000.");
        return;
    }
    api("POST", "/api/series/verified", { series, total: n })
        .then(() => location.reload())
        .catch(fail);
});

maybeId("series-verify-remove", HTMLButtonElement)?.addEventListener("click", () => {
    api("DELETE", `/api/series/verified?series=${encodeURIComponent(series)}`)
        .then(() => location.reload())
        .catch(fail);
});
