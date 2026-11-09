import { byId } from "./dom.js";

/**
 * The readers' Contents/Bookmarks sidebar (#toc-sidebar, opened by #toc-btn).
 * Open state is the "open" class (CSS slides it in below the top bar).
 * Closes on the button, Escape, or a press in the reading area. That press
 * only closes: the click that follows doesn't also turn the page.
 */
export interface Sidebar {
    close(): void;
}

export function setupSidebar(): Sidebar {
    const sidebar = byId("toc-sidebar");
    const btn = byId("toc-btn");
    const isOpen = (): boolean => sidebar.classList.contains("open");
    const set = (open: boolean): void => {
        sidebar.classList.toggle("open", open);
        btn.setAttribute("aria-expanded", String(open));
        if (open) sidebar.querySelector("[aria-current]")?.scrollIntoView({ block: "center" });
    };
    const close = (): void => {
        if (isOpen()) set(false);
    };

    // Start right below the top bar, whose height depends on how its
    // controls wrap (more than --topbar-total on some phones).
    const bar = document.querySelector<HTMLElement>(".top-bar");
    if (bar) {
        new ResizeObserver(() => sidebar.style.setProperty("--sidebar-top", `${bar.getBoundingClientRect().bottom}px`)).observe(bar);
    }

    btn.addEventListener("click", () => set(!isOpen()));
    document.addEventListener("keydown", (e) => {
        if (e.key === "Escape") close();
    });

    // A press outside the sidebar and the top bar dismisses it; swallow the
    // click it turns into.
    let dismissedAt = 0;
    document.addEventListener(
        "pointerdown",
        (e) => {
            const t = e.target;
            if (!isOpen() || !(t instanceof Node) || sidebar.contains(t)) return;
            if ((t instanceof Element ? t : t.parentElement)?.closest(".top-bar")) return;
            close();
            dismissedAt = e.timeStamp;
        },
        true,
    );
    document.addEventListener(
        "click",
        (e) => {
            if (dismissedAt && e.timeStamp - dismissedAt < 1000) {
                e.stopPropagation();
                e.preventDefault();
            }
            dismissedAt = 0;
        },
        true,
    );

    return { close };
}
