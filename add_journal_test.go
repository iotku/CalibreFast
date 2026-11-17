package main

import (
	"bytes"
	"database/sql"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// The steps an add passes (add_journal.go: addStep), in order; the last two
// come after the commit.
var (
	uploadSteps = []string{"journal", "file1", "file2", "journaled", "renamed", "committed", "finished"}
	importSteps = []string{"journal", "file1", "file2", "file3", "journaled", "renamed", "committed", "finished"}
)

func committedAt(step string) bool { return step == "committed" || step == "finished" }

// stopAt makes the next add stop at step with err (errSimulatedCrash: as if
// the server died there).
func stopAt(t *testing.T, step string, err error) {
	t.Helper()
	addStepHook = func(s string) error {
		if s == step {
			return err
		}
		return nil
	}
	t.Cleanup(func() { addStepHook = nil })
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// libraryHasNoBookFolders fails if anything but metadata.db (and SQLite's
// own files) is in the library.
func libraryHasNoBookFolders(t *testing.T) {
	t.Helper()
	entries, _ := os.ReadDir(baseDir)
	for _, e := range entries {
		switch e.Name() {
		case "metadata.db", "metadata.db-wal", "metadata.db-shm", "metadata.db-journal":
		default:
			t.Errorf("%s is left in the library", e.Name())
		}
	}
}

type uploadFixture struct {
	paths []string
	data  [][]byte
}

func newUpload(t *testing.T) (uploadFixture, []formatEntry) {
	t.Helper()
	dir := t.TempDir()
	f := uploadFixture{data: [][]byte{importTestEPUB(t), []byte("%PDF-1.4 not really")}}
	var entries []formatEntry
	for i, ext := range []string{"epub", "pdf"} {
		p := filepath.Join(dir, "calibrefast-upload-x."+ext)
		writeTestFile(t, p, f.data[i])
		f.paths = append(f.paths, p)
		entries = append(entries, formatEntry{Ext: ext, Filename: "Go." + ext, Size: int64(len(f.data[i])), TempPath: p})
	}
	return f, entries
}

func uploadMeta() *UploadedBookMeta {
	return &UploadedBookMeta{OPFMetadata: OPFMetadata{Title: "Crash Course", Creators: []string{"Ann Author"}}}
}

// checkUploadSettled checks the library after a recovery: the book is there
// whole (added) or not at all, with its files back where they were.
func checkUploadSettled(t *testing.T, db *sql.DB, f uploadFixture, added bool) {
	t.Helper()
	if _, err := os.Stat(addingDir()); err == nil {
		t.Errorf("%s is still there", addingDir())
	}
	if !added {
		if n := countRows(t, db, "books"); n != 0 {
			t.Errorf("%d book(s) in metadata.db, want none", n)
		}
		if n := countRows(t, db, "data"); n != 0 {
			t.Errorf("%d format row(s) in metadata.db, want none", n)
		}
		libraryHasNoBookFolders(t)
		for i, p := range f.paths {
			if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, f.data[i]) {
				t.Errorf("%s isn't back as it was: %v", p, err)
			}
		}
		return
	}
	if n := countRows(t, db, "books"); n != 1 {
		t.Fatalf("%d book(s) in metadata.db, want 1", n)
	}
	if n := countRows(t, db, "data"); n != 2 {
		t.Errorf("%d format row(s), want 2", n)
	}
	calibreFindsFiles(t, db)
	var path string
	db.QueryRow(`SELECT path FROM books`).Scan(&path)
	if !exists(filepath.Join(baseDir, path, "metadata.opf")) {
		t.Error("no metadata.opf")
	}
	for _, p := range f.paths {
		if exists(p) {
			t.Errorf("%s is still there as well as in the library", p)
		}
	}
}

func TestUploadSurvivesACrashAtEveryStep(t *testing.T) {
	for _, step := range uploadSteps {
		t.Run(step, func(t *testing.T) {
			db := withTestLibrary(t)
			f, entries := newUpload(t)
			stopAt(t, step, errSimulatedCrash)
			if _, _, err := addBookToLibrary(uploadMeta(), entries); !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("add: %v", err)
			}
			addStepHook = nil
			recoverInterruptedAdds() // the next start
			checkUploadSettled(t, db, f, committedAt(step))
			recoverInterruptedAdds() // and the one after: nothing more to do
			checkUploadSettled(t, db, f, committedAt(step))
		})
	}
}

func TestUploadFailingAtAStepIsUndone(t *testing.T) {
	for _, step := range uploadSteps[:5] {
		t.Run(step, func(t *testing.T) {
			db := withTestLibrary(t)
			f, entries := newUpload(t)
			stopAt(t, step, errors.New("disk full"))
			if _, _, err := addBookToLibrary(uploadMeta(), entries); err == nil {
				t.Fatal("added anyway")
			}
			checkUploadSettled(t, db, f, false) // no recovery needed
		})
	}
}

// An unfinished copy (moveFile across filesystems) in the staging folder is
// dropped; its source is whole.
func TestRecoveryDropsAPartialCopy(t *testing.T) {
	db := withTestLibrary(t)
	f, entries := newUpload(t)
	stopAt(t, "file1", errSimulatedCrash)
	addBookToLibrary(uploadMeta(), entries)
	staged, _ := filepath.Glob(filepath.Join(addingDir(), "*", "Crash Course.epub"))
	if len(staged) != 1 {
		t.Fatalf("staged: %v", staged)
	}
	writeTestFile(t, filepath.Join(filepath.Dir(staged[0]), "Crash Course.pdf"+partialSuffix), []byte("%PDF-1.4 no"))
	addStepHook = nil
	recoverInterruptedAdds()
	checkUploadSettled(t, db, f, false)
}

// newImport puts a two-file book with a chosen cover in the import queue.
func newImport(t *testing.T) (*folderWatcher, *pendingImport) {
	t.Helper()
	w, dir := newTestWatcher(t)
	writeTestFile(t, filepath.Join(dir, "Go Book.epub"), importTestEPUB(t))
	writeTestFile(t, filepath.Join(dir, "Go Book.pdf"), []byte("%PDF-1.4 not really"))
	w.scan()
	w.scan()
	it := w.inbox.list()[0]
	cover, err := os.Create(filepath.Join(w.inbox.itemDir(it.ID), "cover.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	jpeg.Encode(cover, image.NewRGBA(image.Rect(0, 0, 4, 6)), nil)
	cover.Close()
	return w, it
}

func TestImportSurvivesACrashAtEveryStep(t *testing.T) {
	for _, step := range importSteps {
		t.Run(step, func(t *testing.T) {
			db := withTestLibrary(t)
			w, it := newImport(t)
			itemDir := w.inbox.itemDir(it.ID)
			meta := metadataFromRequest(ApplyMetadataRequest{
				Title: "The Go Programming Language", Creators: "Alan Donovan", Description: "<p>About Go.</p>",
			})
			stopAt(t, step, errSimulatedCrash)
			if _, err := w.inbox.approve(it.ID, meta, false); !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("approve: %v", err)
			}
			addStepHook = nil
			recoverInterruptedAdds()
			again, err := openInbox(w.inbox.dir) // the queue, as the next start reads it
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(addingDir()); err == nil {
				t.Errorf("%s is still there", addingDir())
			}

			if !committedAt(step) {
				if n := countRows(t, db, "books"); n != 0 {
					t.Errorf("%d book(s) in metadata.db, want none", n)
				}
				libraryHasNoBookFolders(t)
				back := again.get(it.ID)
				if back == nil {
					t.Fatal("the book isn't waiting in the queue again")
				}
				for _, name := range []string{back.Files[0].Name, back.Files[1].Name, "cover.jpg", "item.json"} {
					if !exists(filepath.Join(itemDir, name)) {
						t.Errorf("%s isn't back in the queue folder", name)
					}
				}
				return
			}

			if again.get(it.ID) != nil || exists(itemDir) {
				t.Error("the added book is still in the queue")
			}
			var id int64
			var path string
			var hasCover bool
			if err := db.QueryRow(`SELECT id, path, has_cover FROM books`).Scan(&id, &path, &hasCover); err != nil {
				t.Fatal(err)
			}
			if !hasCover || !exists(filepath.Join(baseDir, path, "cover.jpg")) {
				t.Error("the cover didn't come along")
			}
			var comment string
			db.QueryRow(`SELECT text FROM comments WHERE book = ?`, id).Scan(&comment)
			if comment != "<p>About Go.</p>" {
				t.Errorf("description %q: the editor's metadata wasn't applied", comment)
			}
			calibreFindsFiles(t, db)
		})
	}
}

func TestImportFailingAtAStepStaysInTheQueue(t *testing.T) {
	for _, step := range importSteps[:6] {
		t.Run(step, func(t *testing.T) {
			db := withTestLibrary(t)
			w, it := newImport(t)
			stopAt(t, step, errors.New("disk full"))
			meta := metadataFromRequest(ApplyMetadataRequest{Title: "Go", Creators: "Alan Donovan"})
			if _, err := w.inbox.approve(it.ID, meta, false); err == nil {
				t.Fatal("added anyway")
			}
			if n := countRows(t, db, "books"); n != 0 {
				t.Errorf("%d book(s) in metadata.db", n)
			}
			libraryHasNoBookFolders(t)
			if w.inbox.get(it.ID) == nil {
				t.Error("not back in the queue")
			}
			if _, err := loadPendingImport(w.inbox.itemDir(it.ID), it.ID); err != nil {
				t.Errorf("queue folder: %v", err)
			}
		})
	}
}
