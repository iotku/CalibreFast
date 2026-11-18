// htmx under Trusted Types (trusted.ts, csp.go).
//
// htmx 4 hands every response to policy.createHTML before parsing it, every
// <script> in swapped markup to policy.createScript, and hx-on handlers to a
// Function constructor; an extension's init can replace all three
// (initSecurity). Ours: responses (our server's, from html/template) go
// through the "cf-htmx" policy, swapped scripts are dropped, and hx-on and
// other attribute JavaScript throw. CalibreFast uses none of those: page
// code lives in the bundles.
//
// Imported for its side effect by modal.ts, which every htmx page loads.
// Module scripts run after htmx's classic <script> and before it starts
// (DOMContentLoaded), so this is in place before the first swap.
import { htmxPolicy } from "./trusted.js";

function noInlineJs(): never {
    throw new Error("htmx: hx-on and other inline JavaScript are disabled (Trusted Types)");
}

if (typeof htmx !== "undefined") {
    htmx.registerExtension("cf-trusted-types", {
        init(api) {
            api.initSecurity(htmxPolicy(), noInlineJs, noInlineJs);
        },
    });
}
