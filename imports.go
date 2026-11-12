package main

// The watch folder and its import queue.
//
// With -watch-dir, book files that appear in that folder (the operating
// system says when; see folderWatcher.run) are moved into
// <datadir>/imports/<id>/ and wait there: nothing is added to metadata.db or
// the library folder until an admin reviews the book at /import/<id>, which
// is the metadata editor (templates/book-edit.html) pointed at the waiting
// book instead of one in the library. There the admin fixes the metadata
// (each file's own values, ISBNs found in the files and online lookups are
// offered as usual), chooses the cover, and adds the book to the library or
// discards it.
//
// A waiting book is a folder: its files under their own names, item.json
// (what was read from them) and cover.jpg: taken from the files when they
// arrive if possible (import_covers.go), else chosen on the review page. It lives in
// -datadir, not the library, so Calibre never sees a half-imported book.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
)

var (
	// watchDir is -watch-dir: the folder watched for new books ("" for none).
	watchDir string
	// watchForcePolling is -watch-dir-force-polling: scan on a timer even
	// where the folder can be watched. Some mounts (NFS, SMB, some container
	// bind mounts) accept a watch and then never report anything.
	watchForcePolling bool
	// imports is the queue. It is opened even without -watch-dir, so books
	// left from an earlier run can still be reviewed.
	imports *importInbox
	// watcher scans watchDir; nil when nothing is watched.
	watcher *folderWatcher
)

const (
	// watchSettle is how long a file must stay the same (size and
	// modification time) before it's taken: how long a copy has to stall to
	// be mistaken for a finished one.
	watchSettle = 5 * time.Second
	// watchDebounce gathers a burst of filesystem events into one scan.
	watchDebounce = 500 * time.Millisecond
	// watchPollInterval is how often the folder is scanned where the
	// operating system can't report changes to it.
	watchPollInterval = 30 * time.Second
)

func watching() bool { return watcher != nil }

// importFile is one file of a waiting book (TS: ImportFile in web/types.ts).
type importFile struct {
	Name   string `json:"name"`
	Format string `json:"format"` // "epub", "pdf", …
	Size   int64  `json:"size"`
}

// pendingImport is a book waiting for review (item.json). It doesn't change
// once written: the admin's edits go straight into the library on approval.
type pendingImport struct {
	ID    string       `json:"id"`
	Added time.Time    `json:"added"`
	From  string       `json:"from"` // folder inside the watch folder it came from; "" for its top
	Files []importFile `json:"files"`
	// Merged is the starting metadata (mergeMeta of Sources), shown as the
	// editor's "current"; Sources is what each file says, by format.
	Merged  UploadedBookMeta    `json:"merged"`
	Sources []*UploadedBookMeta `json:"sources"`
}

// importInbox is the import queue: the folders under dir.
type importInbox struct {
	dir   string
	mu    sync.Mutex
	items map[string]*pendingImport

	dupMu sync.Mutex
	dups  map[string]cachedDuplicates // duplicates, by id
}

// openInbox opens (creating if needed) the queue in dir and loads the books
// waiting in it.
func openInbox(dir string) (*importInbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := &importInbox{dir: dir, items: map[string]*pendingImport{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !isSafeUUIDComponent(e.Name()) {
			continue
		}
		it, err := loadPendingImport(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			// Never deleted automatically: it may hold someone's only copy.
			log.Printf("imports: skipping %s: %v", filepath.Join(dir, e.Name()), err)
			continue
		}
		b.items[it.ID] = it
	}
	return b, nil
}

func loadPendingImport(dir, id string) (*pendingImport, error) {
	data, err := os.ReadFile(filepath.Join(dir, "item.json"))
	if err != nil {
		return nil, err
	}
	var it pendingImport
	if err := json.Unmarshal(data, &it); err != nil {
		return nil, err
	}
	if it.ID != id || len(it.Files) == 0 {
		return nil, errors.New("item.json doesn't describe this folder")
	}
	for _, f := range it.Files {
		if f.Name != filepath.Base(f.Name) {
			return nil, fmt.Errorf("bad file name %q", f.Name)
		}
		if _, err := os.Stat(filepath.Join(dir, f.Name)); err != nil {
			return nil, err
		}
	}
	return &it, nil
}

func (b *importInbox) itemDir(id string) string { return filepath.Join(b.dir, id) }

// count is how many books are waiting (0 for a nil inbox).
func (b *importInbox) count() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

// list returns the waiting books, oldest first.
func (b *importInbox) list() []*pendingImport {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*pendingImport, 0, len(b.items))
	for _, it := range b.items {
		out = append(out, it)
	}
	slices.SortFunc(out, func(x, y *pendingImport) int {
		if c := x.Added.Compare(y.Added); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

func (b *importInbox) get(id string) *pendingImport {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.items[id]
}

// take removes a book from the queue for approving or discarding, so two
// requests can't both act on it; put returns it if that fails.
func (b *importInbox) take(id string) *pendingImport {
	b.mu.Lock()
	it := b.items[id]
	delete(b.items, id)
	b.mu.Unlock()
	b.dupMu.Lock()
	delete(b.dups, id)
	b.dupMu.Unlock()
	return it
}

func (b *importInbox) put(it *pendingImport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.items[it.ID] = it
}

// stage moves the files at paths (the formats of one book) into the queue
// and reads their metadata. from is the folder they came from, for display.
// If it fails, the files are back where they were.
func (b *importInbox) stage(paths []string, from string) (*pendingImport, error) {
	if len(paths) == 0 {
		return nil, errors.New("no files")
	}
	it := &pendingImport{ID: uuid.New().String(), Added: time.Now().UTC(), From: from}
	dir := b.itemDir(it.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	type move struct{ from, to string }
	var moved []move
	fail := func(err error) (*pendingImport, error) {
		clean := true
		for _, m := range moved {
			if bErr := moveFile(m.to, m.from); bErr != nil {
				log.Printf("imports: couldn't move %s back to %s: %v", m.to, m.from, bErr)
				clean = false
			}
		}
		if clean { // otherwise a book file is still in there
			os.RemoveAll(dir)
		}
		return nil, err
	}

	for _, p := range paths {
		name := filepath.Base(p)
		dest := filepath.Join(dir, name)
		if err := moveFile(p, dest); err != nil {
			return fail(fmt.Errorf("moving %s: %w", p, err))
		}
		moved = append(moved, move{p, dest})
		info, err := os.Stat(dest)
		if err != nil {
			return fail(err)
		}
		meta := readBookMeta(dest, name)
		it.Files = append(it.Files, importFile{Name: name, Format: meta.Format, Size: info.Size()})
		it.Sources = append(it.Sources, meta)
	}
	it.Merged = *mergeMeta(it.Sources)
	it.Merged.Format, it.Merged.FileName = "current", ""

	data, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "item.json"), data, 0o600); err != nil {
		return fail(err)
	}
	// Its cover from its files, if one can be had without a browser
	// (import_covers.go); else the review page picks one.
	extractImportCover(dir, it.Files)
	b.put(it)
	return it, nil
}

// discard deletes a waiting book and its files.
func (b *importInbox) discard(id string) error {
	it := b.take(id)
	if it == nil {
		return fs.ErrNotExist
	}
	if err := os.RemoveAll(b.itemDir(id)); err != nil {
		b.put(it)
		return err
	}
	return nil
}

// approve adds a waiting book to the library with the metadata the admin
// settled on, its chosen cover and all its files, and removes it from the
// queue. If the book can't be added, it stays in the queue as it was.
//
// A book that looks already in the library (duplicates.go) isn't added: the
// error is a *duplicateError. override adds it anyway, unless the library
// holds one of its files byte for byte.
func (b *importInbox) approve(id string, meta OPFMetadata, override bool) (bookUUID string, err error) {
	it := b.take(id)
	if it == nil {
		return "", fs.ErrNotExist
	}
	dir := b.itemDir(id)

	check, err := findDuplicates(calibreDB, baseDir, meta.Title, meta.Creators, b.dupFiles(it), "")
	if err != nil {
		b.put(it)
		return "", fmt.Errorf("checking for duplicates: %w", err)
	}
	if check.blocks(override) {
		b.put(it)
		return "", &duplicateError{check}
	}

	entries := make([]formatEntry, 0, len(it.Files))
	for _, f := range it.Files {
		entries = append(entries, formatEntry{Ext: f.Format, Filename: f.Name, Size: f.Size, TempPath: filepath.Join(dir, f.Name)})
	}
	// The editor doesn't show library identifiers (an EPUB's own urn:uuid:),
	// so they aren't in what it sent: the files' stay, as on upload.
	libraryIDs, _ := splitLibraryIdentifiers(it.Merged.Identifiers)
	_, edited := splitLibraryIdentifiers(meta.Identifiers)
	book := &UploadedBookMeta{OPFMetadata: meta}
	book.Identifiers = append(slices.Clone(libraryIDs), edited...)
	book.Meta = it.Merged.Meta // series

	bookUUID, bookPath, err := addBookToLibrary(book, entries)
	if err != nil {
		b.put(it)
		return "", err
	}

	// From here the book is in the library: what's left can't undo that.
	destDir := filepath.Join(baseDir, bookPath)
	if _, err := os.Stat(filepath.Join(dir, "cover.jpg")); err == nil {
		if err := copyFile(filepath.Join(dir, "cover.jpg"), filepath.Join(destDir, "cover.jpg")); err != nil {
			log.Printf("imports: %s: couldn't copy the cover: %v", bookUUID, err)
		} else if _, err := calibreDB.Exec(`UPDATE books SET has_cover = 1 WHERE uuid = ?`, bookUUID); err != nil {
			log.Printf("imports: %s: failed to set has_cover: %v", bookUUID, err)
		}
	}
	// The same write the editor's Apply does: the description, every
	// author's sort name and the identifiers reach metadata.db too.
	if _, err := updateBookInCalibreDB(calibreDB, baseDir, bookUUID, &book.OPFMetadata); err != nil {
		log.Printf("imports: %s: added, but its metadata is incomplete: %v", bookUUID, err)
	}
	// What each file said stays on offer in the editor, as after an upload.
	bookMetadataCache.Store(bookUUID, it.Sources)

	if err := os.RemoveAll(dir); err != nil {
		log.Printf("imports: couldn't remove %s: %v", dir, err)
	}
	return bookUUID, nil
}

// ---------------------------------------------------------------------------
// Watching the folder
// ---------------------------------------------------------------------------

// fileStamp is what tells a file that is still being copied from one that's
// done: its size and modification time, unchanged for a while.
type fileStamp struct {
	size int64
	mod  int64
}

// seenFile is a file's stamp and when a scan first saw it that way.
type seenFile struct {
	fileStamp
	since time.Time
}

type folderWatcher struct {
	dir   string
	inbox *importInbox

	settle    time.Duration // how long a file must be unchanged; 0: just since the scan before
	forcePoll bool          // never watch for events: scan on a timer (-watch-dir-force-polling)

	mu       sync.Mutex
	seen     map[string]seenFile  // every book file in the last scan
	failed   map[string]fileStamp // ones that couldn't be taken; left alone until they change
	dirs     []string             // the folder and its subfolders, as of the last scan
	waiting  int                  // book files seen but not taken yet
	settling int                  // those of them a later scan may take without anything changing
	lastErr  string               // why the last scan failed or left a file behind
	polling  bool                 // scanning on a timer: the OS isn't reporting changes
}

func newFolderWatcher(dir string, inbox *importInbox) *folderWatcher {
	return &folderWatcher{dir: dir, inbox: inbox, seen: map[string]seenFile{}, failed: map[string]fileStamp{}}
}

// isBookFile reports whether name has a book format's extension, in any case.
func isBookFile(name string) bool { return hasBookExt(strings.ToLower(name)) }

// scan looks through the folder (and its subfolders) once and moves every
// finished book into the queue. Files in one folder with the same name and
// different formats ("Book.epub", "Book.pdf") are one book, taken together
// once all of them are finished. It returns how many books it took.
//
// Hidden files and folders (".name") and symbolic links are left alone.
func (w *folderWatcher) scan() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	at := time.Now()
	now := map[string]seenFile{}
	groups := map[string][]string{} // folder + name without extension -> its files
	var order, dirs []string
	err := filepath.WalkDir(w.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == w.dir {
				return err
			}
			return nil // an unreadable subfolder doesn't stop the rest
		}
		name := d.Name()
		if p != w.dir && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		if !d.Type().IsRegular() || !isBookFile(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // gone again
		}
		st := seenFile{fileStamp{info.Size(), info.ModTime().UnixNano()}, at}
		if prev, ok := w.seen[p]; ok && prev.fileStamp == st.fileStamp {
			st.since = prev.since
		}
		now[p] = st
		key := filepath.Dir(p) + "\x00" + strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
		return nil
	})
	if err != nil {
		w.note(fmt.Sprintf("can't read the watch folder: %v", err))
		return 0
	}
	scanErr := ""

	taken := 0
	for _, key := range order {
		paths := groups[key]
		finished := true
		for _, p := range paths {
			st := now[p]
			if prev, ok := w.seen[p]; !ok || prev.fileStamp != st.fileStamp || at.Sub(st.since) < w.settle ||
				st.size == 0 || w.failed[p] == st.fileStamp {
				finished = false
			}
		}
		if !finished {
			continue
		}
		// One file per format; a second ("Book.epub", "book.EPUB") is its
		// own book on the next scan.
		var take []string
		formats := map[string]bool{}
		for _, p := range paths {
			if ext := strings.ToLower(filepath.Ext(p)); !formats[ext] {
				formats[ext] = true
				take = append(take, p)
			}
		}
		from, _ := filepath.Rel(w.dir, filepath.Dir(take[0]))
		if from == "." {
			from = ""
		}
		it, err := w.inbox.stage(take, from)
		if err != nil {
			scanErr = fmt.Sprintf("couldn't take %s: %v", take[0], err)
			for _, p := range take {
				w.failed[p] = now[p].fileStamp
			}
			continue
		}
		for _, p := range take {
			delete(now, p)
		}
		taken++
		log.Printf("watch folder: %q is waiting for review (%d file(s), /import/%s)", it.Merged.Title, len(it.Files), it.ID)
	}

	for p := range w.failed {
		if _, ok := now[p]; !ok {
			delete(w.failed, p)
		}
	}
	if scanErr == "" && len(w.failed) > 0 {
		scanErr = w.lastErr // still true: those files are still there
	}
	// Empty files and ones that couldn't be taken only matter again when
	// they change; the rest are taken by a later scan as they are.
	settling := 0
	for p, st := range now {
		if st.size > 0 && w.failed[p] != st.fileStamp {
			settling++
		}
	}
	w.seen, w.dirs, w.waiting, w.settling = now, dirs, len(now), settling
	w.note(scanErr)
	return taken
}

// note records the scan's problem, logging it when it changes (not every
// few seconds). Called with w.mu held.
func (w *folderWatcher) note(problem string) {
	if problem != "" && problem != w.lastErr {
		log.Println("watch folder:", problem)
	}
	w.lastErr = problem
}

// run watches the folder until the program ends. The operating system
// reports changes to it (inotify, kqueue, ReadDirectoryChangesW; fsnotify),
// so nothing is read while nothing happens: an event leads to a scan, and
// scans go on every watchSettle while files are still settling. Where the
// folder can't be watched that way, it is scanned every watchPollInterval.
func (w *folderWatcher) run() {
	if w.forcePoll {
		w.poll(nil)
		return
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.poll(err)
		return
	}
	defer fsw.Close()

	watched := map[string]bool{}
	// follow watches the folders the last scan found. Watches aren't
	// recursive, so each subfolder needs its own.
	follow := func() error {
		w.mu.Lock()
		dirs := slices.Clone(w.dirs)
		w.mu.Unlock()
		current := make(map[string]bool, len(dirs))
		for _, d := range dirs {
			current[d] = true
			if watched[d] {
				continue
			}
			if err := fsw.Add(d); err != nil {
				if d != w.dir && errors.Is(err, fs.ErrNotExist) {
					continue // removed since the scan
				}
				return fmt.Errorf("%s: %w", d, err)
			}
			watched[d] = true
		}
		for d := range watched {
			if !current[d] { // gone; its watch went with it
				delete(watched, d)
			}
		}
		return nil
	}

	// Watch, then scan: nothing that arrives in between is missed. A scan
	// finds new subfolders, which are then watched and scanned once more.
	if err := fsw.Add(w.dir); err != nil {
		w.poll(err)
		return
	}
	watched[w.dir] = true
	log.Printf("watch folder: watching %s for changes", w.dir)

	timer := time.NewTimer(0)
	pending := true // a scan is due when the timer fires
	schedule := func(d time.Duration) {
		timer.Reset(d)
		pending = true
	}
	for {
		select {
		case _, ok := <-fsw.Events:
			if !ok {
				w.poll(errors.New("the watcher stopped"))
				return
			}
			// A scan already due sooner than a settle away is soon enough.
			if !pending {
				schedule(watchDebounce)
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				w.poll(errors.New("the watcher stopped"))
				return
			}
			// Typically events were dropped (too many at once): look.
			log.Println("watch folder:", err)
			schedule(watchDebounce)
		case <-timer.C:
			pending = false
			before := len(watched)
			w.scan()
			if err := follow(); err != nil {
				w.poll(err)
				return
			}
			w.mu.Lock()
			settling := w.settling
			w.mu.Unlock()
			switch {
			case len(watched) > before:
				schedule(watchDebounce) // files may have arrived in a new subfolder before it was watched
			case settling > 0:
				schedule(w.settle)
			}
		}
	}
}

// poll scans the folder on a timer, for when it can't be watched (why) or
// the admin asked for it (nil).
func (w *folderWatcher) poll(why error) {
	if why == nil {
		log.Printf("watch folder: -watch-dir-force-polling: checking %s every %s", w.dir, watchPollInterval)
	} else {
		log.Printf("watch folder: the system can't report changes to %s (%v); checking it every %s instead", w.dir, why, watchPollInterval)
	}
	w.mu.Lock()
	w.polling = true
	w.mu.Unlock()
	for {
		w.scan()
		time.Sleep(watchPollInterval)
	}
}

func (w *folderWatcher) status() (waiting int, problem string, polling bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waiting, w.lastErr, w.polling
}

// insideDir reports whether p is dir or somewhere under it.
func insideDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath is p made absolute, with symbolic links resolved when it exists.
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return p
}

// checkWatchDir refuses a watch folder that would have the watcher taking
// files out of the library or its own queue.
func checkWatchDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a folder")
	}
	w := realPath(dir)
	for what, other := range map[string]string{"the library (-basedir)": baseDir, "-datadir": dataDir} {
		o := realPath(other)
		if insideDir(w, o) || insideDir(o, w) {
			return fmt.Errorf("it overlaps %s: use a folder of its own", what)
		}
	}
	return nil
}

// startImports opens the import queue and, with -watch-dir, starts watching.
func startImports() {
	var err error
	if imports, err = openInbox(filepath.Join(dataDir, "imports")); err != nil {
		log.Fatal("import queue: ", err)
	}
	if n := imports.count(); n > 0 {
		log.Printf("import queue: %d book(s) waiting for review at /import", n)
		go backfillImportCovers(imports)
	}
	if watchDir == "" {
		if watchForcePolling {
			log.Println("-watch-dir-force-polling does nothing without -watch-dir")
		}
		return
	}
	if !allowWrites {
		// Taking files out of a folder is a change the server was told not to make.
		log.Println("-watch-dir is ignored without -allow-writes")
		return
	}
	if err := checkWatchDir(watchDir); err != nil {
		log.Fatalf("-watch-dir %s: %v", watchDir, err)
	}
	watcher = newFolderWatcher(watchDir, imports)
	watcher.settle, watcher.forcePoll = watchSettle, watchForcePolling
	log.Printf("-watch-dir: books put in %s wait for an admin's review at /import", watchDir)
	go watcher.run()
}

// ---------------------------------------------------------------------------
// HTTP: the queue and the review page's endpoints. All behind requireWrites.
// ---------------------------------------------------------------------------

// importPageData is what templates/book-edit.html needs to review a waiting
// book instead of editing one in the library.
type importPageData struct {
	ID       string
	Files    []importFile
	From     string
	HasCover bool
	Waiting  int // books in the queue, this one included
	// The book added just before this one, when the admin is working
	// through the queue.
	AddedUUID, AddedTitle string
}

// importSummary is a waiting book in the queue's list (TS: ImportSummary).
type importSummary struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Authors  []string     `json:"authors"`
	Files    []importFile `json:"files"`
	From     string       `json:"from"`
	Added    time.Time    `json:"added"`
	HasCover bool         `json:"has_cover"`
	// Books in the library it may duplicate (duplicates.go); nil for none.
	Duplicate *duplicateCheck `json:"duplicate,omitempty"`
}

// importQueue is GET /api/imports (TS: ImportQueue).
type importQueue struct {
	Watching bool            `json:"watching"`
	Folder   string          `json:"folder,omitempty"`  // the watch folder
	Polling  bool            `json:"polling,omitempty"` // it is checked on a timer: the OS doesn't report changes to it
	Settling int             `json:"settling"`          // files in it not taken yet (still being copied, or just seen)
	Problem  string          `json:"problem,omitempty"` // why a file can't be taken
	Items    []importSummary `json:"items"`
}

func registerImportRoutes() {
	http.HandleFunc("GET /import", requireWrites(importQueuePage))
	http.HandleFunc("GET /api/imports", requireWrites(importQueueHandler))
	http.HandleFunc("GET /import/{id}", requireWrites(importReviewPage))
	http.HandleFunc("GET /import/{id}/metadata", requireWrites(importMetadataHandler))
	http.HandleFunc("POST /import/{id}/apply", requireWrites(importApplyHandler))
	http.HandleFunc("POST /import/{id}/discard", requireWrites(importDiscardHandler))
	http.HandleFunc("/import/{id}/isbns", requireWrites(importISBNsHandler))
	http.HandleFunc("GET /import/{id}/formats", requireWrites(importFormatsHandler))
	http.HandleFunc("GET /import/{id}/file/{format}", requireWrites(importFileHandler))
	http.HandleFunc("GET /import/{id}/cover", requireWrites(importCoverHandler))
	http.HandleFunc("POST /import/{id}/cover", requireWrites(importSaveCoverHandler))
	http.HandleFunc("GET /import/{id}/cover/status", requireWrites(importCoverStatusHandler))
	http.HandleFunc("POST /import/{id}/cover/revert", requireWrites(importRevertCoverHandler))
}

// importFromRequest is the waiting book a request names, or nil after
// answering 404.
func importFromRequest(w http.ResponseWriter, r *http.Request) *pendingImport {
	id := r.PathValue("id")
	if isSafeUUIDComponent(id) {
		if it := imports.get(id); it != nil {
			return it
		}
	}
	http.Error(w, "no such book in the import queue (already added or discarded?)", http.StatusNotFound)
	return nil
}

func importQueuePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "imports.html", PageData{Title: SiteTitle}), "render imports")
}

func importQueueHandler(w http.ResponseWriter, r *http.Request) {
	q := importQueue{Watching: watching(), Items: []importSummary{}}
	if watcher != nil {
		q.Folder = watcher.dir
		q.Settling, q.Problem, q.Polling = watcher.status()
	}
	for _, it := range imports.list() {
		hasCover, _ := coverFilesIn(imports.itemDir(it.ID))
		s := importSummary{
			ID: it.ID, Title: it.Merged.Title, Authors: it.Merged.Creators,
			Files: it.Files, From: it.From, Added: it.Added, HasCover: hasCover,
		}
		if dup := imports.duplicates(it); dup.found() {
			s.Duplicate = dup
		}
		q.Items = append(q.Items, s)
	}
	writeJSON(w, http.StatusOK, q)
}

func importReviewPage(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	hasCover, _ := coverFilesIn(imports.itemDir(it.ID))
	page := &importPageData{ID: it.ID, Files: it.Files, From: it.From, HasCover: hasCover, Waiting: imports.count()}
	if added := r.URL.Query().Get("added"); isSafeUUIDComponent(added) {
		if calibreDB.QueryRow(`SELECT title FROM books WHERE uuid = ?`, added).Scan(&page.AddedTitle) == nil {
			page.AddedUUID = added
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	formats := make([]string, len(it.Files))
	for i, f := range it.Files {
		formats[i] = f.Format
	}
	logErr(templates.ExecuteTemplate(w, "book-edit.html", EditBookPageData{
		Import:      page,
		Duplicates:  imports.duplicates(it),
		ReadLink:    previewReadLink("import", it.ID, formats),
		Lookups:     !noLookups,
		GoogleBooks: googleBooksEnabled(),
	}), "render import review")
}

// importSources is what the editor offers for a waiting book: the merged
// metadata as "current", then what each file says. As for a book in the
// library, library identifiers aren't shown, and dates are the day only
// (the editor's date field). The "no date" placeholder is shown as none.
func importSources(it *pendingImport) []UploadedBookMeta {
	out := make([]UploadedBookMeta, 0, len(it.Sources)+1)
	out = append(out, it.Merged)
	for _, s := range it.Sources {
		out = append(out, *s)
	}
	noDate := normalizeDateOnly(normalizeDate(""))
	for i := range out {
		_, out[i].Identifiers = splitLibraryIdentifiers(out[i].Identifiers)
		if out[i].Date = normalizeDateOnly(out[i].Date); out[i].Date == noDate {
			out[i].Date = ""
		}
	}
	return out
}

func importMetadataHandler(w http.ResponseWriter, r *http.Request) {
	if it := importFromRequest(w, r); it != nil {
		writeJSON(w, http.StatusOK, importSources(it))
	}
}

// nextImport is the book to review after id: the next one in the queue,
// else the first; "" when id is the only one.
func nextImport(list []*pendingImport, id string) string {
	i := slices.IndexFunc(list, func(it *pendingImport) bool { return it.ID == id })
	for n := 1; n < len(list); n++ {
		if next := list[(max(i, 0)+n)%len(list)]; next.ID != id {
			return next.ID
		}
	}
	return ""
}

func importApplyHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	var req ApplyMetadataRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	meta := metadataFromRequest(req)
	next := nextImport(imports.list(), it.ID)

	bookUUID, err := imports.approve(it.ID, meta, req.Override)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "this book was just added or discarded by someone else", http.StatusConflict)
		return
	}
	if dup, ok := errors.AsType[*duplicateError](err); ok {
		writeJSON(w, http.StatusConflict, map[string]any{"error": dup.Error(), "duplicate": dup.check})
		return
	}
	if err != nil {
		log.Println("imports: add to library:", err)
		http.Error(w, "Couldn't add the book to the library: "+err.Error(), http.StatusInternalServerError)
		return
	}
	catalogChanged(bookUUID)
	generatePage1()
	refreshAllCaches()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "uuid": bookUUID, "title": meta.Title, "next": next})
}

func importDiscardHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	if err := imports.discard(it.ID); err != nil && !errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "Couldn't discard the book: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("imports: %q discarded by %s", it.Merged.Title, currentUser(r).Username)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// importISBNsHandler is /metadata/{uuid}/isbns for a waiting book.
func importISBNsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	var pdf *pdfISBNSources
	if r.Method == http.MethodPost {
		pdf = &pdfISBNSources{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(pdf); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	names := make([]string, 0, len(it.Files))
	for _, f := range it.Files {
		names = append(names, f.Name)
	}
	writeJSON(w, http.StatusOK, findBookISBNs(imports.itemDir(it.ID), names, pdf))
}

func importFormatsHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	formats := make([]string, 0, len(it.Files))
	for _, f := range it.Files {
		formats = append(formats, f.Format)
	}
	writeJSON(w, http.StatusOK, formats)
}

// importFileHandler serves one of a waiting book's files to the review page,
// which reads it for cover choices and ISBNs.
func importFileHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	for _, f := range it.Files {
		if f.Format == r.PathValue("format") {
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, filepath.Join(imports.itemDir(it.ID), f.Name))
			return
		}
	}
	http.NotFound(w, r)
}

func importCoverHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	p := filepath.Join(imports.itemDir(it.ID), "cover.jpg")
	if _, err := os.Stat(p); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, p)
}

func importSaveCoverHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	data, err := readCoverUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hasBackup, err := saveCoverJPEG(imports.itemDir(it.ID), data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "hasBackup": hasBackup})
}

func importCoverStatusHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	hasCover, hasBackup := coverFilesIn(imports.itemDir(it.ID))
	writeJSON(w, http.StatusOK, map[string]any{"hasCover": hasCover, "hasBackup": hasBackup})
}

func importRevertCoverHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	if err := revertCoverIn(imports.itemDir(it.ID)); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errNoCoverBackup) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "reverted": true, "hasBackup": true})
}
