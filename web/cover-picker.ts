// The upload page's cover picker: upload an image, or pick a page of the
// book's PDF or EPUB from a strip of thumbnails. A click saves it. With
// autoSelectDefault, the EPUB's cover (or the PDF's first page) is saved
// right away. The metadata editor uses cover-dialog.ts instead.

import { libraryBook } from "./book-target.js";
import { el, errorMessage } from "./dom.js";
import { type CoverChoice, epubPages, fileChoice, pdfPages } from "./cover-sources.js";

/** Called after a cover is saved for a book. */
type CoverSaved = ((uuid: string) => void) | null;

export interface CoverPickerOptions {
    autoSelectDefault?: boolean;
}

export function buildCoverPicker(
    uuid: string,
    formats: string[] = [],
    onCoverSaved: CoverSaved = null,
    options: CoverPickerOptions = {},
): HTMLDivElement {
    const book = libraryBook(uuid);
    const picker = document.createElement("div");
    picker.className = "cover-picker";
    const done = el("span", { className: "cover-picker-done" }, "✓ Cover updated");
    const actions = el("div", { className: "cover-picker-actions" });
    const strip = el("div", { className: "cover-pages-strip" });
    const status = el("div", { className: "cover-pages-status" });
    const pages = el("div", { className: "cover-pages" }, strip, status);
    picker.append(el("div", { className: "cover-picker-header" }, el("h3", {}, "Cover Image"), done), actions, pages);

    const save = async (choice: CoverChoice, thumb: HTMLElement | null, isDefault = false): Promise<void> => {
        if (!choice.save) return;
        strip.querySelectorAll(".selected").forEach((t) => t.classList.remove("selected"));
        thumb?.classList.add("selected");
        status.textContent = isDefault ? `Saving the default cover (${choice.label})…` : "Saving cover…";
        try {
            await choice.save();
            done.textContent = isDefault ? "✓ Default cover saved" : "✓ Cover saved";
            done.classList.add("visible");
            status.textContent = isDefault ? `✓ Saved ${choice.label} as the cover (click another to change)` : "Cover saved!";
            onCoverSaved?.(uuid);
        } catch (err: unknown) {
            status.textContent = "Failed to save: " + errorMessage(err);
        }
    };

    const thumb = (choice: CoverChoice): HTMLImageElement => {
        const img = document.createElement("img");
        img.src = choice.src;
        img.alt = choice.label;
        img.title = `${choice.label} · ${choice.width} × ${choice.height} px`;
        img.addEventListener("click", () => void save(choice, img));
        strip.appendChild(img);
        return img;
    };

    // Upload: saved as soon as it's picked.
    const uploadLabel = document.createElement("label");
    uploadLabel.textContent = "Upload image (JPG/PNG)";
    const fileIn = document.createElement("input");
    fileIn.type = "file";
    fileIn.accept = ".jpg,.jpeg,.png";
    uploadLabel.appendChild(fileIn);
    actions.appendChild(uploadLabel);
    fileIn.addEventListener("change", async () => {
        const file = fileIn.files?.[0];
        if (!file) return;
        pages.classList.add("visible");
        try {
            await save(await fileChoice(book, file), null);
        } catch (err: unknown) {
            status.textContent = "Upload failed: " + errorMessage(err);
        } finally {
            fileIn.value = "";
        }
    });

    /** Shows a source's pages in the strip; its button again hides them. */
    const show = async (btn: HTMLButtonElement, source: AsyncGenerator<CoverChoice>, autoSave = false): Promise<void> => {
        const allBtns = actions.querySelectorAll("button");
        if (!autoSave && btn.classList.contains("active")) {
            allBtns.forEach((b) => b.classList.remove("active"));
            pages.classList.remove("visible");
            return;
        }
        allBtns.forEach((b) => b.classList.remove("active"));
        btn.classList.add("active");
        strip.replaceChildren();
        status.textContent = "Loading pages…";
        pages.classList.add("visible");
        let n = 0;
        try {
            for await (const choice of source) {
                const img = thumb(choice);
                if (autoSave && n === 0) void save(choice, img, true);
                n++;
            }
            if (!autoSave) status.textContent = n ? "Click a page to use it as the cover" : "No pages to show.";
        } catch (err: unknown) {
            status.textContent = "Failed to load pages: " + errorMessage(err);
        }
    };

    const sourceButton = (text: string, source: () => AsyncGenerator<CoverChoice>): HTMLButtonElement => {
        const btn = document.createElement("button");
        btn.type = "button";
        btn.textContent = text;
        btn.addEventListener("click", () => void show(btn, source()));
        actions.appendChild(btn);
        return btn;
    };
    const pdfBtn = formats.includes("pdf") ? sourceButton("Pick from PDF pages", () => pdfPages(book)) : null;
    const epubBtn = formats.includes("epub") ? sourceButton("Pick from EPUB pages", () => epubPages(book)) : null;

    // On upload: the EPUB's cover, else the PDF's first page.
    if (options.autoSelectDefault) {
        if (epubBtn) void show(epubBtn, epubPages(book), true);
        else if (pdfBtn) void show(pdfBtn, pdfPages(book), true);
    }

    return picker;
}
