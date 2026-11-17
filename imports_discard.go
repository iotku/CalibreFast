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
		check, err := findDuplicates(calibreDB, baseDir, it.Merged.Title, it.Merged.Creators, imports.dupFiles(it), "")
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", it.Merged.Title, err))
			continue
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
			continue
		}
		if len(same) == len(it.Files) {
			if err := imports.discard(it.ID); err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", it.Merged.Title, err))
				}
				continue
			}
			logEdit(by, "discard import", logKindImport, it.ID, it.Merged.Title, nil, nil)
			res.Discarded++
			res.Files += len(it.Files)
			continue
		}
		into := ""
		if len(holders) == 1 {
			into = holders[0]
		}
		off, err := imports.splitOff(it.ID, same, into)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", it.Merged.Title, err))
			}
			continue
		}
		if err := imports.discard(off.ID); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%q: %v", it.Merged.Title, err))
			continue
		}
		logEdit(by, "discard import", logKindImport, off.ID, off.Merged.Title+" ("+fileNames(off.Files)+")", nil, nil)
		res.Split++
		res.Files += len(off.Files)
	}
	if res.Discarded+res.Split > 0 {
		log.Printf("imports: %d identical files discarded by %s", res.Files, by.Username)
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

func writeItemJSON(dir string, it *pendingImport) error {
	data, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "item.json"), data, 0o600)
}
