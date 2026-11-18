// The only places our own scripts hand a string to a Trusted Types sink.
//
// Pages send `require-trusted-types-for 'script'` (csp.go): the browser
// refuses (or, in report-only mode, reports) a plain string assigned to
// innerHTML, given to DOMParser, set as a <script>'s src, and the like.
// Everything built from data uses DOM APIs instead (el(), icon(),
// textContent, setAttribute). What's left goes through the named policies
// here, each for one narrow job:
//
// - serverHtml / parseServerHtml ("cf-server-html"): markup our own server
//   rendered with html/template, which escapes every value by context (the
//   book page's user controls from /book/me/<uuid>; the book popup's HTML).
// - parseInert ("cf-inert-parse"): hostile text (a book's XHTML, OPF, NCX;
//   a looked-up description) parsed into a separate document that runs and
//   loads nothing, to read from.
// - setVendorScriptSrc ("cf-vendor-script"): one of our vendored libraries (static/vendor).
//
// Never pass anything else here: not data from JSON, not a book's contents
// (except to parseInert), not a string built in the browser.

interface TrustedHTMLPolicy {
    createHTML(html: string): unknown;
}
interface TrustedScriptURLPolicy {
    createScriptURL(url: string): unknown;
}
declare global {
    interface Window {
        trustedTypes?: {
            createPolicy(name: string, rules: { createHTML?(html: string): string; createScriptURL?(url: string): string }): TrustedHTMLPolicy & TrustedScriptURLPolicy;
        };
    }
}

let policy: TrustedHTMLPolicy | null | undefined;

function serverPolicy(): TrustedHTMLPolicy | null {
    if (policy === undefined) {
        try {
            policy = window.trustedTypes?.createPolicy("cf-server-html", { createHTML: (html) => html }) ?? null;
        } catch {
            policy = null; // a CSP trusted-types list without this name: assignment is reported/refused below
        }
    }
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
    if (inertPolicy === undefined) {
        try {
            inertPolicy = window.trustedTypes?.createPolicy("cf-inert-parse", { createHTML: (s) => s }) ?? null;
        } catch {
            inertPolicy = null;
        }
    }
    return new DOMParser().parseFromString((inertPolicy ? inertPolicy.createHTML(text) : text) as string, type);
}

let scriptPolicy: TrustedScriptURLPolicy | null | undefined;

/** Set a <script>'s src to one of our vendored libraries (a path under /static/vendor/); anything else throws. */
export function setVendorScriptSrc(script: HTMLScriptElement, src: string): void {
    if (!src.startsWith("/static/vendor/") || src.includes("..")) throw new Error(`not a vendored script: ${src}`);
    if (scriptPolicy === undefined) {
        try {
            scriptPolicy =
                window.trustedTypes?.createPolicy("cf-vendor-script", {
                    createScriptURL: (url) => {
                        if (!url.startsWith("/static/vendor/") || url.includes("..")) throw new Error(`not a vendored script: ${url}`);
                        return url;
                    },
                }) ?? null;
        } catch {
            scriptPolicy = null;
        }
    }
    (script as unknown as { src: unknown }).src = scriptPolicy ? scriptPolicy.createScriptURL(src) : src;
}
