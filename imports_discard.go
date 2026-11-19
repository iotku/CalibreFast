package main

// Discard Existing Files (/import): every file in the queue that the library
// already holds byte for byte (duplicates.go) is discarded at once, instead
// of one book at a time.
//
// A waiting book whose files are all in the library is discarded whole. One
// with only some of them (an EPUB that's there and a PDF that isn't) loses
// just those: they're split off into a book of their own and that one is
// discarded, so each discard can be undone (editlog.go) like any other. What
// stays is new files for the library book that had the others, when that's
// a single book: reviewed as such (add_format.go).

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
	"time"

	"github.com/google/uuid"
)

// discardExistingResult is POST /api/imports/discard-existing (TS:
// DiscardExistingResult).
type discardExistingResult struct {
	Discarded int      `json:"discarded"` // books discarded whole
	Split     int      `json:"split"`     // books that lost only some files
	Files     int      `json:"files"`     // files discarded, in all
	Errors    []string `json:"errors,omitempty"`
}

func importDiscardExistingHandler(w http.ResponseWriter, r *http.Request) {
	by := currentUser(r)
	res := discardExistingResult{}
	for _, it := range imports.list() {
		if it.Into != "" {
			continue // new files for a book: reviewed against it
		}
		out, err := discardIdentical(it, it.Merged.Title, it.Merged.Creators, by)
		switch {
		case err != nil:
			if !errors.Is(err, fs.ErrNotExist) {
				res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", it.Merged.Title, err))
			}
		case out.Kept != nil:
			res.Split++
		case out.Files > 0:
			res.Discarded++
		}
		res.Files += out.Files
	}
	if res.Discarded+res.Split > 0 {
		log.Printf("imports: %d identical files discarded by %s", res.Files, by.Username)
	}
	writeJSON(w, http.StatusOK, res)
}

// identicalDiscarded is what discardIdentical did with one waiting book.
type identicalDiscarded struct {
	Files int            // discarded: none if it had no identical file
	Kept  *pendingImport // its other files, still waiting; nil if it went whole
}

// discardIdentical discards waiting book it's files that the library holds
// byte for byte (looked for among books titled title, by authors): it
// whole, or only those, split off, with the rest left as new files for the
// one library book that holds them (if one does).
func discardIdentical(it *pendingImport, title string, authors []string, by *User) (identicalDiscarded, error) {
	check, err := findDuplicates(calibreDB, baseDir, title, authors, imports.dupFiles(it), "")
	if err != nil {
		return identicalDiscarded{}, err
	}
	same := map[string]bool{}
	var holders []string // the library books holding them
	for _, d := range check.Books {
		for _, f := range d.SameFiles {
			same[f] = true
		}
		if len(d.SameFiles) > 0 && !slices.Contains(holders, d.UUID) {
			holders = append(holders, d.UUID)
		}
	}
	if len(same) == 0 {
		return identicalDiscarded{}, nil
	}
	if len(same) == len(it.Files) {
		if err := imports.discard(it.ID); err != nil {
			return identicalDiscarded{}, err
		}
		logEdit(by, "discard import", logKindImport, it.ID, it.Merged.Title, nil, nil)
		return identicalDiscarded{Files: len(it.Files)}, nil
	}
	into := ""
	if len(holders) == 1 {
		into = holders[0]
	}
	off, err := imports.splitOff(it.ID, same, into)
	if err != nil {
		return identicalDiscarded{}, err
	}
	if err := imports.discard(off.ID); err != nil {
		return identicalDiscarded{}, err
	}
	logEdit(by, "discard import", logKindImport, off.ID, off.Merged.Title+" ("+fileNames(off.Files)+")", nil, nil)
	return identicalDiscarded{Files: len(off.Files), Kept: imports.get(it.ID)}, nil
}

// importDiscardIdenticalHandler is POST /import/{id}/discard-identical:
// discardIdentical for one book, from its review page, with the title and
// authors the editor has (the check found the identical file by them).
// The answer says where its other files are now reviewed.
func importDiscardIdenticalHandler(w http.ResponseWriter, r *http.Request) {
	it := importFromRequest(w, r)
	if it == nil {
		return
	}
	var req struct {
		Title    string `json:"title"`
		Creators string `json:"creators"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req)
	title, authors := it.Merged.Title, it.Merged.Creators
	if strings.TrimSpace(req.Title) != "" {
		title, authors = req.Title, parseCreatorsString(req.Creators)
	}
	out, err := discardIdentical(it, title, authors, currentUser(r))
	if err != nil {
		http.Error(w, "Couldn't discard the files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	res := map[string]any{"discarded": out.Files}
	switch {
	case out.Kept != nil && out.Kept.Into != "":
		res["url"] = addFormatPath(out.Kept)
	case out.Kept != nil:
		res["url"] = "/import/" + out.Kept.ID
	default:
		res["url"] = "/import"
	}
	writeJSON(w, http.StatusOK, res)
}

func fileNames(files []importFile) string {
	out := ""
	for i, f := range files {
		if i > 0 {
			out += ", "
		}
		out += f.Name
	}
	return out
}

// splitOff moves the files named in names out of waiting book id into a new
// waiting book of their own, which it returns. What stays in id is new
// files for library book into, unless into is "". Both keep their metadata
// as their files say it.
func (b *importInbox) splitOff(id string, names map[string]bool, into string) (*pendingImport, error) {
	it := b.take(id)
	if it == nil {
		return nil, fs.ErrNotExist
	}
	keep, off := *it, &pendingImport{ID: uuid.New().String(), Added: time.Now().UTC(), From: it.From}
	keep.Files, keep.Sources = nil, nil
	for i, f := range it.Files {
		var src *UploadedBookMeta
		if i < len(it.Sources) {
			src = it.Sources[i]
		}
		if names[f.Name] {
			off.Files = append(off.Files, f)
			if src != nil {
				off.Sources = append(off.Sources, src)
			}
		} else {
			keep.Files = append(keep.Files, f)
			if src != nil {
				keep.Sources = append(keep.Sources, src)
			}
		}
	}
	if len(off.Files) == 0 || len(keep.Files) == 0 {
		b.put(it)
		return nil, errors.New("nothing to split off")
	}
	remerge := func(p *pendingImport) {
		if len(p.Sources) > 0 {
			p.Merged = *mergeMeta(p.Sources)
			p.Merged.Format, p.Merged.FileName = "current", ""
		}
	}
	remerge(off)
	remerge(&keep)
	if ph := placeholderByUUID(it.Placeholder); ph != nil {
		keep.Merged.OPFMetadata = withPlaceholder(keep.Merged.OPFMetadata, ph)
	}
	keep.Into = into

	src, dst := b.itemDir(id), b.itemDir(off.ID)
	if err := os.Mkdir(dst, 0o700); err != nil {
		b.put(it)
		return nil, err
	}
	var moved []string
	undo := func(err error) (*pendingImport, error) {
		for _, n := range moved {
			if mErr := moveFile(filepath.Join(dst, n), filepath.Join(src, n)); mErr != nil {
				log.Printf("imports: couldn't move %s back: %v", n, mErr)
			}
		}
		os.Remove(filepath.Join(dst, "item.json"))
		os.Remove(dst)
		b.put(it)
		return nil, err
	}
	for _, f := range off.Files {
		if err := moveFile(filepath.Join(src, f.Name), filepath.Join(dst, f.Name)); err != nil {
			return undo(err)
		}
		moved = append(moved, f.Name)
	}
	if err := writeItemJSON(dst, off); err != nil {
		return undo(err)
	}
	if err := writeItemJSON(src, &keep); err != nil {
		return undo(err)
	}
	b.put(&keep)
	b.put(off)
	return off, nil
}

// writeItemJSON writes a waiting book's item.json whole or not at all
// (a temporary file, synced, then renamed over it): a crash part way
// through never leaves one that can't be read, which would have the book
// skipped at startup.
func writeItemJSON(dir string, it *pendingImport) error {
	data, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".item-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing left once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "item.json"))
}
