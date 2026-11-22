/** Calibre's ratings: 0–10 half-stars (Go: rating.go). */

/** A rating ("8", half-stars) as words: "4 stars", "4½ stars"; "" for none. */
export function ratingLabel(v: string): string {
    const n = Number(v);
    if (!v || !Number.isInteger(n) || n <= 0 || n > 10) return "";
    const whole = Math.floor(n / 2);
    const half = n % 2 === 1 ? "½" : "";
    return `${whole || half ? `${whole || ""}${half}` : "0"} ${n <= 2 ? "star" : "stars"}`;
}
