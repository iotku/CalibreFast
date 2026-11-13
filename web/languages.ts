/**
 * Language codes for the metadata editor: names and preferred (3-letter)
 * codes for the 2- and 3-letter codes books use ("en", "eng", "fre", …).
 * The table comes from the server (Go: languageTable in languages.go).
 *
 * Everything here is synchronous and falls back to the raw code until the
 * table has loaded; call loadLanguageCodes() and re-render once it resolves.
 */

interface LanguageCode {
    name: string;
    /** Preferred code for the language, e.g. "eng" for "en". */
    code: string;
}

let table: Record<string, LanguageCode> = {};
let loading: Promise<void> | null = null;

export function loadLanguageCodes(): Promise<void> {
    loading ??= fetch("/api/languages/codes")
        .then((r) => (r.ok ? r.json() : {}))
        .then((t: Record<string, LanguageCode>) => {
            table = t;
        })
        .catch(() => {
            loading = null; // retry next time
        });
    return loading;
}

/** "en_US" → ["en", "US"]: the base code and any subtags as written. */
function split(code: string): [string, string] {
    const t = code.trim().replace(/_/g, "-");
    const i = t.indexOf("-");
    return i === -1 ? [t, ""] : [t.slice(0, i), t.slice(i + 1)];
}

/**
 * The code to prefer for a value: a known bare code becomes its 3-letter
 * form ("en" → "eng", "fre" → "fra"); anything else is kept as written.
 */
export function preferredLanguageCode(code: string): string {
    const [base, rest] = split(code);
    const known = table[base.toLowerCase()];
    return known && !rest ? known.code : code.trim();
}

/** Identity of a language value: "en", "ENG" and "eng" share one key. */
export function languageKey(code: string): string {
    const [base, rest] = split(code);
    const known = table[base.toLowerCase()];
    return ((known ? known.code : base) + (rest ? `-${rest}` : "")).toLowerCase();
}

/** "English" for "eng" or "en"; an unknown code as is. */
export function languageName(code: string): string {
    return table[split(code)[0].toLowerCase()]?.name ?? code;
}

/** Chip label: "English (eng)", "English (en-US)"; unknown codes as is. */
export function languageChipLabel(code: string): string {
    const known = table[split(code)[0].toLowerCase()];
    return known ? `${known.name} (${code})` : code;
}
