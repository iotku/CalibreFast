// Small typed DOM helpers shared by the page scripts.
/**
 * The element with this id, checked to be of the expected type. Throws if
 * it's missing or the wrong kind: a template/script mismatch should fail
 * loudly rather than as "cannot read properties of null" somewhere later.
 */
export function byId(id, type) {
    const el = document.getElementById(id);
    if (!el)
        throw new Error(`#${id} not found`);
    if (type && !(el instanceof type))
        throw new Error(`#${id} is not a ${type.name}`);
    return el;
}
/** Like byId, but returns null when the element is optional on this page. */
export function maybeId(id, type) {
    const el = document.getElementById(id);
    if (!el || (type && !(el instanceof type)))
        return null;
    return el;
}
/** Create an element with properties and children in one go. */
export function el(tag, props = {}, ...children) {
    const e = Object.assign(document.createElement(tag), props);
    e.append(...children);
    return e;
}
/** Message from an unknown thrown value. */
export function errorMessage(err) {
    return err instanceof Error ? err.message : String(err);
}
/** fetch() JSON helper: throws with the server's text on non-2xx. */
export async function api(method, url, body, init = {}) {
    const res = await fetch(url, {
        method,
        credentials: "same-origin",
        headers: body === undefined ? {} : { "content-type": "application/json" },
        body: body === undefined ? null : JSON.stringify(body),
        ...init,
    });
    if (!res.ok) {
        const err = new Error((await res.text()).trim() || `HTTP ${res.status}`);
        err.status = res.status;
        throw err;
    }
    return (res.status === 204 ? null : await res.json());
}
/**
 * A status line (success/error message) for an element that starts hidden.
 * Returns say(text, isError).
 */
export function statusLine(id) {
    const el = maybeId(id);
    return (text, isError = false) => {
        if (!el)
            return;
        el.hidden = false;
        el.textContent = text;
        el.className = isError ? "auth-error" : "auth-notice";
    };
}
