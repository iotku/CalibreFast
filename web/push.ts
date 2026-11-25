/**
 * System notifications on this device (webpush.go, sw.ts): turning them on
 * and off, and whether to offer them.
 *
 * Off unless the user turns them on: a push goes through the browser maker's
 * push service, a third party. It carries nothing (the service worker fetches
 * the text from this server), but the service still learns that something
 * happened.
 *
 * The user's "no" stands:
 *   - the browser's permission prompt is only ever shown when they press
 *     "Turn on" (never on its own), and never again once the browser says
 *     notifications are blocked (only its own settings can change that);
 *   - "No thanks" in the menu offer is kept on the account (push_declined),
 *     so no device offers again. Preferences can still turn them on.
 * Notifications always stay in the app (the menu and /me/notifications),
 * whatever is chosen here.
 */
import { api } from "./dom.js";
import { accountPrefs, setAccountPrefs } from "./prefs.js";

export type PushState =
    /** This browser can't (no service worker or Push API; iOS outside the installed app). */
    | "unsupported"
    /** -no-web-push on the server. */
    | "server-off"
    /** The browser's own setting blocks them: only it can change that. */
    | "blocked"
    | "on"
    | "off";

interface PushInfo {
    enabled: boolean;
    key?: string;
}

let info: Promise<PushInfo> | null = null;
function pushInfo(): Promise<PushInfo> {
    info ??= api<PushInfo>("GET", "/api/me/push").catch(() => ({ enabled: false }));
    return info;
}

export function pushSupported(): boolean {
    return "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
}

/** Opened as the installed app (home screen, dock), not in a browser tab. */
export function installedApp(): boolean {
    return matchMedia("(display-mode: standalone)").matches || (navigator as { standalone?: boolean }).standalone === true;
}

async function registration(): Promise<ServiceWorkerRegistration | null> {
    if (!pushSupported()) return null;
    return (await navigator.serviceWorker.getRegistration("/")) ?? null;
}

async function currentSub(): Promise<PushSubscription | null> {
    return (await (await registration())?.pushManager.getSubscription()) ?? null;
}

const b64url = (buf: ArrayBuffer | null): string =>
    buf ? btoa(String.fromCharCode(...new Uint8Array(buf))).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "") : "";

export async function pushState(): Promise<PushState> {
    if (!pushSupported()) return "unsupported";
    const i = await pushInfo();
    if (!i.enabled) return "server-off";
    if (Notification.permission === "denied") return "blocked";
    const sub = await currentSub();
    if (!sub || Notification.permission !== "granted") return "off";
    // Subscribed with a key the server no longer has (it made a new one):
    // the server dropped it, so subscribe again quietly (the user said yes).
    if (b64url(sub.options.applicationServerKey) !== i.key) {
        try {
            await sub.unsubscribe();
            await subscribe(i);
        } catch {
            return "off";
        }
    } else {
        // Make sure the server has it (it drops endpoints that failed for long).
        await api("PUT", "/api/me/push", { endpoint: sub.endpoint }).catch(() => undefined);
    }
    return "on";
}

async function subscribe(i: PushInfo): Promise<void> {
    const reg = await registration();
    if (!reg || !i.key) throw new Error("System notifications aren't available here.");
    const sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: i.key });
    await api("PUT", "/api/me/push", { endpoint: sub.endpoint });
}

/**
 * Turn them on for this device. Call only from a click: it may show the
 * browser's permission prompt. Resolves to the state after.
 */
export async function turnOnPush(): Promise<PushState> {
    if (!pushSupported()) return "unsupported";
    const i = await pushInfo();
    if (!i.enabled) return "server-off";
    if (Notification.permission === "denied") return "blocked";
    const perm = Notification.permission === "granted" ? "granted" : await Notification.requestPermission();
    if (perm === "denied") return "blocked";
    if (perm !== "granted") return "off"; // the prompt was closed: nothing chosen
    await subscribe(i);
    return "on";
}

/** Turn them off for this device (also on signing out). */
export async function turnOffPush(): Promise<void> {
    const sub = await currentSub();
    if (!sub) return;
    const endpoint = sub.endpoint;
    await sub.unsubscribe().catch(() => undefined);
    await api("DELETE", "/api/me/push", { endpoint }).catch(() => undefined);
}

/** Whether to offer them in the notifications menu: the installed app, never asked or answered. */
export async function shouldOfferPush(): Promise<boolean> {
    if (!installedApp() || !pushSupported() || accountPrefs().push_declined) return false;
    if (Notification.permission !== "default") return false;
    return (await pushState()) === "off";
}

/** "No thanks": never offered again (on any device). */
export function declinePush(): Promise<void> {
    return setAccountPrefs({ push_declined: true });
}
