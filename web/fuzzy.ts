/**
 * Fuzzy name matching for search boxes over short names (authors, tags,
 * series…). A query matches a name, best first:
 *
 *   1. the name starts with it               "tolk"     → "Tolkien, J. R. R."
 *   2. a word of the name starts with it     "tolk"     → "J. R. R. Tolkien"
 *   3. the name contains it                  "kien"     → "Tolkien"
 *   4. every query word matches a word of the name, in any order, allowing
 *      typos (one in 4+ letters, two in 8+) "tolkein j" → "J. R. R. Tolkien"
 *   5. the query's letters appear in order, each one right after the last
 *      or at the start of a word            "jrrt"     → "J. R. R. Tolkien"
 *                                           "scifi"    → "Science Fiction"
 *
 * Everything is compared without case or accents ("emile" finds "Émile").
 */

/** Lower case without accents: "Émile" and "emile" match. */
export function fold(s: string): string {
    return s.normalize("NFD").replace(/\p{M}/gu, "").toLowerCase();
}

const WORD_SPLIT = /[^\p{L}\p{N}]+/u;

/** A name prepared for matching: folded once, not on every key. */
export interface Hay {
    text: string; // folded
    words: string[]; // folded words, punctuation dropped
    starts: Uint8Array; // starts[i] = 1 if text[i] begins a word
}

export function prepare(s: string): Hay {
    const text = fold(s);
    const starts = new Uint8Array(text.length);
    let prevWordChar = false;
    for (let i = 0; i < text.length; i++) {
        const isWordChar = !WORD_SPLIT.test(text[i] ?? "");
        if (isWordChar && !prevWordChar) starts[i] = 1;
        prevWordChar = isWordChar;
    }
    return { text, words: text.split(WORD_SPLIT).filter(Boolean), starts };
}

/** A query prepared once per keystroke. */
export interface Query {
    text: string; // folded, trimmed
    tokens: string[];
    compact: string; // letters and digits only, for initials matching
}

export function prepareQuery(q: string): Query {
    const text = fold(q.trim());
    const tokens = text.split(WORD_SPLIT).filter(Boolean);
    return { text, tokens, compact: tokens.join("") };
}

/** Typos a query word of this length may have. */
function allowedTypos(len: number): number {
    return len >= 8 ? 2 : len >= 4 ? 1 : 0;
}

/**
 * Edit distance (insert, delete, substitute, swap neighbours) between a and
 * b, or max+1 as soon as it must exceed max.
 */
function distance(a: string, b: string, max: number): number {
    if (Math.abs(a.length - b.length) > max) return max + 1;
    let prev2: number[] = [];
    let prev = Array.from({ length: b.length + 1 }, (_, j) => j);
    for (let i = 1; i <= a.length; i++) {
        const cur = [i];
        let rowMin = i;
        for (let j = 1; j <= b.length; j++) {
            const cost = a[i - 1] === b[j - 1] ? 0 : 1;
            let d = Math.min((prev[j] ?? 0) + 1, (cur[j - 1] ?? 0) + 1, (prev[j - 1] ?? 0) + cost);
            if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) {
                d = Math.min(d, (prev2[j - 2] ?? 0) + 1);
            }
            cur.push(d);
            if (d < rowMin) rowMin = d;
        }
        if (rowMin > max) return max + 1;
        prev2 = prev;
        prev = cur;
    }
    return prev[b.length] ?? max + 1;
}

/**
 * How well one query word matches one name word: 3 prefix, 2 contains,
 * 1 within the allowed typos (of the whole word or of its start, so a
 * half-typed word with a typo still matches), 0 not at all.
 */
function wordScore(token: string, word: string): number {
    if (word.startsWith(token)) return 3;
    if (token.length >= 3 && word.includes(token)) return 2;
    const max = allowedTypos(token.length);
    if (max === 0) return 0;
    if (distance(token, word, max) <= max) return 1;
    // a prefix of the word as long as the token, give or take a letter
    for (let len = token.length - max; len <= token.length + max; len++) {
        if (len < 1 || len >= word.length) continue;
        if (distance(token, word.slice(0, len), max) <= max) return 1;
    }
    return 0;
}

/** Every query word matches a different name word: the summed word scores, else 0. */
function tokensScore(tokens: string[], words: string[]): number {
    const used = new Uint8Array(words.length);
    let total = 0;
    for (const t of tokens) {
        let best = 0;
        let bestAt = -1;
        for (let i = 0; i < words.length; i++) {
            if (used[i]) continue;
            const s = wordScore(t, words[i] ?? "");
            if (s > best) {
                best = s;
                bestAt = i;
                if (s === 3) break;
            }
        }
        if (!best) return 0;
        used[bestAt] = 1;
        total += best;
    }
    return total;
}

/**
 * The query's letters in order in the name, each right after the previous
 * one or at the start of a word ("jrrt", "scifi"). Greedy, preferring to
 * continue the current run. Returns the number of runs (fewer is better),
 * else 0.
 */
function initialsRuns(q: string, h: Hay): number {
    if (q.length < 2) return 0;
    let qi = 0;
    let runs = 0;
    let last = -2;
    for (let i = 0; i < h.text.length && qi < q.length; i++) {
        if (h.text[i] !== q[qi]) continue;
        if (i === last + 1) {
            qi++;
            last = i;
        } else if (h.starts[i]) {
            qi++;
            last = i;
            runs++;
        }
    }
    return qi === q.length ? runs : 0;
}

/** Score of a query against one name: higher is better, 0 is no match. */
export function score(q: Query, h: Hay): number {
    if (!q.text) return 0;
    if (h.text.startsWith(q.text)) return 1000;
    const at = h.text.indexOf(q.text);
    if (at >= 0) return h.starts[at] ? 900 : 800;
    if (q.tokens.length) {
        const t = tokensScore(q.tokens, h.words);
        // 500 + up to 3 per word, fewer typos ranking higher
        if (t) return 500 + (t * 100) / (3 * q.tokens.length);
    }
    const runs = initialsRuns(q.compact, h);
    if (runs) return 100 + 300 / runs;
    return 0;
}

/** Best score of a query against several names (a name and its aliases). */
export function bestScore(q: Query, hays: Hay[]): number {
    let best = 0;
    for (const h of hays) {
        const s = score(q, h);
        if (s > best) best = s;
    }
    return best;
}
