// The only places our own scripts hand a string to a Trusted Types sink.
//
// Pages send `require-trusted-types-for 'script'` (csp.go): the browser
// refuses a plain string assigned to innerHTML, given to DOMParser, set as
// a <script>'s src, registered as a service worker, compiled by Function,
// and the like. Everything built from data uses DOM APIs instead (el(),
// icon(), textContent, setAttribute). What's left goes through the named
// policies here, each for one narrow job. csp.go lists these names in the
// header's trusted-types directive, so a policy under any other name is
// refused (TestTrustedTypesPolicyNames keeps the two in step):
//
// - serverHtml / parseServerHtml ("cf-server-html"): markup our own server
//   rendered with html/template, which escapes every value by context (the
//   book page's user controls from /book/me/<uuid>; the book popup's HTML).
// - parseInert ("cf-inert-parse"): hostile text (a book's XHTML, OPF, NCX;
//   a looked-up description) parsed into a separate document that runs and
//   loads nothing, to read from.
// - dompurifyPolicy ("cf-dompurify", description.ts): DOMPurify parsing a
//   description into its own inert document to sanitize it. It only
//   parses; nothing it is given is put into this page except DOMPurify's
//   sanitized result, as DOM nodes.
// - scriptUrl / setVendorScriptSrc ("cf-script-url"): /sw.js and our own
//   scripts under /static/ (vendored libraries, bundles, pdf.js's worker).
// - htmxPolicy ("cf-htmx", htmx-security.ts): htmx's swaps of our server's
//   responses; scripts in swapped markup are dropped.
// - allowEpubChapters ("default", reader only): epub.js parsing the book's
//   XML and XHTML with DOMParser (inert, like parseInert) and setting each
//   chapter as its sandboxed, script-less iframe's srcdoc. Any other sink
//   that gets a plain string while it's registered is refused, as without it.
//
// Never pass anything else here: not data from JSON, not a book's contents
// (except to parseInert), not a string built in the browser.

interface TrustedHTMLPolicy {
    createHTML(html: string): unknown;
}
interface TrustedScriptURLPolicy {
    createScriptURL(url: string): unknown;
}
interface TrustedScriptPolicy {
    createScript(script: string): unknown;
}
interface PolicyRules {
    createHTML?(input: string, type?: string, sink?: string): string | null;
    createScript?(input: string, type?: string, sink?: string): string | null;
    createScriptURL?(input: string, type?: string, sink?: string): string | null;
}
declare global {
    interface Window {
        trustedTypes?: {
            createPolicy(name: string, rules: PolicyRules): TrustedHTMLPolicy & TrustedScriptURLPolicy & TrustedScriptPolicy;
        };
    }
}

/** A policy, or null where Trusted Types aren't supported (strings are then fine) or the name is refused. */
function makePolicy(name: string, rules: PolicyRules): (TrustedHTMLPolicy & TrustedScriptURLPolicy & TrustedScriptPolicy) | null {
    try {
        return window.trustedTypes?.createPolicy(name, rules) ?? null;
    } catch {
        return null; // a CSP trusted-types list without this name: assignments are then refused
    }
}

let policy: TrustedHTMLPolicy | null | undefined;

function serverPolicy(): TrustedHTMLPolicy | null {
    policy ??= makePolicy("cf-server-html", { createHTML: (html) => html });
    return policy;
}

/**
 * Replace an element's contents with a fragment our server rendered (a
 * same-origin response from an html/template page, never data). Scripts in
 * it don't run, as with any innerHTML.
 */
export function serverHtml(target: Element, html: string): void {
    const p = serverPolicy();
    (target as unknown as { innerHTML: unknown }).innerHTML = p ? p.createHTML(html) : html;
}

/** Parse a fragment our server rendered (as for serverHtml) into an inert document: nothing loads or runs. */
export function parseServerHtml(html: string): Document {
    const p = serverPolicy();
    return new DOMParser().parseFromString((p ? p.createHTML(html) : html) as string, "text/html");
}

let inertPolicy: TrustedHTMLPolicy | null | undefined;

/**
 * Parse text that may be hostile (a book's own XHTML, OPF or NCX; a
 * description from a lookup) into a separate, inert document: DOMParser
 * documents run no scripts and load nothing. Read from it (textContent,
 * attributes) or hand it to the EPUB reader, which renders chapters in its
 * sandboxed iframe; never move its nodes into this page.
 */
export function parseInert(text: string, type: DOMParserSupportedType): Document {
    inertPolicy ??= makePolicy("cf-inert-parse", { createHTML: (s) => s });
    return new DOMParser().parseFromString((inertPolicy ? inertPolicy.createHTML(text) : text) as string, type);
}

let purifyPolicy: (TrustedHTMLPolicy & TrustedScriptURLPolicy) | null | undefined;

/**
 * The policy DOMPurify parses with (its TRUSTED_TYPES_POLICY option): its
 * own "dompurify" policy isn't in the header's list. Only for
 * DOMPurify.sanitize, never for a sink of ours. Undefined where Trusted
 * Types aren't supported: DOMPurify then parses plain strings.
 */
export function dompurifyPolicy(): (TrustedHTMLPolicy & TrustedScriptURLPolicy) | undefined {
    purifyPolicy ??= makePolicy("cf-dompurify", {
        createHTML: (s) => s,
        createScriptURL: () => null, // DOMPurify requires the hook; it loads nothing
    });
    return purifyPolicy ?? undefined;
}

/** Our own scripts: /sw.js, or a file under /static/ (no "..", no other host). */
function isOwnScript(url: string): boolean {
    return (url === "/sw.js" || url.startsWith("/static/")) && !url.includes("..") && !url.includes("\\");
}

let scriptPolicy: TrustedScriptURLPolicy | null | undefined;

/**
 * A script URL for a sink that loads code (a service worker, a Worker, a
 * <script>'s src): /sw.js or one of our files under /static/; anything
 * else throws.
 */
export function scriptUrl(url: string): string {
    if (!isOwnScript(url)) throw new Error(`not one of our scripts: ${url}`);
    scriptPolicy ??= makePolicy("cf-script-url", {
        createScriptURL: (u) => {
            if (!isOwnScript(u)) throw new Error(`not one of our scripts: ${u}`);
            return u;
        },
    });
    // A TrustedScriptURL where supported: the sinks take it as a string.
    return (scriptPolicy ? scriptPolicy.createScriptURL(url) : url) as string;
}

/** Set a <script>'s src to one of our vendored libraries (a path under /static/vendor/); anything else throws. */
export function setVendorScriptSrc(script: HTMLScriptElement, src: string): void {
    if (!src.startsWith("/static/vendor/")) throw new Error(`not a vendored script: ${src}`);
    script.src = scriptUrl(src);
}

/**
 * The policy htmx parses responses and runs swapped scripts with
 * (htmx-security.ts). Responses come from our own server, rendered by
 * html/template; a <script> in swapped markup is dropped rather than run
 * (none of our fragments has one: the book popup is book.html without its
 * Standalone parts).
 */
export function htmxPolicy(): { createHTML(html: string): unknown; createScript(script: string): unknown } {
    const rules = {
        createHTML: (html: string) => html,
        createScript: (script: string) => {
            if (script.trim()) console.warn("htmx: a script in swapped content was not run");
            return "";
        },
    };
    return makePolicy("cf-htmx", rules) ?? rules;
}

let epubAllowed = false;

/** The sinks epub.js hands plain strings to, both safe for a book's contents. */
const EPUB_SINKS = new Set([
    // The book's container, OPF, NCX and XHTML, parsed into a separate
    // document that runs and loads nothing (as parseInert).
    "DOMParser parseFromString",
    // A chapter, shown in an iframe sandboxed without allow-scripts: the
    // book's own scripts never run.
    "HTMLIFrameElement srcdoc",
]);

/**
 * Let epub.js parse a book and show its chapters: it hands the browser
 * plain strings we can't wrap. This is the "default" policy, which the
 * browser asks about every plain string; it answers yes for epub.js's two
 * sinks only, so anything else is refused (and reported, and warned about
 * in the console) as before. Call it on the reader page only.
 */
export function allowEpubChapters(): void {
    if (epubAllowed) return;
    epubAllowed = true;
    makePolicy("default", {
        createHTML: (html, _type, sink) => {
            if (sink && EPUB_SINKS.has(sink)) return html;
            console.warn(`Trusted Types: refused a plain string at ${sink ?? "a sink"}`);
            return null;
        },
    });
}
