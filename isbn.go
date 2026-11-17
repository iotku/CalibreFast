package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/iotku/CalibreFast/pkg/isbn"
)

// Finding ISBNs in a book's own files, for the metadata editor's "Find
// ISBNs" (/metadata/{uuid}/isbns), is in pkg/isbn. Nothing here writes: the
// editor lists the candidates and the admin picks one, which is saved like
// any other edit.

// isbnCandidatesHandler: GET looks in the book's EPUB, MOBI and AZW3 files;
// POST does the same and also parses what the editor read from its PDF.
func isbnCandidatesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uuid := r.PathValue("uuid")
	bookPath, err := resolveBookPath(uuid)
	if err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
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
	resp := isbn.FindInBook(filepath.Join(baseDir, bookPath), resolveFormats(uuid, baseDir, bookPath), pdf)
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(resp), "could not encode ISBN candidates")
}
