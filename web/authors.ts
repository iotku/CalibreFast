// How the editor reads and writes authors: the same rules as authors.go
// (parseAuthors), so the field, Compare and what's saved agree. Keep the
// two in sync.
//   - Several authors are written "A & B", as Calibre does.
//   - ";" and "&" separate authors. Without them, commas do only if every
//     part is a full name ("Alan Donovan, Brian Kernighan"); otherwise the
//     comma is inside one name ("Donovan, Alan").
//   - "Last, First" becomes "First Last".

const SEPARATOR = /[;&]/;
/** Come after a comma without making a name "Last, First". */
const SUFFIXES = new Set(["jr", "sr", "ii", "iii", "iv", "phd", "md"]);

/** "Last, First" → "First Last". */
function firstLast(name: string): string {
    const i = name.indexOf(",");
    if (i < 0) return name;
    const last = name.slice(0, i).trim();
    const first = name.slice(i + 1).trim();
    if (!last || !first || first.includes(",") || SUFFIXES.has(first.replace(/^[. ]+|[. ]+$/g, "").toLowerCase())) return name;
    return `${first} ${last}`;
}

/** The authors in a field's text, each as "First Last", without repeats. */
export function parseAuthors(s: string | null | undefined): string[] {
    const text = s ?? "";
    let parts: string[];
    if (SEPARATOR.test(text)) {
        parts = text.split(SEPARATOR);
    } else if (text.includes(",")) {
        parts = text.split(",");
        if (parts.some((p) => !p.trim().includes(" "))) parts = [text]; // "Last, First": one name
    } else {
        parts = [text];
    }
    const out: string[] = [];
    const seen = new Set<string>();
    for (const p of parts) {
        const name = firstLast(p.split(/\s+/).filter(Boolean).join(" "));
        if (name && !seen.has(name.toLowerCase())) {
            seen.add(name.toLowerCase());
            out.push(name);
        }
    }
    return out;
}

/** Authors as the field shows them. */
export function joinAuthors(names: string[]): string {
    return names.join(" & ");
}
