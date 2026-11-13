package main

// Adding new files (formats) to a book already in the library, from its
// book page (admins).
//
// POST /book/add-format/{uuid} takes the files. Each must be a book file,
// one per format; one byte for byte the same as the book's own file in its
// format is left out (there's nothing to add). The rest wait in the import
// queue (imports.go) as an item with Into set, so an upload left unreviewed
// is still listed at /import and never lost.
//
// GET /book/add-format/{uuid}/{id} is the merge editor (merge.go) with the
// book and the new files as the two "books": the metadata may be taken from
// what the new files say, and where the book already has a file in a format,
// the admin picks which one it keeps (a book has one file per format, as in
// Calibre). It also lists books the new files may belong to instead: those
// with the same title (and authors) or ISBN, and above all one holding an
// identical file, which is offered to be merged into this book.
//
// POST /book/add-format/{uuid}/{id}/apply is mergeInto: the files go in, a
// file replaced goes to the Recycling Bin with readers' bookmarks and
// highlights made in it (they'd be in the wrong places in another file;
// places are kept), and the item leaves the queue.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func addFormatPath(it *pendingImport) string {
	return "/book/add-format/" + it.Into + "/" + it.ID
}

func registerAddFormatRoutes() {
	http.HandleFunc("POST /book/add-format/{uuid}", requireWrites(addFormatUploadHandler))
	http.HandleFunc("GET /book/add-format/{uuid}/{id}", requireWrites(addFormatPage))
	http.HandleFunc("GET /book/add-format/{uuid}/{id}/metadata", requireWrites(addFormatMetadataHandler))
	http.HandleFunc("POST /book/add-format/{uuid}/{id}/apply", requireWrites(addFormatApplyHandler))
}

// incomingMergeBook is the new files waiting in the queue, as a merge's
// "book".
func incomingMergeBook(it *pendingImport) *mergeBook {
	dir := imports.itemDir(it.ID)
	b := &mergeBook{Letter: "New", UUID: incomingUUID, Import: it.ID, Title: it.Merged.Title,
		Authors: strings.Join(it.Merged.Creators, " & "), Dir: dir, Files: map[string]string{}}
	for _, f := range it.Files {
		b.Files[f.Format] = filepath.Join(dir, f.Name)
	}
	if st, err := os.Stat(filepath.Join(dir, "cover.jpg")); err == nil {
		b.HasCover = true
		b.CoverModTime = st.ModTime().UnixNano()
	}
	return b
}

// addFormatBooks are the book and its waiting new files, for the item in
// the request's path.
func addFormatBooks(w http.ResponseWriter, r *http.Request) (keep, incoming *mergeBook, it *pendingImport, ok bool) {
	u, id := r.PathValue("uuid"), r.PathValue("id")
	if isSafeUUIDComponent(id) {
		it = imports.get(id)
	}
	if it == nil || it.Into != u {
		http.Error(w, "these files aren't waiting to be added to this book (already added or discarded?)", http.StatusNotFound)
		return nil, nil, nil, false
	}
	keep, err := loadMergeBook(u, "A")
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return nil, nil, nil, false
	}
	return keep, incomingMergeBook(it), it, true
}

// incomingDuplicates are the library books other than the one it's for
// that the new files may belong to: same title (and authors), same ISBN, or
// an identical file.
func incomingDuplicates(it *pendingImport) *duplicateCheck {
	var isbns []string
	for _, s := range append([]*UploadedBookMeta{&it.Merged}, it.Sources...) {
		for _, id := range s.Identifiers {
			if n := identifierISBN(id.Scheme, id.Value); n != "" {
				isbns = append(isbns, n)
			}
		}
	}
	check, err := findDuplicatesISBN(calibreDB, baseDir, it.Merged.Title, it.Merged.Creators, imports.dupFiles(it), it.Into, isbns)
	if err != nil {
		logErr(err, "add format: duplicate check for "+it.ID)
		return nil
	}
	return check
}

func addFormatUploadHandler(w http.ResponseWriter, r *http.Request) {
	keep, err := loadMergeBook(r.PathValue("uuid"), "A")
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "couldn't read the upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files", http.StatusBadRequest)
		return
	}
	tmp, err := os.MkdirTemp("", "calibrefast-add-format-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmp)

	seen := map[string]bool{}
	var paths, same []string
	for _, fh := range files {
		name := filepath.Base(strings.ReplaceAll(fh.Filename, `\`, "/"))
		format := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
		if !hasBookExt("." + format) {
			http.Error(w, fmt.Sprintf("%s isn't a book file this library takes (EPUB, PDF, MOBI, AZW3, DJVU)", name), http.StatusBadRequest)
			return
		}
		if seen[format] {
			http.Error(w, fmt.Sprintf("more than one %s: a book has one file per format", strings.ToUpper(format)), http.StatusBadRequest)
			return
		}
		seen[format] = true
		if strings.HasPrefix(name, ".") || name == "" {
			name = "book." + format
		}
		p := filepath.Join(tmp, name)
		if err := saveMultipartFile(fh, p); err != nil {
			http.Error(w, "couldn't save "+name+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		if old, ok := keep.Files[format]; ok && fileHash(old) != "" && fileHash(old) == fileHash(p) {
			same = append(same, strings.ToUpper(format))
			continue
		}
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("the book already has this %s, byte for byte", strings.Join(same, " and ")),
			"same":  same,
		})
		return
	}
	it, err := imports.stage(paths, "", keep.UUID)
	if err != nil {
		http.Error(w, "couldn't keep the files for review: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("add format: %s uploaded %d file(s) for %q (%s), waiting at %s", currentUser(r).Username, len(it.Files), keep.Title, keep.UUID, addFormatPath(it))
	writeJSON(w, http.StatusOK, map[string]any{"id": it.ID, "url": addFormatPath(it), "same": same})
}

func saveMultipartFile(fh *multipart.FileHeader, dst string) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func addFormatPage(w http.ResponseWriter, r *http.Request) {
	keep, incoming, it, ok := addFormatBooks(w, r)
	if !ok {
		return
	}
	// The new files are what was asked for: chosen over the book's own.
	chosen := map[string]string{}
	for f := range incoming.Files {
		chosen[f] = incomingUUID
	}
	d := newMergePageData([]*mergeBook{keep, incoming}, keep.UUID, "", chosen)
	d.Incoming = it.ID
	for _, f := range it.Files {
		d.IncomingFiles = append(d.IncomingFiles, f.Name)
	}
	data := EditBookPageData{
		UUID:         keep.UUID,
		CoverModTime: keep.CoverModTime,
		ReadLink:     previewReadLink("uuid", keep.UUID, keep.FormatList()),
		Lookups:      !noLookups,
		GoogleBooks:  googleBooksEnabled(),
		Merge:        d,
		Duplicates:   incomingDuplicates(it),
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, "book-edit.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func addFormatMetadataHandler(w http.ResponseWriter, r *http.Request) {
	keep, incoming, _, ok := addFormatBooks(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, mergeSources([]*mergeBook{keep, incoming}, keep.UUID))
}

func addFormatApplyHandler(w http.ResponseWriter, r *http.Request) {
	var req mergeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	{
		meta := metadataFromRequest(req.ApplyMetadataRequest)
		if _, _, err := cleanSeries(meta.Series, meta.SeriesIndex); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	u, id := r.PathValue("uuid"), r.PathValue("id")
	var it *pendingImport
	if isSafeUUIDComponent(id) {
		it = imports.take(id) // no one else adds or discards it meanwhile
	}
	if it == nil {
		http.Error(w, "these files were just added or discarded", http.StatusConflict)
		return
	}
	if it.Into != u {
		imports.put(it)
		http.Error(w, "these files aren't waiting to be added to this book", http.StatusNotFound)
		return
	}
	// A file the library already holds in another book isn't added twice:
	// that book is this one, and is merged into it instead.
	if dup := incomingDuplicates(it); dup.SameFile() {
		imports.put(it)
		writeJSON(w, http.StatusConflict, map[string]any{"error": (&duplicateError{dup}).Error(), "duplicate": dup})
		return
	}

	res, err := addFormats(u, it, &req, currentUser(r).Username)
	if res == nil {
		imports.put(it)
		status := http.StatusInternalServerError
		if errors.Is(err, errNoSuchBook) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	if rmErr := os.RemoveAll(imports.itemDir(it.ID)); rmErr != nil {
		log.Printf("add format: couldn't remove %s: %v", imports.itemDir(it.ID), rmErr)
	}
	out := map[string]any{"uuid": res.UUID, "title": res.Title, "batch": res.Batch, "success": err == nil}
	if err != nil {
		out["warning"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// addFormats adds the waiting new files to the book uuid (mergeInto), with
// the editor's metadata and choices: req.Formats names, per format, the
// book's own uuid to keep its file instead of the new one.
func addFormats(uuid string, it *pendingImport, req *mergeRequest, by string) (*mergeResult, error) {
	bulkMu.Lock()
	defer bulkMu.Unlock()
	keep, err := loadMergeBook(uuid, "A")
	if err != nil {
		return nil, err
	}
	incoming := incomingMergeBook(it)
	// A new file unless the book's own is asked for.
	formats := map[string]string{}
	for f := range incoming.Files {
		if req.Formats[f] == keep.UUID {
			formats[f] = keep.UUID
		} else {
			formats[f] = incomingUUID
		}
	}
	req.Formats, req.Keep = formats, keep.UUID
	if req.Cover != incomingUUID {
		req.Cover = ""
	}
	return mergeInto([]*mergeBook{keep, incoming}, req, by)
}
