// A placeholder book's editor (templates/placeholder.html, placeholders.go):
// its details, its cover, deleting it, and sending its files to the import
// queue. Admins only. Tags are chips and authors, series, publisher and tags
// are completed from the library, as in the metadata editor (metadata.ts).

import { createChipInput } from "./chips.js";
import { api, byId, errorMessage, maybeId, statusLine } from "./dom.js";
import { attachSuggest } from "./suggest.js";

interface Saved {
    uuid: string;
    url: string;
}

const say = statusLine("ph-status"); // the details form
const sayView = statusLine("ph-view-status"); // cover and formats
const form = byId("ph-form", HTMLFormElement);
const id = form.dataset["uuid"] ?? "";
const base = `/api/placeholders/${encodeURIComponent(id)}`;

/** Goes to a page the server named on this site. */
function go(url: string): void {
    if (url.startsWith("/") && !url.startsWith("//")) location.assign(url);
    else location.reload();
}

/** Sends a multipart form; the answer as JSON. */
async function send<T>(method: string, url: string, body: FormData): Promise<T> {
    const res = await fetch(url, { method, body, credentials: "same-origin" });
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    return (await res.json()) as T;
}

const input = (name: string): HTMLInputElement | null => form.querySelector<HTMLInputElement>(`input[name="${name}"]`);
const creators = input("creators");
if (creators) attachSuggest(creators, { category: "author", multi: true, separators: "&" });
const series = input("series");
if (series) attachSuggest(series, { category: "series" });
const publisher = input("publisher");
if (publisher) attachSuggest(publisher, { category: "publisher" });
const subjects = input("subjects");
const tags = subjects ? createChipInput(subjects, { placeholder: "Add tags (comma separated)…" }) : null;
// The chips take the field's starting value from an input event (chips.ts).
subjects?.dispatchEvent(new Event("input"));
if (tags)
    attachSuggest(tags.textInput, {
        category: "tag",
        multi: true,
        exclude: tags.activeValues,
        onPick: () => tags.commitPending(), // picking a suggestion adds it
    });

form.addEventListener("submit", (e) => {
    e.preventDefault();
    tags?.commitPending(); // what's half typed counts
    const f = new FormData(form);
    const field = (k: string): string => String(f.get(k) ?? "");
    const body = {
        title: field("title"),
        creators: field("creators"),
        series: field("series"),
        series_index: field("series_index"),
        publisher: field("publisher"),
        date: field("date"),
        subjects: field("subjects"),
        isbn: field("isbn"),
        hardcover: field("hardcover"),
        description: field("description"),
    };
    const btn = form.querySelector<HTMLButtonElement>('button[type="submit"]');
    if (btn) btn.disabled = true;
    say("Saving…");
    api<Saved>(id ? "PUT" : "POST", id ? base : "/api/placeholders", body)
        .then((res) => go(res.url))
        .catch((err: unknown) => {
            say(errorMessage(err), true);
            if (btn) btn.disabled = false;
        });
});

maybeId("ph-delete")?.addEventListener("click", () => {
    if (!confirm("Delete this placeholder? Nothing in the library changes.")) return;
    api("DELETE", base)
        .then(() => go("/missing"))
        .catch((err: unknown) => say(errorMessage(err), true));
});

const coverFile = maybeId("ph-cover-file", HTMLInputElement);
maybeId("ph-cover-pick")?.addEventListener("click", () => coverFile?.click());
coverFile?.addEventListener("change", () => {
    const file = coverFile.files?.[0];
    if (!file) return;
    const fd = new FormData();
    fd.append("image", file);
    sayView("Saving the picture…");
    send("POST", `${base}/cover`, fd)
        .then(() => location.reload())
        .catch((err: unknown) => sayView(errorMessage(err), true));
});
maybeId("ph-cover-remove")?.addEventListener("click", () => {
    api("DELETE", `${base}/cover`)
        .then(() => location.reload())
        .catch((err: unknown) => sayView(errorMessage(err), true));
});

const files = maybeId("ph-files", HTMLInputElement);
maybeId("ph-files-pick")?.addEventListener("click", () => files?.click());
files?.addEventListener("change", () => {
    const list = files.files;
    if (!list?.length) return;
    const fd = new FormData();
    for (const file of list) fd.append("files", file);
    sayView(`Uploading ${list.length === 1 ? "the file" : `${list.length} files`}…`);
    send<{ url: string }>("POST", `${base}/files`, fd)
        .then((res) => go(res.url))
        .catch((err: unknown) => sayView(errorMessage(err), true));
});
