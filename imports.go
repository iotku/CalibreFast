package main

// The watch folder and its import queue.
//
// With -watch-dir, book files that appear in that folder (the operating
// system says when; see folderWatcher.run) are moved into
// <datadir>/imports/<id>/ (or -import-dir) and wait there: nothing is added to metadata.db or
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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/iotku/CalibreFast/pkg/isbn"
	"github.com/iotku/CalibreFast/pkg/opf"
)

var (
	// watchDir is -watch-dir: the folder watched for new books ("" for none).
	watchDir string
	// importDir is -import-dir: the queue's folder ("" for <datadir>/imports;
	// importQueueDir). On the library's file system, adding a book renames
	// its files into it instead of copying them.
	importDir string
	// watchForcePolling is -watch-dir-force-polling: scan on a timer even
	// where the folder can be watched. Some mounts (NFS, SMB, some container
	// bind mounts) accept a watch and then never report anything.
	watchForcePolling bool
	// imports is the queue. It is opened even without -watch-dir, so books
	// left from an earlier run can still be reviewed.
	imports *importInbox
	// watcher scans watchDir; nil when nothing is watched.
	watcher *folderWatcher
	// importWorkers is -import-workers: how many books a scan stages at
	// once (reading their metadata is CPU work). Never more than the CPU
	// threads (startImports).
	importWorkers = min(runtime.NumCPU(), 8)
	// stageIO bounds the file moves (a full copy where the watch folder is
	// on another file system) going on at once, so parallel staging doesn't
	// have a hard disk seeking between many large files.
	stageIO = make(chan struct{}, 2)
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

// pendingImport is a book waiting for review (item.json). Quick edits in
// the queue's table (imports_edit.go) replace Merged, never in place: a
// new copy goes into importInbox.items, so whoever holds the old one reads
// it whole. The review page's edits go straight into the library on
// approval.
type pendingImport struct {
	ID    string       `json:"id"`
	Added time.Time    `json:"added"`
	From  string       `json:"from"` // folder inside the watch folder it came from; "" for its top
	Files []importFile `json:"files"`
	// Into: new files for this library book (add_format.go), reviewed at
	// /book/<into>/add-format/<id>; "" for a book of its own.
	Into string `json:"into,omitempty"`
	// Placeholder: the placeholder book these files are for
	// (placeholders.go), deleted once the book is added.
	Placeholder string `json:"placeholder,omitempty"`
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

	// Quick edits' item.json writes (imports_edit_queue.go). flushMu is
	// held while an item.json is written from items or a book is taken;
	// lock it before mu, never while holding mu.
	flushMu   sync.Mutex
	dirty     map[string]int64 // under mu: books whose item.json is behind items, by journal version
	editSeq   int64            // under mu: the last journal version
	flushWake chan struct{}    // nil: no writer (tests), item.json is written at once

	coverMu    sync.Mutex
	coverIDs   []string        // waiting for extractImportCover (coverSoon)
	coverWake  chan struct{}   // nil: no cover worker (tests), covers are taken at once
	coverTried map[string]bool // books whose files were looked at for a cover since the server started

	dupMu      sync.Mutex
	dups       map[string]dupResult // duplicates, by id: the list's last check
	dupRunning bool                 // checkDuplicatesSoon is at work
	dupAgain   bool                 // and should look again when done
}

// openInbox opens (creating if needed) the queue in dir and loads the books
// waiting in it.
func openInbox(dir string) (*importInbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	b := &importInbox{dir: dir, items: map[string]*pendingImport{}, dirty: map[string]int64{}}
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
	// No item.json write of it is under way once it's taken, and none starts.
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	return b.takeLocked(id)
}

// takeLocked is take for a caller holding b.flushMu.
func (b *importInbox) takeLocked(id string) *pendingImport {
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
//
// Its cover is taken from its files in the background (coverSoon), so the
// book is listed without waiting for that.
func (b *importInbox) stage(paths []string, from, into string) (*pendingImport, error) {
	return b.stageFiles(paths, from, into, false)
}

// stageFiles is stage; coverNow takes the cover from the files before it
// returns.
func (b *importInbox) stageFiles(paths []string, from, into string, coverNow bool) (*pendingImport, error) {
	if len(paths) == 0 {
		return nil, errors.New("no files")
	}
	it := &pendingImport{ID: uuid.New().String(), Added: time.Now().UTC(), From: from, Into: into}
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
		stageIO <- struct{}{}
		err := moveFile(p, dest)
		<-stageIO
		if err != nil {
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
	// No series in the files: a placeholder for the book says which, so
	// the review (or an automatic add) starts with it. The placeholder is
	// deleted only once the book is added (resolvePlaceholders), so one the
	// reviewer decides isn't this book stays.
	fillFromPlaceholder(&it.Merged.OPFMetadata)

	if err := writeItemJSON(dir, it); err != nil {
		return fail(err)
	}
	// Its cover from its files, if one can be had without a browser
	// (import_covers.go); else the review page picks one.
	if coverNow {
		extractImportCover(dir, it.Files)
	}
	b.put(it)
	if !coverNow {
		b.coverSoon(it.ID)
	}
	return it, nil
}

// stageForPlaceholder queues files for placeholder p: what p says is the
// starting metadata, and p's cover is the book's unless the files have one
// (so theirs is taken first, not in the background).
func (b *importInbox) stageForPlaceholder(paths []string, p *placeholder) (*pendingImport, error) {
	it, err := b.stageFiles(paths, "", "", true)
	if err != nil {
		return nil, err
	}
	dir := b.itemDir(it.ID)
	b.mu.Lock()
	defer b.mu.Unlock()
	it.Placeholder = p.UUID
	it.Merged.OPFMetadata = withPlaceholder(it.Merged.OPFMetadata, p)
	if err := writeItemJSON(dir, it); err != nil {
		log.Printf("imports: %s: %v", it.ID, err)
	}
	cover := filepath.Join(dir, "cover.jpg")
	if _, err := os.Stat(cover); err != nil && strings.HasSuffix(p.Cover, ".jpg") {
		if err := copyFile(filepath.Join(dataDir, placeholderCoversDir, p.Cover), cover); err != nil {
			log.Printf("imports: %s: the placeholder's cover: %v", it.ID, err)
		}
	}
	return it, nil
}

// waitingIn counts the waiting books' folders in dir (a queue folder not in
// use), without loading them.
func waitingIn(dir string) int {
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if e.IsDir() && isSafeUUIDComponent(e.Name()) {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "item.json")); err == nil {
				n++
			}
		}
	}
	return n
}

// discard takes a waiting book out of the queue, to discardedDir.
func (b *importInbox) discard(id string) error {
	b.flushMu.Lock()
	it := b.takeLocked(id)
	if it == nil {
		b.flushMu.Unlock()
		return fs.ErrNotExist
	}
	// Quick edits not yet in its item.json go with it (it can be restored).
	if err := b.flushTaken(it); err != nil {
		b.flushMu.Unlock()
		b.put(it)
		return fmt.Errorf("saving its edits first: %w", err)
	}
	b.flushMu.Unlock()
	// Kept, not deleted: it may be someone's only copy, and the discard can
	// be undone (editlog.go). Nothing on the web empties the folder.
	if err := os.MkdirAll(b.discardedDir(), 0o700); err != nil {
		b.put(it)
		return err
	}
	if err := moveDir(b.itemDir(id), filepath.Join(b.discardedDir(), id)); err != nil {
		b.put(it)
		return err
	}
	return nil
}

// slowAdd: an add that takes longer is logged with how long each step
// took (approve).
const slowAdd = time.Second

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

	start := time.Now()
	check, err := findDuplicates(calibreDB, baseDir, meta.Title, meta.Creators, b.dupFiles(it), "")
	dupTime := time.Since(start)
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
	libraryIDs, _ := opf.SplitLibraryIdentifiers(it.Merged.Identifiers)
	_, edited := opf.SplitLibraryIdentifiers(meta.Identifiers)
	book := &UploadedBookMeta{OPFMetadata: meta}
	book.Identifiers = append(slices.Clone(libraryIDs), edited...)
	book.Meta = it.Merged.Meta // series, as the files had it…
	if meta.SeriesSet {
		book.SyncSeriesMeta() // …or as the editor sent it
	}
	if meta.RatingSet { // the rating the same way
		book.Rating = meta.Rating
		book.SyncRatingMeta()
	}

	// The cover and the editor's metadata go in with the book, and the
	// queue folder is removed once it's in (add_journal.go: finishAdd), so
	// even a crash part way through leaves the book here or there, whole.
	var timing addTiming
	opts := addOptions{Import: &addJournalImport{Dir: dir, Meta: book.OPFMetadata, SeriesSet: book.SeriesSet, RatingSet: book.RatingSet}, Timing: &timing}
	if _, err := os.Stat(filepath.Join(dir, "cover.jpg")); err == nil {
		opts.Cover = filepath.Join(dir, "cover.jpg")
	}
	// The placeholder it stands for, before adding it deletes it.
	ph := importPlaceholder(it)
	bookUUID, _, err = addBook(book, entries, opts)
	if err != nil {
		b.put(it)
		return "", err
	}
	if took := time.Since(start); took >= slowAdd {
		var size int64
		for _, f := range it.Files {
			size += f.Size
		}
		log.Printf("imports: adding %q took %v: duplicate check %v, files %v (%.1f MB into the library), database %v, last steps %v",
			meta.Title, took.Round(time.Millisecond), dupTime.Round(time.Millisecond), timing.Files.Round(time.Millisecond),
			float64(size)/1e6, timing.Database.Round(time.Millisecond), timing.Finish.Round(time.Millisecond))
	}
	// What each file said stays on offer in the editor, as after an upload,
	// and what its placeholder said.
	sources := slices.Clone(it.Sources)
	if ph != nil {
		sources = append(sources, placeholderSource(ph))
	}
	bookMetadataCache.Store(bookUUID, sources)
	b.dropPending(id) // it's in the library: its quick edits went in with it
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
	staging  int                  // books a scan is taking in right now
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
	// What's finished is staged with w.mu released, so the queue's page
	// (status) never waits on a batch being taken in, and several books are
	// staged at once (stageAll). Only this goroutine scans, so nothing else
	// changes seen or failed meanwhile.
	var jobs []stageJob
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
		jobs = append(jobs, stageJob{take, from})
	}
	w.staging = len(jobs)
	w.mu.Unlock()
	errs := w.stageAll(jobs)
	w.mu.Lock()

	scanErr := ""
	taken := 0
	for i, j := range jobs {
		if errs[i] != nil {
			scanErr = fmt.Sprintf("couldn't take %s: %v", j.paths[0], errs[i])
			for _, p := range j.paths {
				w.failed[p] = now[p].fileStamp
			}
			continue
		}
		for _, p := range j.paths {
			delete(now, p)
		}
		taken++
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

// stageJob is one book a scan takes: its files, and the folder they're in.
type stageJob struct {
	paths []string
	from  string
}

// stageAll stages jobs, importWorkers at a time, each book joining the
// queue (and its list) as soon as it's in. errs[i] is jobs[i]'s.
func (w *folderWatcher) stageAll(jobs []stageJob) (errs []error) {
	errs = make([]error, len(jobs))
	next := make(chan int)
	var wg sync.WaitGroup
	for range max(1, min(importWorkers, len(jobs))) {
		wg.Go(func() {
			for i := range next {
				it, err := w.inbox.stage(jobs[i].paths, jobs[i].from, "")
				errs[i] = err
				if err == nil {
					log.Printf("watch folder: %q is waiting for review (%d file(s), /import/%s)", it.Merged.Title, len(it.Files), it.ID)
				}
				w.mu.Lock()
				w.staging--
				w.mu.Unlock()
			}
		})
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()
	return errs
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

// status is for the queue's list. It never waits on a scan: w.mu is only
// held while the folder is read, not while books are staged.
func (w *folderWatcher) status() (waiting, staging int, problem string, polling bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.waiting, w.staging, w.lastErr, w.polling
}

// insideDir reports whether p is dir or somewhere under it.
func insideDir(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath is p made absolute, with symbolic links resolved: in p, or for a
// path not made yet, in the nearest folder above it that is.
func realPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	var rest []string
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, rest...)...)
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// importQueueDir is the queue's folder: -import-dir, else <datadir>/imports.
func importQueueDir() string {
	if importDir != "" {
		return filepath.Clean(importDir) // "x/" would put x/-discarded inside it
	}
	return filepath.Join(dataDir, "imports")
}

// checkImportDir refuses a queue folder that overlaps the library (Calibre
// would find stray folders in it, and the queue the library's) or holds
// -datadir. Inside -datadir is where it is by default.
func checkImportDir(dir string) error {
	q := realPath(dir) // may be made on first use
	l := realPath(baseDir)
	if insideDir(q, l) || insideDir(l, q) {
		return errors.New("it overlaps the library (-basedir): use a folder beside it, on the same drive")
	}
	if insideDir(realPath(dataDir), q) {
		return errors.New("it holds -datadir: use a folder of its own")
	}
	return nil
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
	for what, other := range map[string]string{"the library (-basedir)": baseDir, "-datadir": dataDir, "the import queue (-import-dir)": importQueueDir()} {
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
	dir := importQueueDir()
	if err := checkImportDir(dir); err != nil {
		log.Fatalf("-import-dir %s: %v", dir, err)
	}
	if imports, err = openInbox(dir); err != nil {
		log.Fatal("import queue: ", err)
	}
	// Before anything reads the waiting books: edits not yet in their
	// item.json (imports_edit_queue.go).
	if err := imports.replayImportEdits(); err != nil {
		log.Fatal("import queue: edits saved in the app database: ", err)
	}
	imports.startItemWriter()
	if importDir != "" {
		log.Printf("import queue: %s", dir)
		// Books left in the old folder aren't moved (a copy across drives,
		// perhaps a long one): they'd only be out of sight.
		if old := filepath.Join(dataDir, "imports"); realPath(old) != realPath(dir) {
			if n := waitingIn(old); n > 0 {
				log.Printf("import queue: %d book(s) still wait in %s, which -import-dir replaces: move their folders into %s (with the server stopped) to review them", n, old, dir)
			}
		}
	}
	imports.startCoverWorker()
	if n := imports.count(); n > 0 {
		log.Printf("import queue: %d book(s) waiting for review at /import", n)
		for _, it := range imports.list() { // staged before covers were taken from files
			imports.coverSoon(it.ID)
		}
	}
	importWorkers = max(1, min(importWorkers, runtime.NumCPU()))
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
	// Not checked for duplicates yet: that runs in the background, and a
	// later poll has the result.
	Checking bool `json:"checking,omitempty"`
	// Into: new files for this library book (add_format.go), reviewed at Link.
	Into      string `json:"into,omitempty"`
	IntoTitle string `json:"into_title,omitempty"`
	Link      string `json:"link,omitempty"`
}

// importQueue is GET /api/imports (TS: ImportQueue).
type importQueue struct {
	Watching bool            `json:"watching"`
	Folder   string          `json:"folder,omitempty"`  // the watch folder
	Polling  bool            `json:"polling,omitempty"` // it is checked on a timer: the OS doesn't report changes to it
	Settling int             `json:"settling"`          // files in it not taken yet (still being copied, or just seen)
	Staging  int             `json:"staging"`           // books being taken into the queue right now
	Problem  string          `json:"problem,omitempty"` // why a file can't be taken
	Items    []importSummary `json:"items"`
}

func registerImportRoutes() {
	http.HandleFunc("GET /import", requireEditor(importQueuePage))
	http.HandleFunc("GET /api/imports", requireEditor(importQueueHandler))
	http.HandleFunc("GET /import/edit", requireEditor(importEditPage))
	http.HandleFunc("GET /api/imports/table", requireEditor(importTableHandler))
	http.HandleFunc("POST /api/imports/bulk", requireEditor(importBulkHandler))
	http.HandleFunc("GET /import/{id}", requireEditor(importReviewPage))
	http.HandleFunc("GET /import/{id}/metadata", requireEditor(importMetadataHandler))
	http.HandleFunc("POST /import/{id}/apply", requireEditor(importApplyHandler))
	http.HandleFunc("POST /import/{id}/discard", requireEditor(importDiscardHandler))
	http.HandleFunc("POST /api/imports/discard-existing", requireEditor(importDiscardExistingHandler))
	http.HandleFunc("POST /import/{id}/accept", requireWrites(importAcceptHandler))
	http.HandleFunc("POST /api/imports/accept-done", requireWrites(importAcceptDoneHandler))
	http.HandleFunc("POST /import/{id}/discard-identical", requireEditor(importDiscardIdenticalHandler))
	http.HandleFunc("/import/{id}/isbns", requireEditor(importISBNsHandler))
	http.HandleFunc("GET /import/{id}/formats", requireEditor(importFormatsHandler))
	http.HandleFunc("GET /import/{id}/file/{format}", requireEditor(importFileHandler))
	http.HandleFunc("GET /import/{id}/as-epub/{format}", requireEditor(importAsEPUBHandler)) // convert_read.go
	http.HandleFunc("GET /import/{id}/cover", requireEditor(importCoverHandler))
	http.HandleFunc("POST /import/{id}/cover", requireEditor(importSaveCoverHandler))
	http.HandleFunc("GET /import/{id}/cover/status", requireEditor(importCoverStatusHandler))
	http.HandleFunc("POST /import/{id}/cover/revert", requireEditor(importRevertCoverHandler))
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
	u := currentUser(r)
	data := struct {
		Title string
		Admin bool // offered the mass import (imports_accept.go)
	}{SiteTitle, u != nil && u.IsAdmin}
	logErr(templates.ExecuteTemplate(w, "imports.html", data), "render imports")
}

func importQueueHandler(w http.ResponseWriter, r *http.Request) {
	q := importQueue{Watching: watching(), Items: []importSummary{}}
	if watcher != nil {
		q.Folder = watcher.dir
		q.Settling, q.Staging, q.Problem, q.Polling = watcher.status()
	}
	checking := false
	for _, it := range imports.list() {
		hasCover, _ := coverFilesIn(imports.itemDir(it.ID))
		s := importSummary{
			ID: it.ID, Title: it.Merged.Title, Authors: it.Merged.Creators,
			Files: it.Files, From: it.From, Added: it.Added, HasCover: hasCover,
		}
		if it.Into != "" {
			s.Into, s.Link = it.Into, addFormatPath(it)
			if b, ok := currentCatalog().lookup(it.Into); ok {
				s.IntoTitle = b.Title
			}
		} else {
			dup, ok, stale := imports.cachedDuplicates(it)
			s.Checking = !ok
			checking = checking || !ok || stale
			if dup.found() {
				s.Duplicate = dup
			}
		}
		q.Items = append(q.Items, s)
	}
	if checking {
		imports.checkDuplicatesSoon()
	}
	writeJSON(w, http.StatusOK, q)
}

func importReviewPage(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	if it.Into != "" {
		// New files for a library book: reviewed against it.
		http.Redirect(w, r, addFormatPath(it), http.StatusSeeOther)
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
		Duplicates:  imports.recheckDuplicates(it), // afresh: the library may have changed since the list
		ReadLink:    previewReadLink("import", it.ID, formats),
		Lookups:     !noLookups,
		GoogleBooks: googleBooksEnabled(),
		Hardcover:   hardcoverEnabled(),
	}), "render import review")
}

// importPlaceholder: the placeholder a waiting book stands for: the one
// its files were added for, else the one it matches (looked for now, so
// it holds for books queued before the placeholder was made).
func importPlaceholder(it *pendingImport) *placeholder {
	if it.Placeholder != "" {
		if p := placeholderByUUID(it.Placeholder); p != nil {
			return p
		}
	}
	return matchingPlaceholder(&it.Merged.OPFMetadata)
}

// importSources is what the editor offers for a waiting book: the merged
// metadata as "current", then what each file says, then its placeholder's. As for a book in the
// library, library identifiers aren't shown, and dates are the day only
// (the editor's date field). The "no date" placeholder is shown as none.
func importSources(it *pendingImport) []UploadedBookMeta {
	out := make([]UploadedBookMeta, 0, len(it.Sources)+2)
	out = append(out, it.Merged)
	for _, s := range it.Sources {
		out = append(out, *s)
	}
	// The placeholder it stands for, if any, to pick its values from.
	if p := importPlaceholder(it); p != nil {
		out = append(out, *placeholderSource(p))
	}
	noDate := normalizeDateOnly(normalizeDate(""))
	for i := range out {
		out[i].FillSeries() // queued before the editor showed series
		_, out[i].Identifiers = opf.SplitLibraryIdentifiers(out[i].Identifiers)
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
		if next := list[(max(i, 0)+n)%len(list)]; next.ID != id && next.Into == "" {
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
	if _, _, err := opf.CleanSeries(meta.Series, meta.SeriesIndex); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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
	logBookEdit(currentUser(r), "import", bookUUID, nil)
	if it.Placeholder != "" {
		p := placeholderByUUID(it.Placeholder)
		if err := replacePlaceholder(it.Placeholder, bookUUID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("imports: placeholder %s: %v", it.Placeholder, err)
		} else if p != nil {
			logEdit(currentUser(r), "placeholder added", logKindPlaceholder, p.UUID, p.Meta.Title, snapPlaceholder(p), nil)
		}
	}
	catalogChanged(bookUUID)
	refreshLibraryPages()
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
	logEdit(currentUser(r), "discard import", logKindImport, it.ID, it.Merged.Title, nil, nil)
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
	var pdf *isbn.PDFSources
	if r.Method == http.MethodPost {
		pdf = &isbn.PDFSources{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(pdf); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	names := make([]string, 0, len(it.Files))
	for _, f := range it.Files {
		names = append(names, f.Name)
	}
	writeJSON(w, http.StatusOK, isbn.FindInBook(imports.itemDir(it.ID), names, pdf))
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
