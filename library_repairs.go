package main

// The Library repairs page (/admin/repairs, web/library-repairs.ts): fixes
// for metadata.db that a library shouldn't need, for books added by older
// versions of CalibreFast or changed outside Calibre. Nothing runs by itself:
// checking can stat every book file, which is slow on a big library, and
// most libraries have nothing to fix. An admin checks, sees what would
// change, and repairs. Only with -allow-writes; only the database is
// modified, never the files.
//
//   - Format file names (calibre_names.go: repairFormatNames): data.name
//     isn't the file's name, so Calibre can't find the file.
//   - Files metadata.db doesn't list (addUnlistedFormats): a book with no
//     data rows whose folder holds book files. The bulk editor shows these
//     too, and repairs them with POST /api/library/repair-formats.

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

func registerLibraryRepairRoutes() {
	http.HandleFunc("GET /admin/repairs", requireWrites(libraryRepairsPage))
	http.HandleFunc("GET /api/admin/repairs", requireWrites(libraryRepairsCheck))
	http.HandleFunc("POST /api/admin/repairs/format-names", requireWrites(repairFormatNamesHandler))
}

func libraryRepairsPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	logErr(templates.ExecuteTemplate(w, "library-repairs.html", PageData{Title: SiteTitle}), "render library-repairs")
}

// repairCheck is what the repairs would change (TS: RepairCheck in web/types.ts).
type repairCheck struct {
	Misnamed []misnamedJSON `json:"misnamed"`
	Unlisted []unlistedJSON `json:"unlisted"`
}

type misnamedJSON struct {
	UUID   string `json:"uuid"`
	Title  string `json:"title"`
	Format string `json:"format"` // "EPUB"
	From   string `json:"from"`   // data.name now
	To     string `json:"to"`     // the file's name
}

type unlistedJSON struct {
	UUID      string   `json:"uuid"`
	Title     string   `json:"title"`
	Formats   []string `json:"formats"`   // rows the repair would add
	Ambiguous []string `json:"ambiguous"` // formats with several files: left alone
}

func libraryRepairsCheck(w http.ResponseWriter, r *http.Request) {
	bulkMu.Lock()
	misnamed, err := findMisnamedFormats(calibreDB, baseDir)
	var unlisted []unlistedBook
	if err == nil {
		unlisted, err = findUnlistedFormats(calibreDB, baseDir, nil)
	}
	bulkMu.Unlock()
	if err != nil {
		log.Println("checking the library:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res := repairCheck{Misnamed: []misnamedJSON{}, Unlisted: []unlistedJSON{}}
	for _, m := range misnamed {
		res.Misnamed = append(res.Misnamed, misnamedJSON{m.uuid, m.title, m.format, m.from, m.to})
	}
	for _, b := range unlisted {
		u := unlistedJSON{UUID: b.uuid, Title: b.title, Formats: []string{}, Ambiguous: []string{}}
		for _, f := range b.add {
			u.Formats = append(u.Formats, f.format)
		}
		u.Ambiguous = append(u.Ambiguous, b.ambiguous...)
		res.Unlisted = append(res.Unlisted, u)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(res), "encode repair check")
}

func repairFormatNamesHandler(w http.ResponseWriter, r *http.Request) {
	bulkMu.Lock()
	n, err := repairFormatNames(calibreDB, baseDir)
	bulkMu.Unlock()
	if errors.Is(err, errReadOnly) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err != nil {
		log.Println("repairing format file names:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if n > 0 {
		log.Printf("repair format names: %s pointed %d format(s) at their files", currentUser(r).Username, n)
		refreshAllCaches()
	}
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(map[string]int{"repaired": n}), "encode format name repair")
}
