// The admin's Server page (templates/admin-server.html): keep its uptime,
// API budgets, backups and storage current (GET /admin/server/live, Go: serverLive
// in server.go), and stop the server (POST /api/admin/server/stop, Go:
// serverStopHandler), then watch for it to come back when a service
// manager restarts it. Update (POST/GET /api/admin/server/update, Go:
// server_update.go) builds the latest version while the server runs, shows
// the build's output, and reloads once the new build answers.
import { api, byId, errorMessage, maybeId, statusLine } from "./dom.js";
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

// Update (server_update.go): the server builds while it keeps running, then
// restarts onto the build if it's new. The page follows the build's output,
// and reloads once a different process (the new build) answers.
type UpdateStatus = {
    running: boolean;
    done: boolean;
    ok: boolean;
    restarting: boolean;
    message: string;
    output: string[] | null;
    by: string;
    started: string;
};

const updateButton = maybeId("server-update", HTMLButtonElement);
const updateOutput = maybeId("update-output", HTMLPreElement);
const sayUpdate = updateButton ? statusLine("update-status") : () => {};
const UPDATE_POLL_MS = 1000;

/** Show the build's output, following its end unless the admin scrolled up. */
function showOutput(lines: string[] | null): void {
    if (!updateOutput || !lines?.length) return;
    const atEnd = updateOutput.scrollHeight - updateOutput.scrollTop - updateOutput.clientHeight < 24;
    updateOutput.hidden = false;
    updateOutput.textContent = lines.join("\n");
    if (atEnd) updateOutput.scrollTop = updateOutput.scrollHeight;
}

/**
 * Follow an update until it's done; when it restarts the server, until a
 * different process answers (the new build), then reload.
 */
async function followUpdate(first: UpdateStatus): Promise<void> {
    if (!updateButton) return;
    const process = first.started;
    const until = Date.now() + GIVE_UP_MS;
    updateButton.disabled = true;
    let st = first;
    let wentAway = false;
    for (;;) {
        showOutput(st.output);
        if (st.started !== process) {
            sayUpdate("Updated. Reloading…");
            location.reload();
            return;
        }
        if (st.restarting || wentAway) {
            stopping = true; // the live sections would only fail meanwhile
            clearTimeout(refreshTimer);
            sayUpdate(st.restarting ? st.message : "Restarting…");
        } else if (st.done) {
            sayUpdate(st.message, !st.ok);
            updateButton.disabled = false;
            return;
        } else {
            sayUpdate(st.by ? `Building (started by ${st.by})…` : "Building…");
        }
        if (Date.now() > until) {
            sayUpdate("It hasn't come back. Check the service on the server (systemctl status, journalctl).", true);
            return;
        }
        await new Promise((r) => setTimeout(r, UPDATE_POLL_MS));
        try {
            st = await api<UpdateStatus>("GET", "/api/admin/server/update");
        } catch (err) {
            // fetch's TypeError: not answering, as while it restarts (the
            // last status before that may not have said so yet). Any other
            // answer after that is a server that doesn't know this update:
            // the new build (an older one, rolled back to, may lack the
            // endpoint).
            if (err instanceof TypeError) wentAway = true;
            else if (wentAway || st.restarting) {
                location.reload();
                return;
            }
        }
    }
}

if (updateButton) {
    updateButton.addEventListener("click", async () => {
        if (!confirm("Build the latest version and restart onto it? The server keeps running while it builds.")) return;
        updateButton.disabled = true;
        try {
            await followUpdate(await api<UpdateStatus>("POST", "/api/admin/server/update"));
        } catch (err) {
            updateButton.disabled = false;
            sayUpdate(errorMessage(err), true);
        }
    });
    // An update already running (another admin, or this page reloaded): follow it.
    api<UpdateStatus>("GET", "/api/admin/server/update")
        .then((st) => (st.running || st.restarting ? followUpdate(st) : undefined))
        .catch(() => {});
}

// Backups (backup.go): "Back up now" starts one in the background; the live
// Backups list shows it running, then done.
const sayBackup = statusLine("backup-status");
for (const btn of document.querySelectorAll<HTMLButtonElement>("button[data-backup]")) {
    btn.addEventListener("click", async () => {
        const kind = btn.dataset["backup"] ?? "";
        btn.disabled = true;
        try {
            await api("POST", `/api/admin/backups?kind=${encodeURIComponent(kind)}`);
            sayBackup("Backing up… it appears in the list when it's done.");
            void refresh();
        } catch (err) {
            sayBackup(errorMessage(err), true);
        } finally {
            btn.disabled = false;
        }
    });
}
