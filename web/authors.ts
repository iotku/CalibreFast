// How the editor reads and writes authors.
//   - Several authors are written "A & B", as Calibre does, and the field is
//     read by splitting on "&" only, keeping names as written: the same as
//     splitAuthors in authors.go (keep the two in sync), so Compare shows
//     what's saved.
//   - A list written another way ("Hailu, Gebreyesus; Negash, Ghirmai") or a
//     name written "Last, First" is only rewritten when the admin clicks
//     Split: tidyAuthors is that guess, and it can be wrong.

/** Come after a comma without making a name "Last, First". */
const SUFFIXES = new Set(["jr", "sr", "ii", "iii", "iv", "phd", "md"]);

/** Names without empties or repeats (ignoring case), spaces tidied. */
function uniqueNames(parts: string[]): string[] {
    const out: string[] = [];
    const seen = new Set<string>();
    for (const p of parts) {
        const name = p.split(/\s+/).filter(Boolean).join(" ");
        if (name && !seen.has(name.toLowerCase())) {
            seen.add(name.toLowerCase());
            out.push(name);
        }
    }
    return out;
}

/** The authors in the field: "A & B". */
export function splitAuthors(s: string | null | undefined): string[] {
    return uniqueNames((s ?? "").split("&"));
}

/** Authors as the field shows them. */
export function joinAuthors(names: string[]): string {
    return names.join(" & ");
}

/** "Last, First" → "First Last". */
function firstLast(name: string): string {
    const i = name.indexOf(",");
    if (i < 0) return name;
    const last = name.slice(0, i).trim();
    const first = name.slice(i + 1).trim();
    if (!last || !first || first.includes(",") || SUFFIXES.has(first.replace(/^[. ]+|[. ]+$/g, "").toLowerCase())) return name;
    return `${first} ${last}`;
}

/**
 * The field's authors as Split would write them:
 *   - ";" and "&" separate authors. Without them, commas do only if every
 *     part is a full name ("Alan Donovan, Brian Kernighan"); otherwise the
 *     comma is inside one name ("Donovan, Alan").
 *   - "Last, First" becomes "First Last" (not "King, Jr." or names with more
 *     than one comma).
 */
export function tidyAuthors(s: string | null | undefined): string[] {
    const text = s ?? "";
    let parts: string[];
    if (/[;&]/.test(text)) {
        parts = text.split(/[;&]/);
    } else if (text.includes(",")) {
        parts = text.split(",");
        if (parts.some((p) => !p.trim().includes(" "))) parts = [text]; // "Last, First": one name
    } else {
        parts = [text];
    }
    return uniqueNames(parts.map((p) => firstLast(p.split(/\s+/).filter(Boolean).join(" "))));
}
