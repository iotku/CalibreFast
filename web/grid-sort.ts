/**
 * The sort select above a book grid (templates/grid-sort.html, Go:
 * catalog_sort.go). Choosing an order reloads the page with ?sort= (from the
 * first page), and remembers it in this browser for pages of the same kind
 * (Latest, an author, a tag, search, …). A page opened without ?sort= takes
 * the remembered order: sortRedirecting is then true, and the grid doesn't
 * start, since the page is about to be replaced.
 */

const box = document.querySelector<HTMLElement>(".grid-sort");
const select = box?.querySelector<HTMLSelectElement>("select.grid-sort-select") ?? null;
const kind = box?.dataset["sortKind"] ?? "";
const fallback = box?.dataset["sortDefault"] ?? "";
const storageKey = `cf-sort-${kind}`;

function remembered(): string | null {
    try {
        return localStorage.getItem(storageKey);
    } catch {
        return null; // storage blocked
    }
}

function remember(key: string): void {
    try {
        if (key === fallback) localStorage.removeItem(storageKey);
        else localStorage.setItem(storageKey, key);
    } catch {
        /* storage blocked: the URL still has it */
    }
}

/** This page's URL in order key, from the first page. */
function urlFor(key: string): string {
    const params = new URLSearchParams(window.location.search);
    if (key === fallback) params.delete("sort");
    else params.set("sort", key);
    params.delete("page");
    const q = params.toString();
    return window.location.pathname + (q ? `?${q}` : "");
}

function offered(key: string): boolean {
    return !!select && Array.from(select.options).some((o) => o.value === key);
}

function applyRemembered(): boolean {
    if (!select || !kind) return false;
    const params = new URLSearchParams(window.location.search);
    if (params.has("sort") || params.has("page")) return false; // the link says how
    const want = remembered();
    if (!want || want === fallback || want === select.value || !offered(want)) return false;
    window.location.replace(urlFor(want));
    return true;
}

export const sortRedirecting = applyRemembered();

select?.addEventListener("change", () => {
    const key = select.value;
    if (!offered(key)) return;
    remember(key);
    window.location.assign(urlFor(key));
});
