// The admin's Server page (templates/admin-server.html): keep its uptime,
// API budgets and storage current (GET /admin/server/live, Go: serverLive
// in server.go), and stop the server (POST /api/admin/server/stop, Go:
// serverStopHandler), then watch for it to come back when a service
// manager restarts it.
import { api, byId, errorMessage, statusLine } from "./dom.js";
import { parseServerHtml } from "./trusted.js";

const say = statusLine("server-status");
const button = byId("server-stop", HTMLButtonElement);

/** How often to look for the server once it's stopping, and for how long. */
const POLL_MS = 2000;
const GIVE_UP_MS = 10 * 60_000;

/** How often to refresh the live sections (the server says; 5 s by default). */
const REFRESH_MS = Number(byId("server-page", HTMLElement).dataset["refreshMs"]) || 5000;

let stopping = false;
let refreshTimer: ReturnType<typeof setTimeout> | undefined;
let refreshing = false;

/**
 * Fetch the live sections and swap in each one that changed. A section
 * that's the same is left alone, so a disk warning (role="alert") isn't
 * announced again every few seconds.
 */
async function refresh(): Promise<void> {
    if (stopping || refreshing) return;
    refreshing = true;
    try {
        const res = await fetch("/admin/server/live", { cache: "no-store", credentials: "same-origin" });
        if (!res.ok) return; // signed out, or the server's going away: try again next time
        const fresh = parseServerHtml(await res.text());
        for (const next of fresh.querySelectorAll<HTMLElement>("[data-live][id]")) {
            const cur = document.getElementById(next.id);
            if (cur && cur.outerHTML !== next.outerHTML) cur.replaceWith(document.importNode(next, true));
        }
    } catch {
        /* not answering just now */
    } finally {
        refreshing = false;
        scheduleRefresh();
    }
}

/** Ask again in REFRESH_MS, only while the page is shown. */
function scheduleRefresh(): void {
    clearTimeout(refreshTimer);
    if (stopping || document.visibilityState === "hidden") return;
    refreshTimer = setTimeout(() => void refresh(), REFRESH_MS);
}

document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void refresh();
    else clearTimeout(refreshTimer);
});
scheduleRefresh();

/** Whether the server answers this page (it's back, and still lets this admin in). */
async function answering(): Promise<boolean> {
    try {
        const res = await fetch(location.pathname, { method: "HEAD", cache: "no-store", credentials: "same-origin" });
        return res.ok;
    } catch {
        return false; // not listening
    }
}

/** Wait for the server to go away, then to come back, and reload. */
async function waitForRestart(): Promise<void> {
    const until = Date.now() + GIVE_UP_MS;
    let wentDown = false;
    while (Date.now() < until) {
        await new Promise((r) => setTimeout(r, POLL_MS));
        const up = await answering();
        if (!up) {
            if (!wentDown) say("Stopped. Waiting for it to start again…");
            wentDown = true;
        } else if (wentDown) {
            say("Back up.");
            location.reload();
            return;
        }
    }
    say("It hasn't come back. Check the service on the server (systemctl status, journalctl).", true);
}

button.addEventListener("click", async () => {
    const restarts = button.dataset["restarts"] === "true";
    const ask = restarts
        ? "Stop the server? Requests in progress finish first; systemd should start it again."
        : "Stop the server? Nothing will start it again: it stays down until someone starts it on the server.";
    if (!confirm(ask)) return;
    button.disabled = true;
    stopping = true;
    clearTimeout(refreshTimer);
    try {
        const res = await api<{ stopping: boolean; restarts: boolean }>("POST", "/api/admin/server/stop");
        if (res.restarts) {
            say("Stopping…");
            await waitForRestart();
        } else {
            say("Stopping. The server will stay down until it's started again.");
        }
    } catch (err) {
        button.disabled = false;
        stopping = false;
        scheduleRefresh();
        say(errorMessage(err), true);
    }
});
