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

/**
 * A 32-bit set of the characters in s (code point mod 32). A bit that isn't
 * set means that character is certainly absent, so masks rule names out
 * cheaply before the slower checks; a set bit proves nothing.
 */
function charMask(s: string): number {
    let m = 0;
    for (let i = 0; i < s.length; i++) m |= 1 << (s.charCodeAt(i) & 31);
    return m;
}

/** How many characters of s are certainly not in a string with this mask. */
function missing(s: string, mask: number): number {
    let n = 0;
    for (let i = 0; i < s.length; i++) if (!(mask & (1 << (s.charCodeAt(i) & 31)))) n++;
    return n;
}

/** A name prepared for matching: folded once, not on every key. */
export interface Hay {
    text: string; // folded
    words: string[]; // folded words, punctuation dropped
    wordMasks: number[]; // charMask of each word
    mask: number; // charMask of text
    starts: Uint8Array; // starts[i] = 1 if text[i] begins a word
}

const WORD_CHAR = /[\p{L}\p{N}]/u;

/** Whether text[i] is part of a word: a letter or digit (ASCII without a regex). */
function isWordChar(text: string, i: number): boolean {
    const c = text.charCodeAt(i);
    if (c < 128) return (c >= 97 && c <= 122) || (c >= 48 && c <= 57) || (c >= 65 && c <= 90);
    if (c >= 0xd800 && c <= 0xdfff) {
        // half of a pair: test the whole character
        const at = c >= 0xdc00 && i > 0 ? i - 1 : i;
        return WORD_CHAR.test(String.fromCodePoint(text.codePointAt(at) ?? c));
    }
    return WORD_CHAR.test(text.charAt(i));
}

export function prepare(s: string): Hay {
    const text = fold(s);
    const starts = new Uint8Array(text.length);
    const words: string[] = [];
    const wordMasks: number[] = [];
    let mask = 0;
    let start = -1;
    let wm = 0;
    for (let i = 0; i <= text.length; i++) {
        if (i < text.length && isWordChar(text, i)) {
            if (start < 0) {
                start = i;
                starts[i] = 1;
                wm = 0;
            }
            wm |= 1 << (text.charCodeAt(i) & 31);
        } else if (start >= 0) {
            words.push(text.slice(start, i));
            wordMasks.push(wm);
            mask |= wm;
            start = -1;
        }
    }
    return { text, words, wordMasks, mask, starts };
}

/** A query prepared once per keystroke. */
export interface Query {
    text: string; // folded, trimmed
    tokens: string[];
    typos: number[]; // allowedTypos of each token
    compact: string; // letters and digits only, for initials matching
    compactMask: number;
}

export function prepareQuery(q: string): Query {
    const text = fold(q.trim());
    const tokens = text.split(WORD_SPLIT).filter(Boolean);
    const compact = tokens.join("");
    return { text, tokens, typos: tokens.map((t) => allowedTypos(t.length)), compact, compactMask: charMask(compact) };
}

/** Typos a query word of this length may have. */
function allowedTypos(len: number): number {
    return len >= 8 ? 2 : len >= 4 ? 1 : 0;
}

// Rows of the edit-distance table, reused: no allocation per comparison.
let rowA = new Int32Array(64);
let rowB = new Int32Array(64);
let rowC = new Int32Array(64);

/**
 * Whether token is within max edits (insert, delete, substitute, swap
 * neighbours) of the whole word or of a prefix of it as long as the token,
 * give or take max letters, so a half-typed word with a typo still matches.
 * One table serves every prefix: its last row holds the distance from the
 * token to each prefix of the word.
 */
function nearWord(token: string, word: string, max: number): boolean {
    const n = token.length;
    const cols = Math.min(word.length, n + max); // longer prefixes can't be within max
    if (cols < 1 || n - cols > max) return false;
    if (rowA.length <= cols) {
        rowA = new Int32Array(cols * 2);
        rowB = new Int32Array(cols * 2);
        rowC = new Int32Array(cols * 2);
    }
    let prev2 = rowA;
    let prev = rowB;
    let cur = rowC;
    for (let j = 0; j <= cols; j++) prev[j] = j;
    for (let i = 1; i <= n; i++) {
        const a = token.charCodeAt(i - 1);
        const aBefore = i > 1 ? token.charCodeAt(i - 2) : -1;
        cur[0] = i;
        let rowMin = i;
        for (let j = 1; j <= cols; j++) {
            const b = word.charCodeAt(j - 1);
            let d = Math.min((prev[j] as number) + 1, (cur[j - 1] as number) + 1, (prev[j - 1] as number) + (a === b ? 0 : 1));
            if (i > 1 && j > 1 && a === word.charCodeAt(j - 2) && aBefore === b) {
                d = Math.min(d, (prev2[j - 2] as number) + 1);
            }
            cur[j] = d;
            if (d < rowMin) rowMin = d;
        }
        if (rowMin > max) return false;
        const t = prev2;
        prev2 = prev;
        prev = cur;
        cur = t;
    }
    for (let j = Math.max(1, n - max); j <= cols; j++) {
        if ((prev[j] as number) <= max) return true;
    }
    return false;
}

/**
 * How well one query word matches one name word: 3 prefix, 2 contains,
 * 1 within the allowed typos (of the whole word or of its start), 0 not at all.
 */
function wordScore(token: string, max: number, word: string, wordMask: number): number {
    if (word.startsWith(token)) return 3;
    if (token.length >= 3 && word.includes(token)) return 2;
    if (max === 0) return 0;
    // Each token letter missing from the word costs an edit.
    if (missing(token, wordMask) > max) return 0;
    return nearWord(token, word, max) ? 1 : 0;
}

/** Every query word matches a different name word: the summed word scores, else 0. */
function tokensScore(q: Query, h: Hay): number {
    const { tokens, typos } = q;
    const { words, wordMasks } = h;
    // Cheap rejection first: a word too far off for the whole name can't be near any of its words.
    for (let k = 0; k < tokens.length; k++) {
        if (missing(tokens[k] ?? "", h.mask) > (typos[k] ?? 0)) return 0;
    }
    const used = new Uint8Array(words.length);
    let total = 0;
    for (let k = 0; k < tokens.length; k++) {
        const t = tokens[k] ?? "";
        const max = typos[k] ?? 0;
        let best = 0;
        let bestAt = -1;
        for (let i = 0; i < words.length; i++) {
            if (used[i]) continue;
            const s = wordScore(t, max, words[i] ?? "", wordMasks[i] ?? 0);
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
function initialsRuns(q: string, qMask: number, h: Hay): number {
    if (q.length < 2 || (qMask & h.mask) !== qMask) return 0;
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
        const t = tokensScore(q, h);
        // 500 + up to 3 per word, fewer typos ranking higher
        if (t) return 500 + (t * 100) / (3 * q.tokens.length);
    }
    const runs = initialsRuns(q.compact, q.compactMask, h);
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
