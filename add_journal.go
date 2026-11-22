package main

// Adding a book to the library so that the server dying part way through
// (a crash, a kill, a power cut) never leaves half a book behind.
//
// A book is two things that can't be changed together: its rows in
// metadata.db and its folder of files. addBookToLibrary orders the steps so
// the database commit is the one moment the book appears, and writes a
// journal first so whatever was done before a crash can be put back:
//
//  1. The journal (<library>/.calibrefast-adding/<id>.json) lists each file,
//     where it came from and its name in the book's folder.
//  2. The files are moved (the cover copied) into a staging folder beside
//     it, <library>/.calibrefast-adding/<id>/: the library's filesystem, so
//     step 4 is a rename. Nothing in metadata.db points there.
//  3. The rows are inserted in one transaction. Before it commits, the
//     journal gets the new book's id, uuid and path, metadata.opf is
//     written, and the staging folder is renamed to the book's folder.
//  4. The commit: the book is in the library, files and all.
//  5. What follows the add (for a book from the import queue: the editor's
//     metadata and removing its queue folder) is done, then the journal is
//     removed.
//
// At startup, recoverInterruptedAdds reads every journal left behind. If
// metadata.db has the book (id and uuid), the add committed: step 5 is
// finished. If not, it's undone: the book's folder goes back to staging,
// each file back where it came from (into the queue folder, say), and the
// staging folder is removed. A file is only ever deleted where another
// whole copy of it is kept. A returned error takes the same way back.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iotku/CalibreFast/pkg/opf"
)

// addingDirName is the folder in the library for adds under way.
const addingDirName = ".calibrefast-adding"

func addingDir() string { return filepath.Join(baseDir, addingDirName) }

// addJournal is what's written about an add in progress.
type addJournal struct {
	ID      string           `json:"id"` // names the journal and the staging folder
	Started time.Time        `json:"started"`
	Title   string           `json:"title"` // for the log
	Files   []addJournalFile `json:"files"`
	// Set inside the insert's transaction, before the folder is renamed.
	BookID int64  `json:"book_id,omitempty"`
	UUID   string `json:"uuid,omitempty"`
	Path   string `json:"path,omitempty"`
	// Import: the add is of a book from the import queue.
	Import *addJournalImport `json:"import,omitempty"`
}

type addJournalFile struct {
	Src  string `json:"src"`  // where it came from
	Name string `json:"name"` // its name in the staging (then the book's) folder
	// Copied: Src is kept until the add is done, so undoing just drops the copy.
	Copied bool `json:"copied,omitempty"`
}

// addJournalImport finishes adding a book from the import queue.
type addJournalImport struct {
	Dir       string      `json:"dir"`  // its queue folder, removed once the book is in
	Meta      OPFMetadata `json:"meta"` // what the admin settled on (updateBookInCalibreDB)
	SeriesSet bool        `json:"series_set,omitempty"`
	RatingSet bool        `json:"rating_set,omitempty"`
}

func (j *addJournal) path() string    { return filepath.Join(addingDir(), j.ID+".json") }
func (j *addJournal) staging() string { return filepath.Join(addingDir(), j.ID) }

// save writes the journal so that it survives a crash: whole or not at all.
func (j *addJournal) save() error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := j.path() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cErr := f.Close(); err == nil {
		err = cErr
	}
	if err == nil {
		err = os.Rename(tmp, j.path())
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(addingDir())
	return nil
}

// syncDir flushes a folder's entries (renames, new files) to disk, where the
// system allows it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// addOptions is what an add does besides the book's files.
type addOptions struct {
	Cover  string            // copied into the book's folder as cover.jpg ("" for none)
	Import *addJournalImport // the book's import queue folder; see addJournalImport
	Timing *addTiming        // if set, how long the steps took
}

// addTiming is how long an add's steps took, for telling a slow disk from a
// busy database.
type addTiming struct {
	Files    time.Duration // the files into a staging folder in the library (a copy across drives)
	Database time.Duration // the rows, and the staging folder renamed to the book's
	Finish   time.Duration // the add's last steps (an import's queue folder removed)
}

// errSimulatedCrash, from addStepHook in tests, stops an add where it is,
// as if the process had died: nothing is undone.
var errSimulatedCrash = errors.New("simulated crash")

// addStepHook is set by tests to fail (or crash) an add at a step: "journal",
// "file1"…, "journaled", "renamed", "committed", "finished".
var addStepHook func(step string) error

func addStep(step string) error {
	if addStepHook != nil {
		return addStepHook(step)
	}
	return nil
}

// addBookToLibrary adds a book to metadata.db, writes its metadata.opf and
// moves its files (each entry's TempPath) into the library; the entries then
// name the files as they are there. If any step fails, nothing of the book is
// left in the library and its files are back where they were; if the
// process dies, recoverInterruptedAdds sees to that at the next start.
func addBookToLibrary(meta *UploadedBookMeta, entries []formatEntry) (bookUUID, bookPath string, err error) {
	return addBook(meta, entries, addOptions{})
}

func addBook(meta *UploadedBookMeta, entries []formatEntry, opts addOptions) (bookUUID, bookPath string, err error) {
	if len(entries) == 0 {
		return "", "", errors.New("no book files")
	}
	var token [8]byte
	rand.Read(token[:])
	j := &addJournal{ID: hex.EncodeToString(token[:]), Started: time.Now().UTC(), Title: meta.Title, Import: opts.Import}

	// Each file is named as its data.name row says, or Calibre can't find
	// it: <title>.<format>, numbered when two share a format.
	baseName := bookFileStem(meta.Title)
	taken := map[string]bool{"metadata.opf": true, "cover.jpg": true}
	for i := range entries {
		stem := baseName
		for n := 2; taken[strings.ToLower(stem+"."+entries[i].Ext)]; n++ {
			stem = baseName + "_" + strconv.Itoa(n)
		}
		taken[strings.ToLower(stem+"."+entries[i].Ext)] = true
		entries[i].Stem = stem
		src, aErr := filepath.Abs(entries[i].TempPath)
		if aErr != nil {
			return "", "", aErr
		}
		j.Files = append(j.Files, addJournalFile{Src: src, Name: stem + "." + entries[i].Ext})
	}
	if opts.Cover != "" {
		src, aErr := filepath.Abs(opts.Cover)
		if aErr != nil {
			return "", "", aErr
		}
		j.Files = append(j.Files, addJournalFile{Src: src, Name: "cover.jpg", Copied: true})
	}

	if err := os.MkdirAll(addingDir(), 0o755); err != nil {
		return "", "", fmt.Errorf("failed to create %s: %w", addingDirName, err)
	}
	if err := j.save(); err != nil {
		return "", "", fmt.Errorf("failed to write the add's journal: %w", err)
	}
	// From here on, a returned error puts everything back (a simulated
	// crash leaves it for recovery, as a real one would).
	defer func() {
		if err != nil && !errors.Is(err, errSimulatedCrash) {
			_, rErr := resolveAdd(j)
			logErr(rErr, "undoing the add of "+strconv.Quote(meta.Title))
		}
	}()
	if err = addStep("journal"); err != nil {
		return "", "", err
	}

	timing := opts.Timing
	if timing == nil {
		timing = &addTiming{}
	}
	start := time.Now()
	staging := j.staging()
	if err = os.Mkdir(staging, 0o755); err != nil {
		return "", "", fmt.Errorf("failed to create the staging folder: %w", err)
	}
	for i, f := range j.Files {
		dst := filepath.Join(staging, f.Name)
		if f.Copied {
			if err = copyFileSync(f.Src, dst+partialSuffix); err == nil {
				err = os.Rename(dst+partialSuffix, dst)
			}
		} else {
			err = moveFile(f.Src, dst)
		}
		if err != nil {
			return "", "", fmt.Errorf("failed to move %s into the library: %w", filepath.Base(f.Src), err)
		}
		if err = addStep("file" + strconv.Itoa(i+1)); err != nil {
			return "", "", err
		}
	}

	timing.Files = time.Since(start)
	start = time.Now()
	bookOPF := &OPF{
		Metadata: OPFMetadata{ // TODO: Can we get things to a state where we can just pass in `merged` directly?
			Title:       meta.Title,
			Description: meta.Description,
			Publisher:   meta.Publisher,
			Date:        meta.Date,
			Languages:   meta.Languages,
			Identifiers: meta.Identifiers,
			Subjects:    meta.Subjects,
			Creators:    meta.Creators,
			Meta:        meta.Meta,
		},
	}
	_, bookUUID, bookPath, err = insertBookTx(calibreDB, meta, entries, func(tx *sql.Tx, id int64, uuid, path string) error {
		j.BookID, j.UUID, j.Path = id, uuid, path
		if err := j.save(); err != nil {
			return fmt.Errorf("failed to write the add's journal: %w", err)
		}
		if err := addStep("journaled"); err != nil {
			return err
		}
		if opts.Cover != "" {
			if _, err := tx.Exec(`UPDATE books SET has_cover = 1 WHERE id = ?`, id); err != nil {
				return fmt.Errorf("failed to set has_cover: %w", err)
			}
		}
		if err := opf.Write(bookOPF, filepath.Join(staging, "metadata.opf"), uuid); err != nil {
			return fmt.Errorf("failed to write metadata.opf: %w", err)
		}
		destDir := filepath.Join(baseDir, path) // NOTE: path is sanitized in insertBookIntoCalibreDB
		if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
			return fmt.Errorf("failed to create the author's folder: %w", err)
		}
		if err := setAsideStrayFolder(destDir, j.ID); err != nil {
			return err
		}
		if err := os.Rename(staging, destDir); err != nil {
			return fmt.Errorf("failed to move the book's folder into the library: %w", err)
		}
		syncDir(filepath.Dir(destDir))
		return addStep("renamed")
	})
	if err != nil {
		if !errors.Is(err, errSimulatedCrash) {
			err = fmt.Errorf("failed to insert into database: %w", err)
		}
		return "", "", err
	}

	timing.Database = time.Since(start)
	start = time.Now()

	// The book is in the library: nothing after this undoes that.
	if err := addStep("committed"); err != nil {
		return "", "", err
	}
	finishAdd(j)
	timing.Finish = time.Since(start)
	if err := addStep("finished"); err != nil {
		return "", "", err
	}
	logErr(os.Remove(j.path()), "removing the add's journal")
	os.Remove(addingDir()) // only if nothing else is under way

	for i := range entries {
		entries[i].Filename = j.Files[i].Name
		entries[i].TempPath = ""
	}
	uuidPathIndex.Store(bookUUID, bookPath)
	formatCache.Delete(bookUUID)
	// A placeholder for this book (placeholders.go) has served its turn.
	added := meta.OPFMetadata
	if added.Series == "" {
		added.FillSeries()
	}
	resolvePlaceholders(&added, bookUUID)
	return bookUUID, bookPath, nil
}

// setAsideStrayFolder moves a folder already at a new book's path (which
// metadata.db doesn't know: the path holds the new book's id) out of the
// way, beside it, rather than mixing it with the new book or deleting it.
func setAsideStrayFolder(destDir, token string) error {
	if _, err := os.Lstat(destDir); err != nil {
		return nil
	}
	if os.Remove(destDir) == nil { // empty
		return nil
	}
	aside := destDir + " (stray " + token + ")"
	if err := os.Rename(destDir, aside); err != nil {
		return fmt.Errorf("%s is in the way: %w", destDir, err)
	}
	log.Printf("add: %s was already there, but isn't in metadata.db; moved it to %s", destDir, aside)
	return nil
}

// finishAdd does what follows a committed add (step 5). It can be run again.
func finishAdd(j *addJournal) {
	im := j.Import
	if im == nil {
		return
	}
	meta := im.Meta
	meta.SeriesSet = im.SeriesSet
	meta.RatingSet = im.RatingSet
	// The same write the editor's Apply does: the description, every
	// author's sort name and the identifiers reach metadata.db too.
	if _, err := updateBookInCalibreDB(calibreDB, baseDir, j.UUID, &meta); err != nil {
		log.Printf("imports: %s: added, but its metadata is incomplete: %v", j.UUID, err)
	}
	if im.Dir == "" {
		return
	}
	// Its book files are in the library now; what's left is item.json and
	// the cover, which was copied. Kept if a book file is somehow still there.
	for _, f := range j.Files {
		if !f.Copied && filepath.Dir(f.Src) == filepath.Clean(im.Dir) && pathExists(f.Src) {
			log.Printf("imports: %s is in the library, but %s is still in its queue folder; leaving the folder", j.UUID, f.Src)
			return
		}
	}
	if err := os.RemoveAll(im.Dir); err != nil {
		log.Printf("imports: couldn't remove %s: %v", im.Dir, err)
	}
}

// resolveAdd settles an add that didn't finish: finished if metadata.db has
// the book (committed), else undone. The journal is removed once that's done.
func resolveAdd(j *addJournal) (committed bool, err error) {
	if j.BookID != 0 {
		var n int
		if err := calibreDB.QueryRow(`SELECT COUNT(*) FROM books WHERE id = ? AND uuid = ?`, j.BookID, j.UUID).Scan(&n); err != nil {
			return false, fmt.Errorf("checking metadata.db for book %d: %w", j.BookID, err)
		}
		if n > 0 {
			if pathExists(j.staging()) {
				log.Printf("add: %s is left over beside book %d; leaving it", j.staging(), j.BookID)
			}
			finishAdd(j)
			uuidPathIndex.Store(j.UUID, j.Path)
			return true, os.Remove(j.path())
		}
	}
	return false, undoAdd(j)
}

// undoAdd puts back the files of an add that never committed.
func undoAdd(j *addJournal) error {
	staging := j.staging()
	if j.Path != "" && !pathExists(staging) {
		// The folder was renamed into the library; the commit never came.
		// Its path holds this add's book id, which metadata.db doesn't have.
		dest := filepath.Join(baseDir, j.Path)
		if pathExists(dest) {
			if err := os.Rename(dest, staging); err != nil {
				return fmt.Errorf("moving %s back: %w", dest, err)
			}
			os.Remove(filepath.Dir(dest)) // the author's folder, if it was new
		}
	}
	var failed []string
	for _, f := range j.Files {
		staged := filepath.Join(staging, f.Name)
		os.Remove(staged + partialSuffix) // an unfinished copy; its source is whole
		if !pathExists(staged) {
			continue
		}
		if f.Copied || pathExists(f.Src) {
			// The source is whole (moveFile only removes it after a whole
			// copy is in place), so this is the spare.
			if err := os.Remove(staged); err != nil {
				failed = append(failed, staged)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(f.Src), 0o755); err != nil {
			failed = append(failed, staged)
			continue
		}
		if err := moveFile(staged, f.Src); err != nil {
			log.Printf("add: couldn't move %s back to %s: %v", staged, f.Src, err)
			failed = append(failed, staged)
		}
	}
	os.Remove(filepath.Join(staging, "metadata.opf"))
	if pathExists(staging) {
		if err := os.Remove(staging); err != nil {
			failed = append(failed, staging)
		}
	}
	if len(failed) > 0 {
		// The journal stays, for the next start to try again.
		return fmt.Errorf("couldn't put back %s", strings.Join(failed, ", "))
	}
	err := os.Remove(j.path())
	os.Remove(addingDir()) // only if nothing else is under way
	return err
}

// recoverInterruptedAdds settles every add the server didn't live to finish
// (see the top of this file). It runs at startup, before anything is added.
func recoverInterruptedAdds() {
	dir := addingDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		log.Printf("add recovery: %v", err)
		return
	}
	journals := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			if strings.HasSuffix(name, ".json.tmp") {
				os.Remove(filepath.Join(dir, name)) // never renamed into place: nothing was done
			}
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		journals[id] = true
		data, err := os.ReadFile(filepath.Join(dir, name))
		var j addJournal
		if err == nil {
			err = json.Unmarshal(data, &j)
		}
		if err == nil && j.ID != id {
			err = errors.New("the journal doesn't match its name")
		}
		if err != nil {
			log.Printf("add recovery: %s: %v; left alone", filepath.Join(dir, name), err)
			continue
		}
		switch committed, err := resolveAdd(&j); {
		case err != nil:
			log.Printf("add recovery: %q: %v", j.Title, err)
		case committed:
			log.Printf("add recovery: %q (book %d) was added before the server stopped; finished adding it", j.Title, j.BookID)
		default:
			log.Printf("add recovery: %q wasn't added before the server stopped; its files are back where they came from", j.Title)
		}
	}
	// A staging folder without a journal: emptied ones are removed; anything
	// in one is left for an admin to look at, never deleted.
	for _, e := range entries {
		if !e.IsDir() || journals[e.Name()] {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if os.Remove(p) != nil {
			log.Printf("add recovery: %s has files but no journal; left alone", p)
		}
	}
	os.Remove(dir) // if nothing is left
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
