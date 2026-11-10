# Features in detail

Reference for how CalibreFast behaves, for people and AI tools working on the code. The [README](../README.md) has the short version.

## Browsing

- **The whole library is one continuous grid.** The scrollbar reflects the full library from the start, and you can drag straight to any point. Only the rows near the screen are in the page; the rest are drawn as empty cards, so even a scroll faster than the page can keep up shows the grid's shape, never a blank.
- **60 books per page**, everywhere: the main library, filtered views and search. A page is how the books are fetched and what the page box counts, not a block in the grid: books flow on from one page to the next with no gaps, at any number of columns.
- **Navigation:**
  - a page number box with prev/next in the top bar; going to page N puts the row with its first book at the top
  - Home/End move one page at a time
  - a floating `page / total` badge appears while dragging the scrollbar
  - the page at the top of the screen is kept in the URL (`?page=N`), so links work; reload and back/forward return to the exact row, even after the window has changed width
  - resizing keeps the same book at the top
- **Covers:**
  - load nearest-first in the scroll direction, with a limited number in flight so they don't crowd out page requests
  - requests for covers scrolled past are cancelled
  - books without a cover get a generated placeholder
- **Fast scrolling.** Book data is fetched further ahead the faster you scroll, so it's there before the screen is. Only when pages go by faster than they can be fetched (a scrollbar drag) does it wait until scrolling settles. Covers wait whenever you're moving faster than about 4 screens a second.

## Book details

- **Popup.** Clicking a book opens a details popup (full-screen on phones) with a shareable `#book/<uuid>` URL. There's also a permalink page at `/book/get/<uuid>`. Details are preloaded on hover.
- **Actions.** Read and download buttons sit under the cover, for every format on disk (EPUB, PDF, MOBI, AZW3, DJVU).
- **Links.** Authors, publisher, tags and languages link to their filtered views.
- **Language names.** Codes are shown by name, covering about 120 languages and all their common 2/3-letter codes. Examples: `eng`/`en` → English, `pt-BR` → Portuguese (BR). A group name, if you've set one, takes precedence. Books with several languages show all of them.
- **Descriptions** are rendered as HTML and cleaned with the same rules as the editor (see [Description editor](#description-editor)).

## Authors, publishers, tags and languages

- **Overview pages** (`/authors`, `/publishers`, `/tags`, `/languages`) show a tag cloud sized by book count.
- **Filtered views** (`/author/…`, `/publisher/…`, `/tag/…`, `/lang/…`) use the same grid as the main library.

### Groups

Groups combine values that mean the same thing without editing any books. For example, `en`, `eng` and `en-US` grouped as "English".

- **Same books from every name.** Every member, and the group name itself, opens the same combined list: `/lang/en`, `/lang/eng` and `/lang/English` all match. Matching ignores case, like Calibre.
- **One entry on overview pages.** A group shows once, and a book carrying several members is counted once.
- **Exclusive membership.** A value belongs to at most one group; adding it to another moves it.
- **Unique names.** Two groups can't share a name.
- **Removing a group.** Removing every member, or leaving one member with no new name, removes the group.
- **Editing.** Groups are edited on any filtered page with **Group…** (needs an admin and `-allow-writes`).
  - Changes are staged locally; nothing is saved until **Save grouping**.
  - Suggestions only offer values that exist in the library.
- **Storage.** Groups live in `<library>/calibrefast-groups.json`.
  - Writes are atomic (a temp file, then a rename).
  - The previous version is kept as `.bak`.
  - If the file can't be read, grouping goes read-only rather than overwriting it.

## Authors

Authors are shown by name everywhere ("Alan A. A. Donovan & Brian W. Kernighan"): the grids, the book popup and the editor. The "Last, First" form is only used for sorting.

- **Reading a list of authors** (the editor's field, an uploaded file's creators):
  - `;` and `&` separate authors: `Hailu, Gebreyesus; Negash, Ghirmai; Chrisman, Laura` is three authors.
  - Without them, commas separate authors only if every part is a full name (`Alan Donovan, Brian Kernighan`). Otherwise the comma is inside one name (`Donovan, Alan`).
  - `Last, First` becomes `First Last`. Suffixes (`King, Jr.`) and names with two commas are left alone.
  - Repeats are dropped.
- **The editor** writes several authors as `A & B`, as Calibre does. When what you typed would be saved differently, a **Saved as:** line under the field shows how. **Compare changes** shows it too.

## Search

`/search?q=…` matches titles and authors, using the same grid and navigation as the library.

## Metadata editor

`/book/edit/<uuid>` (needs an admin and `-allow-writes`) writes both `metadata.db` and the book's `metadata.opf`.

### Nothing is lost by accident

- **Identifiers**
  - Values are kept even with no type (`opf:scheme`).
  - Their `id` attributes (`uuid_id`, `calibre_id`, …) are preserved.
  - Values containing commas or colons survive, because identifiers are sent as structured data rather than a string.
- **Removing things**
  - A removed identifier, tag or language is crossed out with **Undo** and moved to the end of its list until you apply.
  - Switching a field to another source crosses out values the new source doesn't have, rather than dropping them.
- **Languages.** A book can have several, kept in order (the first is primary). They're stored as multiple `<dc:language>` entries and ordered links in `metadata.db`.
- **Dates.** A stored date the date picker can't show is kept unless you pick a new one; the page shows a note about it.
- **Leaving the page with unapplied changes** triggers the browser's "Leave site?" prompt.
  - **Leave:** goes without saving.
  - **Cancel:** shows what would be lost and offers to apply it.
  - Half-typed text in the tags or languages box counts as unsaved.

### Editing tools

- **Source buttons** under each field switch its value between:
  - **current:** the existing metadata
  - each **uploaded format:** metadata read from the file
  - **edited:** your own changes. Clicking it also lists that field's changes inline.
- **Compare changes** lists every changed field before and after.
- **Tags and languages are edited as chips:**
  - Type in the box above the chips, then press Enter or **Add**. Commas separate several values; typing a comma doesn't add anything on its own.
  - Click anywhere on a chip, except its ×, to edit it in place.
  - **Split ;** breaks tags that were glued together with semicolons. **Split all** does every such tag at once.
- **Autocomplete** for authors, publisher, tags and languages offers only values in your library; languages match by name too.
  - Nothing is preselected, so Enter adds exactly what you typed.
  - ↑/↓ highlight a suggestion; Enter, Tab or a click takes it.
- **Cover picker:** opens when you click the cover at the top (it's usually right already). Upload an image, pick a page from the PDF/EPUB, or, after a lookup that found one, the cover Open Library has. Picking saves the cover right away; **Revert** brings back the previous one.
- **Apply Changes** saves and opens the book page.

### Finding ISBNs and looking books up

Both only offer choices. Nothing is saved until **Apply Changes**.

- **Find ISBNs in files** (under Identifiers) looks in the book's own files. It runs by itself when the book has no ISBN.
  - **EPUB:** the package's identifiers, then the text of the first 5 and last 3 chapters, where copyright pages are. An EPUB 3 `dc:source` ISBN is the print edition.
  - **MOBI/AZW3:** the ISBN in the file's EXTH header.
  - **PDF:** the XMP metadata, then the text of the first 5 and last 3 pages, read in the browser with pdf.js. Only those pages are downloaded.
  - **Checked:** ISBN-10 and ISBN-13 check digits, so phone numbers and typos are skipped. A 10-digit number needs "ISBN" shortly before it. Every ISBN is shown as ISBN-13, once.
  - **Ranked:** copyright pages list the print editions too, so ISBNs labelled ebook/e-book/EPUB/PDF/digital come first. Next are those from the files' metadata, then other ISBNs in the text, then those labelled hardcover, paperback, print or audio.
  - **Use** puts one in the ISBN field. If the book had a different ISBN, it's crossed out with **Undo**. ISBNs tried in between aren't kept, and a value is only ever crossed out once.
- **Look up** in the top bar (in place of the library search) looks the book up by ISBN. The box starts with the book's ISBN, or the best one found in its files. Without an ISBN, it uses the title and first author. Each found ISBN also has its own **Look up**.
  - **Open Library first.** Only if it doesn't know the ISBN is **Wikidata** asked. **Try Wikidata** and **Try Google Books** ask the others on request. Google Books often has the best description; `-no-google-books` hides it.
  - **A source that knows the book** opens a comparison with the current metadata. **Accept all and apply** takes every value it has (never the cover), saves, and opens the book page. **Pick fields in the form** closes it: the source is a pill under every field (**Open Library**, **Wikidata**, **Google Books**), so values can be picked one by one. **Compare …** reopens it.
  - Its identifier pill adds identifiers (ISBN, `olid`, `wikidata`, `google`) without crossing anything out.
  - Open Library's cover, when it has one, is fetched only when you click **Cover from Open Library** in the cover picker.
  - **Only on a click:** nothing is sent when the editor opens, on upload, or when an ISBN is found, and each click asks one site at a time. Answers are cached, each site has rate and daily limits, and `-no-lookups` turns lookups off.

### Description editor

- **Two tabs:** an editable formatted **Preview** and the raw **HTML**.
- **Pasting** (e.g. from a shop listing) is cleaned with the same rules the book page uses to display descriptions (bluemonday's `UGCPolicy`):
  - **Kept:** headings, paragraphs, bold/italic/underline, lists, links, tables and images.
  - **Removed:** inline styles, classes, scripts, event handlers and `javascript:` links.
  - Bold and italic done with CSS (`font-weight`, `font-style`) are converted to `<strong>`/`<em>` first, so that formatting survives.
- **HTML tab** edits stay as typed until you click **Clean HTML**.
- **Clear** removes inline formatting, links, lists and headings from the selection.
- Deleting all the text leaves an empty description, not the `<br>` browsers leave behind.

## Uploading

`/upload` (needs an admin and `-allow-writes`):
- Files with the same name and different formats (`Book.epub` + `Book.pdf`) become one book with both formats.
- Metadata is read from EPUB and PDF files. Afterwards the editor lets you pick each field's value from each format.

## Accounts, favorites, progress and shelves

- **Accounts:** password, or GitHub/Google sign-in. Admins manage users at `/admin/users`. See [SECURITY.md](SECURITY.md) for who can do what.
- **One-time links** (`/l/<token>`), created on `/admin/users`:
  - a **sign-in link** for an existing user
  - an **invite** to create an account, optionally as an admin, even with registration closed

  They work once, expire after 1 hour / 1 day / 7 days, can be revoked, and aren't used up by chat link previews.
- **Favorites and status:** the book popup has ★ Favorite and a status menu (not started / reading / finished). Lists are at `/me/favorites`, `/me/reading` and `/me/finished`.
  - Turning a page in a reader marks a book as reading; reaching ≥ 97% marks it finished.
  - Automatic changes only move forward and never override a status you set yourself.
- **EPUB reader loads the whole book up front** (with a progress label), then reads offline: chapters, images and fonts come from memory, with no requests at chapter boundaries.
- **Reading position and bookmarks:** kept in the browser and, when signed in, synced to the server, so another device can resume.
  - The newest position wins.
  - Changes made offline are pushed when the connection returns.
- **KOReader progress sync:** KOReader devices can use CalibreFast as their Progress sync server (`<server>/kosync`), signing in with a sync key from **Account → Device sync**. See [KOREADER.md](KOREADER.md).
  - Positions move between devices and the web readers, newest first. EPUB positions land on the same chapter, and usually the same paragraph.
  - Reading on a device marks a library book as reading or finished.
  - Books outside the library still sync between devices.
- **Bookshelves** (`/shelves`):
  - Personal shelves are private to their owner.
  - Global shelves are visible to everyone and edited by admins.
  - Books are added from the book popup's Shelves section.
