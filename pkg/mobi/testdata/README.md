# Test fixtures

`tiny_source.epub` is a small hand-made EPUB with these parts:

- Three chapters, one long enough to span several text records.
- A nested TOC (Chapter Two has two sections).
- A link from chapter one to `#target` in chapter three.
- A PNG in a chapter and in the CSS, an inline SVG, a cover, and non-ASCII
  text.

The other files are Calibre's conversions of it:

```bash
ebook-convert tiny_source.epub tiny.azw3
ebook-convert tiny_source.epub tiny_mobi6.mobi --mobi-file-type old
ebook-convert tiny_source.epub tiny_joint.mobi --mobi-file-type both
```

On macOS, `ebook-convert` is in `/Applications/calibre.app/Contents/MacOS/`.
The fixtures were generated with Calibre 9.18.
