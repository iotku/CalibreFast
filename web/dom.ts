// Small typed DOM helpers shared by the page scripts.

/**
 * The element with this id, checked to be of the expected type. Throws if
 * it's missing or the wrong kind: a template/script mismatch should fail
 * loudly rather than as "cannot read properties of null" somewhere later.
 */
export function byId<T extends HTMLElement = HTMLElement>(id: string, type?: new () => T): T {
    const el = document.getElementById(id);
    if (!el) throw new Error(`#${id} not found`);
    if (type && !(el instanceof type)) throw new Error(`#${id} is not a ${type.name}`);
    return el as T;
}

/** Like byId, but returns null when the element is optional on this page. */
export function maybeId<T extends HTMLElement = HTMLElement>(id: string, type?: new () => T): T | null {
    const el = document.getElementById(id);
    if (!el || (type && !(el instanceof type))) return null;
    return el as T;
}

/** Create an element with properties and children in one go. */
export function el<K extends keyof HTMLElementTagNameMap>(
    tag: K,
    props: Partial<HTMLElementTagNameMap[K]> = {},
    ...children: (Node | string)[]
): HTMLElementTagNameMap[K] {
    const e = Object.assign(document.createElement(tag), props);
    e.append(...children);
    return e;
}

/**
 * A click on the backdrop (outside the box) closes a modal dialog. Only
 * clicks on the dialog itself count: a button pressed with Enter or Space
 * reports (0, 0), which is "outside" too.
 */
export function closeOnBackdropClick(dialog: HTMLDialogElement): void {
    dialog.addEventListener("click", (e) => {
        if (e.target !== dialog) return;
        const r = dialog.getBoundingClientRect();
        if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) dialog.close();
    });
}

/** Whether a modal dialog is open: the keyboard is its own until it closes. */
export function modalOpen(): boolean {
    return document.querySelector("dialog[open]") !== null;
}

/** Message from an unknown thrown value. */
export function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
}

/** fetch() JSON helper: throws with the server's text on non-2xx. */
export async function api<T = unknown>(method: string, url: string, body?: unknown, init: RequestInit = {}): Promise<T> {
    const res = await fetch(url, {
        method,
        credentials: "same-origin",
        headers: body === undefined ? {} : { "content-type": "application/json" },
        body: body === undefined ? null : JSON.stringify(body),
        ...init,
    });
    if (!res.ok) {
        const err = new Error((await res.text()).trim() || `HTTP ${res.status}`) as Error & { status?: number };
        err.status = res.status;
        throw err;
    }
    return (res.status === 204 ? null : await res.json()) as T;
}

/**
 * A status line (success/error message) for an element that starts hidden.
 * Returns say(text, isError).
 */
export function statusLine(id: string): (text: string, isError?: boolean) => void {
    const el = maybeId(id);
    return (text, isError = false) => {
        if (!el) return;
        el.hidden = false;
        el.textContent = text;
        el.className = isError ? "auth-error" : "auth-notice";
    };
}
