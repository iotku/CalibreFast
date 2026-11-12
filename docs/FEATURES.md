# Features in detail

Reference for how CalibreFast behaves, for people and AI tools working on the code. The [README](../README.md) has the short version.

## Browsing

- **The front page (`/`) is Discover**, the same layout for everyone:
  - **An introduction:** the library at a glance (books, authors, tags, languages), a search box and where to browse. For signed-out visitors (on a `-public` server) it adds a line on what an account is for, with Sign in, and Create an account when registration is open. **Hide** shrinks it to the counts line on that browser.
  - **Continue reading:** signed in, the books you're reading, most recently read (on any device) or marked first, with how far along and a button to carry on. Signed out, the books read **on this device**, newest first (they stay on the device; signing in doesn't bring them along yet). **See all** opens `/me/reading`. Once there's something here, the introduction folds away for a signed-in user.
  - **Want to read:** signed in, newest first; **See all** opens `/me/wanted`. Not shown signed out.
  - Signed in with nothing in a row, the row stays and says how to fill it, so the page looks the same for everyone.
  - **Shuffle:** about 12 books from the server's random order. Each load, and each click of **Shuffle**, shows the next 12, so the same books don't come back until the whole library has gone by; the order is new after each server restart. **Previous** goes back through the last 20 sets shown, also after a reload. Books you've finished are skipped (signed out: books read to the end on this device). The page doesn't link to `/shuffle` (the whole order on the grid): the row is meant as a handful at a time, though the address works if typed in.
  - **Recently added:** the 12 newest. **See all** opens `/latest`.
  - Rows scroll sideways; a book opens in the same popup as the grid.
- **Latest** (`/latest`, in the top bar) is every book, newest added first, on the grid. Old links to `/?page=N` go there.
- **The whole library is one continuous grid.** The scrollbar reflects the full library from the start, and you can drag straight to any point. Only the rows near the screen are in the page; the rest are drawn as empty cards, so even a scroll faster than the page can keep up shows the grid's shape, never a blank.
- **60 books per page**, everywhere: Latest, Shuffle, filtered views and search. A page is how the books are fetched and what the page box counts, not a block in the grid: books flow on from one page to the next with no gaps, at any number of columns.
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

- **Several authors** are written `A & B`, as Calibre does: the editor's field, and an uploaded file's creators, split on `&` only. Names are kept as written, and repeats are dropped.
- **Split** (under the authors field) is offered when the authors look written another way, and rewrites them only when clicked; **current** undoes it. Nothing is split by itself, since the guess can be wrong.
  - `;` separates authors: `Hailu, Gebreyesus; Negash, Ghirmai; Chrisman, Laura` → `Gebreyesus Hailu & Ghirmai Negash & Laura Chrisman`.
  - Without `;` or `&`, commas separate authors only if every part is a full name (`Alan Donovan, Brian Kernighan`). Otherwise the comma is inside one name (`Donovan, Alan`).
  - `Last, First` becomes `First Last` (**First name first**). Suffixes (`King, Jr.`) and names with two commas are left alone.

## Search

`/search?q=…` matches titles and authors, using the same grid and navigation as the library.

## Metadata editor

`/book/edit/<uuid>` (needs an admin and `-allow-writes`) writes both `metadata.db` and the book's `metadata.opf`.

**Preview Book** in the editor's top bar (and on a book waiting in the import queue) opens the book's EPUB, or else its PDF, in a new tab to look things up (the copyright page, an ISBN, the series). It's a **preview**: your place, bookmarks, highlights and reading status aren't saved or changed, no copy is kept for offline reading, and Back closes the tab (or split view, or window) rather than going back to the editor. In-book jumps in a preview add no history, since browsers only let a page close a tab with a single history entry.

### Nothing is lost by accident

- **Identifiers**
  - Values are kept even with no type (`opf:scheme`).
  - Their `id` attributes (`uuid_id`, `calibre_id`, …) are preserved.
  - `uuid_id`, which `package@unique-identifier` points at, always goes on the identifier holding the book's uuid from `metadata.db`, never on an ISBN or another typed identifier. If no identifier holds that uuid, a Calibre-style `<dc:identifier opf:scheme="uuid" id="uuid_id">` is added first, so Calibre can rebuild its database from the file. Only a second `uuid_id` (as left by older versions on an ISBN) loses its id; values never change.
  - `calibre` and `uuid` identifiers (Calibre's book id and uuid, also in its exports) and an EPUB's `urn:uuid:` package id aren't shown in the editor. Saving keeps the book's own in `metadata.opf` and `metadata.db` as they are.
  - Values containing commas or colons survive, because identifiers are sent as structured data rather than a string.
- **Removing things**
  - A removed identifier, tag or language is crossed out with **Undo** and moved to the end of its list until you apply. For tags and languages that's only the book's own: one added since (typed, or from a pill) just goes, so trying several sources' pills doesn't pile up crossed-out tags.
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
  - A field never holds the same value twice (ignoring case): adding one that's already there, or a source or lookup offering it again, keeps the field's spelling. The server drops repeats too when saving.
  - Click anywhere on a chip, except its ×, to edit it in place.
  - The source pills are above the box, so they stay put however many chips there are.
  - **Split ;** breaks tags that were glued together with semicolons. **Split all** does every such tag at once.
- **Autocomplete** for authors, publisher, tags and languages offers only values in your library; languages match by name too.
  - Nothing is preselected, so Enter adds exactly what you typed.
  - ↑/↓ highlight a suggestion; Enter, Tab or a click takes it.
- **Cover dialog:** clicking the cover (it's usually right already) opens every cover the book could have, large enough to compare quality, each with its size in pixels and bytes. A picture is saved as it is when it's a JPEG up to 2560px wide and 5 MB (no re-compression); anything else is converted to JPEG once, scaled down to 2560px wide if wider.
  - **Choices:** the current cover and the first 5 pages of the book's PDF and EPUB, loaded when the dialog opens. Also **Upload image…**, and **Look up cover online**: with an ISBN in the lookup box (the book's own, or one found in its files), it asks Open Library for that ISBN's cover, and it also shows the cover from a kept lookup.
  - **Comparing:** the current cover starts shown large. Click another to see the two side by side (a third replaces the older one; clicking one again hides it). An upload or an online cover is shown next to the current one. **Actual pixels** shows them pixel for pixel.
  - **Use this cover** saves it right away, without **Apply Changes**. **Revert** brings back the previous one.
- **Apply Changes** saves and opens the book page. It's off until you change a field (what the editor fills in or tidies when it loads doesn't count), and off again if you undo your changes.

### Finding ISBNs and looking books up

Both only offer choices. Nothing is saved until **Apply Changes**.

- **ISBNs in the book's files** are listed under Identifiers. For a book without an ISBN they're looked for when the editor opens; otherwise **Find ISBNs in files** looks. **↻ Refresh**, next to the list, looks again.
  - **EPUB:** the package's identifiers, then the text of the first 5 and last 3 chapters, where copyright pages are. An EPUB 3 `dc:source` ISBN is the print edition.
  - **MOBI/AZW3:** the ISBN in the file's EXTH header.
  - **PDF:** the XMP metadata, then the text of the first 5 and last 3 pages, read in the browser with pdf.js. Only those pages are downloaded.
  - **Checked:** ISBN-10 and ISBN-13 check digits, so phone numbers and typos are skipped. A 10-digit number needs "ISBN" shortly before it. Every ISBN is shown as ISBN-13, once.
  - **Ranked:** copyright pages list the print editions too, so ISBNs labelled ebook/e-book/EPUB/PDF/digital come first. Next are those from the files' metadata, then other ISBNs in the text, then those labelled hardcover, paperback, print or audio.
  - **Use** puts one in the ISBN field. If the book had a different ISBN, it's crossed out with **Undo**. ISBNs tried in between aren't kept, and a value is only ever crossed out once.
  - **An unlabeled identifier that's an ISBN** (one with no scheme, a common mix-up) moves into the ISBN field when that's empty and the value is one of the ISBNs found in the files. The search says so, and it's saved like any change.
- **The lookup bar** at the top of the editor searches Open Library two ways:
  - **Search ISBN** searches by the ISBN in the bar's box: that one edition, the likeliest match. The box shows the form's ISBN and follows it; when the book has none, type it there. A valid ISBN (checked before anything is sent) also goes into the ISBN field, crossing out a different one with **Undo**.
  - **Search title & author** searches by the form's title and first author: up to 5 books, best first.
  - **The likely match is ticked for you:** the only one a search finds, or, in a title search, the books with an edition of the form's ISBN (the server compares the ISBN with Open Library's edition lists; it isn't sent). Untick it if it's the wrong book.
  - The ISBN field's search button, and each ISBN found in the files, search by that ISBN.
  - **Matches** are listed with their authors, year, publisher, language and identifiers. **Tick** the right ones: each becomes a source under every field (**Open Library**, **Open Library 2**, …), so their values can be compared and picked one by one. Matches with the same value for a field share one pill there (**Open Library +2**; hover for which). Unticking one takes it away again. Ticked matches stay at the top of the list through a new search.
  - **Compare** shows one match against the current metadata. **Pick fields in the form** ticks it; **Accept all and apply** takes every value it has (never the cover), saves, and opens the book page.
  - **Try Google Books** asks Google for the same book, as one more match to tick. It's offered only when the server has a Google Books API key (`CF_GOOGLE_BOOKS_KEY`), and not with `-no-google-books`.
  - A match's identifier pill adds identifiers (ISBN, `olid`, `google`) without crossing anything out.
  - Open Library's cover, when it has one, is fetched only when you click **Look up cover online** in the cover dialog.
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
- Metadata is read from EPUB and PDF files. Afterwards the editor lets you pick each field's value from each format. `metadata.opf` gets the book's uuid as `<dc:identifier opf:scheme="uuid" id="uuid_id">`; the file's own identifiers (an EPUB's `urn:uuid:`, its ISBN) are kept with their ids.

## Duplicates

Uploads and the import queue check the library before adding a book:
- **Same title and authors** (case and spacing aside): a likely duplicate. It isn't added; **Upload anyway** (upload page) or **Add anyway** (import review) adds it.
- **Same title only:** a duplicate only if the library book holds a file identical to one being added (SHA-256). Otherwise it's a different book, added as usual.
- **An identical file** is never added again, override or not.
- The import queue marks books that look already in the library, so you can discard them without opening them. The review page and the library's metadata editor say so at the top, linking the matching books. On review, the check is run again with your edited title and authors when you add the book.

## Merging books

The same book added twice (its EPUB once, its PDF another time, or two copies with different metadata) can be made one book, with every format (`-allow-writes`, admins).

- **Choosing the books:** **Select** in the library's top bar, tick them, **Merge N…** in the bar; or in the bulk editor, tick them and **Merge…**, or **Merge N** on a group of look-alikes. Up to 20 at a time.
- **The merge page** is the metadata editor for the merged book. Each book has a letter (A, B, C…):
  - **The record to keep:** its uuid and link stay, so readers' places and shelves do too. The first book chosen, unless you pick another (the page reloads with its metadata as **current**).
  - **Metadata:** pills under each field are **current** (the kept book's metadata), **B**, **C**… (the others'), and **A epub**, **B pdf**… (what each book's files say). Pick each field as in the editor; the ISBN tools and online lookup work too.
  - **Cover:** any of the books' covers.
  - **Files to keep:** every format any of them has. Where two books have the same format, pick whose file (the kept book's by default; sizes shown). The files are compared (SHA-256): **identical files** when it makes no difference which is kept; otherwise **files differ**, with **Download** on each distinct file to compare them yourself (a file identical to an earlier book's says `= A` instead).
- **Merge N books** does it:
  - the chosen files and cover are copied into the kept book's folder; a file of its own that one replaces goes to the Recycling Bin batch (`replaced/`), and its old cover becomes the cover to revert to;
  - the other books go to the Recycling Bin whole, as one batch marked **Merged** that names the kept book. Restoring one brings it back as a separate book; the merged book keeps the files it took;
  - readers' favorites, reading status, places, bookmarks, highlights and shelves for the other books are copied to the kept book (the newest status and place win). The originals stay with the old books for a restore;
  - the metadata is saved as the editor saves it.
- Afterwards the merged book opens.

## Bulk metadata editor

`/library/edit` (**Bulk edit** in the user menu; admins, `-allow-writes`): every book as a row of a table, to find duplicates and fix metadata across many books.

- **Columns:** title, authors, series, publisher, published, languages, tags, formats, added. Click a heading to sort; click again to reverse.
- **Formats** are the files `metadata.db` lists. A book it lists none for shows the book files in its folder instead (as its page and downloads find them), dashed: Calibre itself doesn't show those. `folder` in the Formats box finds such books. Merging takes their files too, and gives them `metadata.db` rows.
  - While there are any, the toolbar says how many, with **Show** and **Add to metadata.db**. That adds a row for each format whose file is the only one of its kind in the book's folder, named as the file is, so Calibre lists it too. Only the database is modified, and only on a server started with `-allow-writes`. A format with several files in the folder is left alone and named in the result.
- **Filtering** happens in the browser, as you type:
  - the search box matches every column (each word somewhere); the ✕ in it, and in each column's box, clears it;
  - each column's box filters that column: `word` contains it (any case or accent; in tags, languages and formats, any one of them does), `=Fiction` is exactly that, a lone `=` is empty, `!word` doesn't contain it (`!=`: not empty), `sci|fant` either, `<2000` and `>=2010-06` compare dates. The tick box beside each column's name shows only books with nothing in that column, or the default a book gets when nothing was known (title or author `Unknown`, language `und`). Tags, languages, formats, publisher and series boxes suggest the library's values with how many books have each;
  - clicking a tag, language or format in a row filters by it;
  - **Duplicates** shows only books that look like another, grouped: **Similar title** (case, accents, punctuation, a leading "The/A/An" and anything in brackets aside), **Similar title & authors**, or **Same ISBN**. Each group has a **Merge N** link (middle-click or Ctrl/⌘-click opens it in a new tab; the table reloads its rows when you come back to it) on its first row and a **Not Dupe** button on its second (the Edit, Merge and Not Dupe buttons are in the first column, before the tick box and the title, so they're reachable without scrolling sideways);
  - **Not Dupe** marks the group's books as different books: the group no longer shows in any duplicates view. If another look-alike joins it later (a new upload), it shows again. **Duplicates → Marked not duplicates** lists every marked group, with **Unmark** to put it back. Markings are kept in CalibreFast's own database, not the library;
  - **Only selected** shows the ticked books. The filters, sort and duplicates mode are kept in the address, so Back returns to the same view.
- **Many tags or languages:** a cell shows what fits (the first one cut short only if it alone is too wide) and a **+N** button for the rest, which opens them all over the rows below; click a chip there to filter by it, ✎ to edit. Escape or a click elsewhere closes it.
- **Selecting:** tick rows (Shift-click ticks a range), or the heading's box for every book shown. **Edit in table** in the library's selection bar opens the table with those books ticked.
- **Editing one book in place:** double-click its title, authors, publisher, date, tags or languages (clicking a tag or language chip filters instead, so those cells have a ✎ button before the chips). Enter or leaving the box saves; Escape cancels.
- **Editing the selected books** (**Edit…**):
  - **Tags** and **Languages:** the ones the selected books have, with how many have each. Click to remove from all of them; **Add** adds to all; **Replace all with** sets them. Removing `en` removes `eng` too.
  - **Authors**, **Publisher**, **Published:** set for every book (an empty publisher or date clears it).
  - Each book's `metadata.opf` is changed and saved as the metadata editor saves it, with the rest of it as it was. A book the change makes no difference to isn't written. The result says how many changed.
- **Remove…** removes the selected books (the usual confirmation and Recycling Bin).

## Watch folder and import queue

Start the server with `-watch-dir /path/to/folder` (and `-allow-writes`). Books put in that folder are **not** added to the library: they wait for an admin's review.

- **What is taken:** EPUB, PDF, MOBI, AZW3 and DjVu files, in the folder and its subfolders.
  - The operating system tells CalibreFast when something there changes, so an idle folder costs nothing and a new book shows up within seconds. Where it can't (some network and container mounts, or too many folders for the system's limit), the folder is checked every 30 seconds instead; the log and the queue page say so.
  - Some mounts (NFS, SMB, some container bind mounts) accept being watched but never report anything, so books put there aren't noticed. Start the server with `-watch-dir-force-polling` to check such a folder every 30 seconds regardless.
  - A file is taken once it has been unchanged for 5 seconds, so a copy in progress is left alone.
  - Files in one folder with the same name and different formats (`Book.epub` + `Book.pdf`) are one book.
  - Hidden files and folders (`.name`), symbolic links and empty files are ignored. Other files are left where they are.
- **Taken files are moved** out of the watch folder into the queue (in `-datadir`), so the folder shows what is still to come.
- **The queue** (`/import`, **Review imports** in the user menu, and an inbox icon with a count in the top bar while books wait) lists each book with its cover and files. It updates by itself.
- **Covers are taken from the files** when a book arrives: the cover its EPUB declares, else its PDF's first page when that page is a single picture (a scan, or a cover image). Books already waiting get one when the server starts.
- **Reviewing a book** uses the metadata editor, on the waiting book:
  - **current** is what the files say (the best of each format, as on upload); each file's own values are pills under the fields. A file that says nothing gets its file name as the title.
  - **ISBNs** are looked for in the files right away, and the lookup bar fetches metadata from Open Library (or Google Books, with an API key) to compare and pick from, as in the editor. **Accept all** fills the form; it doesn't add the book.
  - **The cover:** click it for the cover dialog (pages of the PDF and EPUB, an upload, Open Library's cover). A book that already has a cover isn't opened in the browser until you click it. One with no cover yet (an EPUB without a declared cover, a PDF whose first page is text) gets its EPUB's first pictures or its PDF's first page, drawn in the browser, until you choose another.
  - **Add to library** adds the book with the form's values, the chosen cover and all its files, then opens the next book in the queue (or the added book, after the last one).
  - **Discard** deletes the book's files from the queue, after a confirmation. They are not put back in the watch folder.
- **Nothing reaches `metadata.db` or the library folder before Add to library.** Leaving the page keeps the book waiting; the form's edits aren't saved until then (the cover is).
- Waiting books survive a restart, and can still be reviewed when the server runs without `-watch-dir`.

## Library repairs

`/admin/repairs` (**Library repairs** in the user menu; admins, `-allow-writes`): fixes for `metadata.db` that a library shouldn't need, for books added by older versions of CalibreFast or changed outside Calibre. Nothing runs by itself, at startup or otherwise: **Check the library** looks at every book's files (slow on a big library), lists what each repair would change (both are always listed, with "Nothing to repair." when there's nothing), and each has its own Repair button. Only the database is modified; no file is renamed, moved or removed.

- **File names Calibre can't find**: a format whose `data.name` isn't its file's name. The repair points it at the one file in the book's folder with that format's extension; a format with no such file, or several, is left alone.
- **Books with files metadata.db doesn't list**: a book with no formats in `metadata.db` whose folder holds book files. The repair adds a row for each format with exactly one file. The bulk editor shows and repairs these too.

## Removing books and the Recycling Bin

Admins on a server with `-allow-writes` can remove books from the library. Nothing is deleted: removed books go to the Recycling Bin, and only the server's owner can empty it.

- **Removing one book:** **Remove from library…** under **Edit metadata**, on the book's page or popup.
- **Removing several:** **Select** in the top bar of the library, a filtered view or search results (or, on a touch screen, a long press on a book, which selects it too), then tap the books (a tick shows on each, and the choice survives scrolling) and **Remove N from library…** in the bar at the bottom. **Done** or Escape leaves selection. The same bar offers **Merge N…** ([Merging books](#merging-books)) and **Edit in table** ([Bulk metadata editor](#bulk-metadata-editor)); the bulk editor can remove books too.
- **Hard to do by accident:** the confirmation lists every book, explains what happens, and its button only works once `delete` is typed. Enter cancels until then.
- **Undo:** the confirmation's result offers **Undo**, which puts everything straight back.
- **What a removal does:** it's one batch in the bin (`-recycle-bin`, by default `<datadir>/recycle-bin/<batch>/`), made before anything changes:
  - `metadata.db`: a copy of the library's database just before the removal;
  - `books/<id>/`: each book's folder, moved there whole (files, `metadata.opf`, cover);
  - `batch.json`: when, by whom, and each book's title, authors and place in the library;
  - `reader-data.json`: every reader's bookmarks, highlights and notes, reading positions, reading status, favorites and shelf entries for those books.

  Then the books' rows go from `metadata.db` (with the authors, publishers, series and tags no other book has, as Calibre does), and their folders (and author folders left empty) from the library. If any step fails, nothing is removed. Readers' data stays in CalibreFast's database too (removing a book doesn't touch it), so it's there again when the book comes back; a restore also adds back from `reader-data.json` any of it that's gone since, without overwriting anything newer. The bin page shows how much each book has.
- **Restoring** (`/recycle-bin`, **Recycling Bin** in the user menu): every batch, newest first, with **Restore** per book and **Restore all**. A book comes back with its id, uuid and folder, its rows copied from the batch's `metadata.db`. Authors, tags and so on are matched by name, and added back if the library no longer has them. Whatever else changed in the library since is kept. A book whose folder is back in the library already, or whose files were emptied from the bin, isn't restored.
- **Emptying** is only possible on the server, so a stolen admin session can't remove the backups too: `CalibreFast -datadir … -empty-recycle-bin 30d` deletes batches removed more than 30 days ago (`0`: all), or delete batch folders by hand.
- **Rolling the whole library back** by hand (e.g. after many removals): stop the server, copy a batch's `metadata.db` over the library's, and move each `books/<id>/` folder back to its `path` from `batch.json`. Changes made after that batch are lost, so restoring from the page is usually better.

## Accounts, favorites, progress and shelves

- **Accounts:** password, or GitHub/Google sign-in. Admins manage users at `/admin/users`. See [SECURITY.md](SECURITY.md) for who can do what.
- **One-time links** (`/l/<token>`), created on `/admin/users`:
  - a **sign-in link** for an existing user
  - an **invite** to create an account, optionally as an admin, even with registration closed

  They work once, expire after 1 hour / 1 day / 7 days, can be revoked, and aren't used up by chat link previews.
- **Favorites and status:** the book popup has ★ Favorite and a status menu (not reading / want to read / reading / finished). Lists are at `/me/favorites`, `/me/wanted`, `/me/reading` and `/me/finished`.
  - Turning a page in a reader marks a book as reading; reaching ≥ 97% marks it finished. Starting a book you wanted takes it off Want to read.
  - **On the covers** (with a mouse): hovering a book in the grid shows ☆ Favorite in the top left, and in the top right a button that depends on the book's status: not reading, a bookmark that adds it to (or removes it from) Want to read; reading, ▶ to open the reader in the format you last read; finished, a check mark. On touch screens use the popup.
  - In the grid, books you're reading have a ring and a bar along the bottom showing how far along you are.
  - The popup shows where you are and when it was saved, e.g. "Page 5 of 25 · synced 2 minutes ago from Kobo Clara" (EPUBs show a percentage). The newest position wins, from the web readers or a KOReader device.
  - Automatic changes only move forward and never override a status you set yourself.
- **Readers** (EPUB and PDF) share their controls:
  - **Bars.** A top bar (back, title and chapter, bookmark, contents, settings) and a bottom bar (previous/next, a scrubber, the position). A tap or click in the middle of the page hides and shows them; turning a page from the page hides them. Escape does the same, and Tab brings them back for the keyboard. While they're hidden the position stays in small print at the bottom.
  - **Turning pages:** tap or click the left or right third, swipe (EPUB), the wheel, ←/→, PageUp/PageDown, Space/Shift+Space. Right-to-left EPUBs turn the other way.
  - **Themes:** light, sepia, dark and black, or the system's light/dark setting (the default). Dark themes recolour an EPUB's text and backgrounds and leave its images alone. PDF pages keep their own colours in every theme: recolouring them would spoil photos and scans.
  - **Sidebar:** Contents (the current chapter is marked), Bookmarks, and for EPUBs Search.
  - **Settings are per device** (a phone and a desktop want different sizes).
- **EPUB settings:** font (the book's own, serif, sans-serif, monospace: fonts already on the device, nothing is downloaded), size, line spacing, margins, alignment, hyphenation, and pages or continuous scrolling within a chapter.
- **EPUB search** (the magnifier, or Ctrl/⌘+F) goes through the book a chapter at a time and lists matches with their chapter and page. Choosing one goes there and highlights it; Back returns.
- **PDF reader:**
  - **Sharp at any zoom,** on any screen: pages are drawn at the screen's own resolution and drawn again whenever the zoom changes.
  - **Zoom:** pinch (it zooms around your fingers), double-tap or double-click to zoom in on a spot and again to fit, Ctrl/⌘+wheel or a trackpad pinch, `+`/`-`/`0`, and Fit page, Fit width and 100% in the settings. Zoom goes from half the fitted size to 1000%. The fit is remembered; zooming by hand lasts until you leave the book.
  - **Moving a zoomed page:** drag with a finger, scroll, or drag with the mouse anywhere that isn't text.
  - **Turning pages:** as above, plus swipe. While a page is zoomed wider than the window, swipes and taps move and don't turn. A taller page (Fit width) scrolls with the wheel, Space and PageDown, and turns at its end. Home/End go to the first and last page.
  - **Magnifier** (mouse only; the button in the top bar, or `M`): a lens follows the pointer and shows what's under it, drawn from the PDF at 2.5× (the wheel changes that, from 1.5× to 8×), so a scan's small print is readable without zooming the page. Esc or `M` turns it off.
  - **Text** can be selected and copied. **Links** work: web links open in a new tab, links within the document go there and Back returns.
  - **Two pages side by side:** on when the reader opens in a window wider than tall, off in a taller one, and following the window as it turns until you tick or untick it yourself (which then holds for that visit, in either shape). **First page on its own** (on by default) keeps a cover from pushing every later pair off by one. **Right to left (manga)** puts the first page of a pair on the right and reverses what "forward" means for taps, swipes, the arrow keys, the bottom bar's buttons and the scrubber; it applies to single pages too.
  - **Contents** marks the current chapter; scanned and CJK PDFs get the fonts and character maps they need from the server.
- **Highlights and notes** (EPUB, and PDFs that have text):
  - Select text and a small toolbar offers six colours, **Note** (highlights and opens the note editor) and **Copy**.
  - Tap or click a highlight to change its colour, edit its note or delete it.
  - The sidebar's **Notes** tab lists them in book order, with chapter and page; choosing one goes there, and Back returns.
  - They stay on the same words whatever the font size, layout or zoom.
  - Kept in the browser and, when signed in, synced: an edit or delete on one device reaches the others, and the newest change wins. Not signed in, they stay on that device.
  - Scanned PDFs have no text to select, so they can't be highlighted yet.
  - **KOReader:** **Notes → KOReader** exports them as the annotations file KOReader imports, and imports the one KOReader exports ([KOREADER.md](KOREADER.md#highlights-and-notes)). Neither direction deletes anything.
- **EPUB reader opens quickly, then keeps the book:** it reads only what the first page needs, then fetches the rest of the book in the background. Page numbers and search work once the whole book has arrived.
  - **Offline reading:** the book is kept in the browser, so reopening it needs no download. On HTTPS (or localhost), a kept book also opens with no connection to the server; any other page then shows **Offline**, a list of the kept books.
  - A kept book is removed when it hasn't been opened for a week, when it's set back to **Not reading**, or when you sign out.
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
