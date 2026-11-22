/** Calibre's ratings: 0–10 half-stars (Go: rating.go). */

/** A rating ("8", half-stars) as words: "4 stars", "4½ stars"; "" for none. */
export function ratingLabel(v: string): string {
    const n = Number(v);
    if (!v || !Number.isInteger(n) || n <= 0 || n > 10) return "";
    const whole = Math.floor(n / 2);
    const half = n % 2 === 1 ? "½" : "";
    return `${whole || half ? `${whole || ""}${half}` : "0"} ${n <= 2 ? "star" : "stars"}`;
}

/**
 * A 0–5 star picker in half stars over a form field holding Calibre's
 * rating ("" or "1"–"10"). A click on a star's left half picks the half,
 * on its right half the whole star; clicking the current rating clears it.
 * Arrow keys step by half a star, Home/Delete clear, End picks 5 stars.
 * Changes dispatch "input" on the field; its value set elsewhere (followed
 * by "input") redraws the stars.
 */
export function mountStarRating(box: HTMLElement, input: HTMLInputElement): void {
    const stars: HTMLSpanElement[] = [];
    for (let i = 0; i < 5; i++) {
        const s = document.createElement("span");
        s.className = "star";
        s.textContent = "★";
        s.setAttribute("aria-hidden", "true");
        stars.push(s);
    }
    box.replaceChildren(...stars);
    box.classList.add("star-rating");
    box.tabIndex = 0;
    box.setAttribute("role", "slider");
    box.setAttribute("aria-label", "Rating");
    box.setAttribute("aria-valuemin", "0");
    box.setAttribute("aria-valuemax", "10");

    const value = (): number => {
        const n = Number(input.value);
        return Number.isInteger(n) && n > 0 && n <= 10 ? n : 0;
    };
    const draw = (n: number): void => {
        stars.forEach((s, i) => {
            const fill = n - i * 2;
            s.classList.toggle("star-full", fill >= 2);
            s.classList.toggle("star-half", fill === 1);
            s.classList.toggle("star-empty", fill <= 0);
        });
    };
    const render = (): void => {
        const n = value();
        const label = ratingLabel(String(n)) || "No rating";
        box.setAttribute("aria-valuenow", String(n));
        box.setAttribute("aria-valuetext", label);
        box.title = `${label} (click a star to change)`;
        draw(n);
    };
    const set = (n: number): void => {
        n = Math.max(0, Math.min(10, n));
        if (n === value()) return render();
        input.value = n ? String(n) : "";
        input.dispatchEvent(new Event("input", { bubbles: true }));
        render();
    };
    /** The rating a pointer over the stars points at. */
    const at = (e: MouseEvent): number => {
        const s = (e.target as Element).closest?.(".star");
        const i = stars.indexOf(s as HTMLSpanElement);
        if (i < 0) return -1;
        const r = s!.getBoundingClientRect();
        return i * 2 + (e.clientX - r.left < r.width / 2 ? 1 : 2);
    };

    box.addEventListener("click", (e) => {
        const n = at(e);
        if (n >= 0) set(n === value() ? 0 : n);
    });
    box.addEventListener("mousemove", (e) => {
        const n = at(e);
        if (n >= 0) draw(n);
    });
    box.addEventListener("mouseleave", render);
    box.addEventListener("keydown", (e) => {
        const n = value();
        const next: Record<string, number> = {
            ArrowRight: n + 1, ArrowUp: n + 1, ArrowLeft: n - 1, ArrowDown: n - 1,
            Home: 0, Delete: 0, Backspace: 0, End: 10,
        };
        if (!(e.key in next)) return;
        e.preventDefault();
        set(next[e.key]!);
    });
    input.addEventListener("input", render);
    render();
}
