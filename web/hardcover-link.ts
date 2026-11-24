/**
 * The account page's Hardcover section (hardcover_account.go) while a sign-in
 * is in progress: it asks the server every few seconds whether the code was
 * entered on Hardcover, and shows the page again once it was (or failed).
 * Everything shown is rendered by the server; nothing here builds HTML.
 */

const POLL_MS = 4000;
const MAX_POLLS = 300; // ~20 minutes, past any code's expiry

async function follow(section: HTMLElement): Promise<void> {
    for (let i = 0; i < MAX_POLLS; i++) {
        await new Promise((resolve) => setTimeout(resolve, POLL_MS));
        if (document.hidden) {
            continue;
        }
        let state = "";
        try {
            const res = await fetch("/api/me/hardcover", { headers: { Accept: "application/json" }, cache: "no-store" });
            if (!res.ok) {
                continue;
            }
            const body = (await res.json()) as { state?: unknown };
            state = typeof body.state === "string" ? body.state : "";
        } catch {
            continue; // offline for a moment: keep asking
        }
        if (state !== "pending") {
            section.setAttribute("aria-busy", "true");
            location.reload();
            return;
        }
    }
}

/** "Copy code": shown only where the clipboard can be written. */
function wireCopy(section: HTMLElement): void {
    for (const btn of section.querySelectorAll<HTMLButtonElement>("button[data-copy]")) {
        const code = document.getElementById(btn.dataset.copy ?? "");
        if (!code || !navigator.clipboard) {
            continue;
        }
        btn.hidden = false;
        btn.addEventListener("click", () => {
            navigator.clipboard.writeText(code.textContent?.trim() ?? "").then(
                () => {
                    btn.textContent = "Copied";
                    setTimeout(() => (btn.textContent = "Copy code"), 2000);
                },
                () => {
                    btn.textContent = "Select the code and copy it";
                },
            );
        });
    }
}

const section = document.getElementById("hardcover");
if (section?.dataset.state === "pending") {
    wireCopy(section);
    void follow(section);
}
