package main

// Exchanging highlights with KOReader.
//
// KOReader's sync protocol (kosync.go) carries reading positions only, but
// KOReader has a file format of its own for moving annotations between
// devices (readerannotation.lua, since 2025): "<book file name>.annotations.lua",
// next to the book's settings or in a folder the user chooses. It writes one
// with "Export annotations" (or on closing a book), and when a book is opened
// it merges such a file in and deletes it.
//
// So there are two endpoints, for a file the user carries across (by hand,
// or with a folder synced to the device):
//
//	GET  /api/me/annotations/{uuid}/koreader?format=epub|pdf   the user's highlights, as that file
//	POST /api/me/annotations/{uuid}/koreader?format=epub|pdf   merges such a file in
//
// Positions are converted (koreader_text.go for EPUB; a PDF's are in points
// on both sides). What a file leaves out is never deleted here, and the file
// we write is dated so that KOReader, too, only adds and updates.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	koreaderDevice   = "CalibreFast" // KOReader skips a file with its own device id
	koreaderTime     = "2006-01-02 15:04:05"
	maxKOReaderFile  = 8 << 20
	koreaderIDPrefix = "ko-"
)

// koreaderOldest dates our files. On import KOReader removes its own
// annotations that are older than the file and not in it; nothing is older
// than this, so it only adds and updates.
const koreaderOldest = "1970-01-01 00:00:00"

// koBook is an EPUB being converted: chapters are parsed once.
type koBook struct {
	*epubBook
	chapters map[int]*chapter
	texts    map[int]*squeezed
}

func openKOBook(file string) (*koBook, error) {
	b, err := openEPUB(file)
	if err != nil {
		return nil, err
	}
	return &koBook{epubBook: b, chapters: map[int]*chapter{}, texts: map[int]*squeezed{}}, nil
}

func (k *koBook) chapter(item int) (*chapter, *squeezed, error) {
	if c := k.chapters[item]; c != nil {
		return c, k.texts[item], nil
	}
	if item < 0 || item >= len(k.itemrefs) || k.itemrefs[item] == "" {
		return nil, nil, errors.New("no such chapter")
	}
	data, err := k.readFile(k.itemrefs[item])
	if err != nil {
		return nil, nil, err
	}
	c, err := parseChapter(data)
	if err != nil {
		return nil, nil, err
	}
	k.chapters[item], k.texts[item] = c, c.squeeze()
	return c, k.texts[item], nil
}

func koreaderDate(ms int64) string { return time.UnixMilli(ms).Format(koreaderTime) }

// koreaderMillis reads KOReader's local date and time; 0 if it isn't one.
func koreaderMillis(s string) int64 {
	t, err := time.ParseInLocation(koreaderTime, s, time.Local)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// ---- export ----------------------------------------------------------------------

// koreaderFile writes annotations as KOReader's annotations file. skipped
// counts those whose place couldn't be converted.
func koreaderFile(file, format string, list []annotationRec) (lua string, skipped int) {
	var book *koBook
	if format == "epub" {
		var err error
		if book, err = openKOBook(file); err != nil {
			return "", len(list)
		}
		defer book.Close()
	}
	out := &luaTable{hash: map[string]any{"datetime": koreaderOldest, "device_id": koreaderDevice}}
	if format == "pdf" {
		out.hash["paging"] = true
	}
	items := &luaTable{hash: map[string]any{}}
	for _, a := range list {
		item := &luaTable{hash: map[string]any{
			"datetime": koreaderDate(a.CreatedAt),
			"drawer":   "lighten",
			"color":    a.Color,
			"text":     a.Text,
		}}
		if a.UpdatedAt > a.CreatedAt {
			item.hash["datetime_updated"] = koreaderDate(a.UpdatedAt)
		}
		if a.Note != "" {
			item.hash["note"] = a.Note
		}
		if a.Chapter != "" {
			item.hash["chapter"] = a.Chapter
		}
		if a.Page > 0 {
			item.hash["pageno"] = a.Page
		}
		ok := false
		if format == "epub" {
			ok = koreaderEPUBPlace(book, a.Anchor, item)
		} else {
			ok = koreaderPDFPlace(a.Anchor, item)
		}
		if !ok {
			skipped++
			continue
		}
		items.arr = append(items.arr, item)
	}
	out.hash["annotations"] = items
	var b strings.Builder
	fmt.Fprintf(&b, "-- %s: highlights from CalibreFast, for KOReader to import\n", filepath.Base(file))
	b.WriteString("return ")
	writeLua(&b, out, 0)
	b.WriteString("\n")
	return b.String(), skipped
}

// koreaderEPUBPlace sets an item's page, pos0 and pos1 from a CFI range.
func koreaderEPUBPlace(book *koBook, anchor string, item *luaTable) bool {
	idx, startPath, endPath, ok := splitCFI(anchor)
	if !ok {
		return false
	}
	c, _, err := book.chapter(idx)
	fragment, hasFragment := book.fragmentForItem(idx)
	if err != nil || !hasFragment {
		return false
	}
	start, ok1 := c.fromCFIPath(startPath)
	end, ok2 := c.fromCFIPath(endPath)
	if !ok1 || !ok2 {
		return false
	}
	pos0, ok1 := c.xpointer(fragment, start)
	pos1, ok2 := c.xpointer(fragment, end)
	if !ok1 || !ok2 {
		return false
	}
	item.hash["page"], item.hash["pos0"], item.hash["pos1"] = pos0, pos0, pos1
	item.hash["epubcfi"] = anchor // newer KOReader keeps one beside its XPointers
	return true
}

// pdfAnchor is a PDF annotation's place (web/annotations.ts: PdfAnchor):
// rectangles in points from the page's top left, as KOReader's are.
type pdfAnchor struct {
	Page  int          `json:"page"`
	Rects [][4]float64 `json:"rects"`
}

// koreaderPDFPlace sets an item's page, pos0, pos1 and pboxes. KOReader finds
// the words again from pos0 and pos1, points inside the first and last word.
func koreaderPDFPlace(anchor string, item *luaTable) bool {
	var a pdfAnchor
	if json.Unmarshal([]byte(anchor), &a) != nil || a.Page < 1 || len(a.Rects) == 0 {
		return false
	}
	pos := func(x, y float64) *luaTable {
		return &luaTable{hash: map[string]any{"page": a.Page, "rotation": 0, "zoom": 1, "x": x, "y": y}}
	}
	boxes := &luaTable{hash: map[string]any{}}
	for _, r := range a.Rects {
		boxes.arr = append(boxes.arr, &luaTable{hash: map[string]any{"x": r[0], "y": r[1], "w": r[2] - r[0], "h": r[3] - r[1]}})
	}
	first, last := a.Rects[0], a.Rects[len(a.Rects)-1]
	inset := func(r [4]float64) float64 { return math.Min(2, (r[2]-r[0])/2) }
	item.hash["page"] = a.Page
	item.hash["pos0"] = pos(first[0]+inset(first), (first[1]+first[3])/2)
	item.hash["pos1"] = pos(last[2]-inset(last), (last[1]+last[3])/2)
	item.hash["pboxes"] = boxes
	return true
}

func koreaderExportHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	file, err := bookFilePath(uuid, format)
	if !validFormat(format) || err != nil {
		http.Error(w, "this book has no such format", http.StatusNotFound)
		return
	}
	rows, err := appDB.Query(`SELECT `+annotationCols+` FROM annotations
		WHERE user_id = ? AND book_uuid = ? AND format = ? AND deleted = 0 ORDER BY created_at, client_id`, u.ID, uuid, format)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	var list []annotationRec
	for rows.Next() {
		if a, err := scanAnnotation(rows); err == nil {
			list = append(list, a)
		}
	}
	rows.Close()
	lua, skipped := koreaderFile(file, format, list)
	if lua == "" {
		http.Error(w, "could not read the book", http.StatusInternalServerError)
		return
	}
	// KOReader looks for exactly "<book file name>.annotations.lua".
	name := filepath.Base(file) + ".annotations.lua"
	w.Header().Set("Content-Type", "text/x-lua; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Annotations-Exported", fmt.Sprint(len(list)-skipped))
	w.Header().Set("X-Annotations-Skipped", fmt.Sprint(skipped))
	_, _ = io.WriteString(w, lua)
}

// ---- import ----------------------------------------------------------------------

// koreaderImport is what an import did.
type koreaderImport struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Skipped   int `json:"skipped"`   // highlights whose place wasn't found in this copy of the book
	Bookmarks int `json:"bookmarks"` // page bookmarks, which aren't highlights
}

// koreaderAnnotations reads a KOReader annotations file into annotations
// for a book file. Their ids are derived from KOReader's own identity for a
// highlight (its place and creation time), so importing a file again
// doesn't duplicate anything.
func koreaderAnnotations(file, format string, t *luaTable) (recs []annotationRec, res koreaderImport, err error) {
	list := t.table("annotations")
	if list == nil {
		return nil, res, errors.New("no annotations in this file: export them from KOReader's bookmark list (Export annotations)")
	}
	if paging, _ := t.hash["paging"].(bool); paging != (format == "pdf") {
		if paging {
			return nil, res, errors.New("this file is for a PDF or another fixed-page book, not for the EPUB")
		}
		return nil, res, errors.New("this file is for an EPUB or another reflowable book, not for the PDF")
	}
	var book *koBook
	if format == "epub" {
		if book, err = openKOBook(file); err != nil {
			return nil, res, errors.New("could not read the book")
		}
		defer book.Close()
	}
	now := time.Now().UnixMilli()
	for _, v := range list.arr {
		item, ok := v.(*luaTable)
		if !ok {
			continue
		}
		if item.str("drawer") == "" {
			res.Bookmarks++ // a page bookmark
			continue
		}
		var anchor, identity string
		if format == "epub" {
			anchor, identity = koreaderEPUBAnchor(book, item)
		} else {
			anchor, identity = koreaderPDFAnchor(item)
		}
		if anchor == "" || len(anchor) > maxAnchorLen {
			res.Skipped++
			continue
		}
		a := annotationRec{
			Format:  format,
			Anchor:  anchor,
			Text:    clip(item.str("text"), maxAnnotationText),
			Note:    clip(item.str("note"), maxAnnotationText),
			Color:   item.str("color"),
			Chapter: clip(strings.TrimSpace(item.str("chapter")), maxLabelLen),
		}
		if !slices.Contains(annotationColors, a.Color) {
			a.Color = annotationColors[0]
		}
		if n, ok := item.num("pageno"); ok && n > 0 && n < 1e7 {
			a.Page = int(n)
		}
		a.CreatedAt = koreaderMillis(item.str("datetime"))
		a.UpdatedAt = koreaderMillis(item.str("datetime_updated"))
		if a.CreatedAt <= 0 || a.CreatedAt > now {
			a.CreatedAt = now
		}
		if a.UpdatedAt < a.CreatedAt || a.UpdatedAt > now {
			a.UpdatedAt = a.CreatedAt
		}
		sum := sha256.Sum256([]byte(format + "\x00" + identity + "\x00" + item.str("datetime")))
		a.ID = koreaderIDPrefix + hex.EncodeToString(sum[:20])
		recs = append(recs, a)
	}
	return recs, res, nil
}

// clip shortens s to at most max bytes, on a character boundary.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// koreaderEPUBAnchor is the CFI range of a KOReader highlight in an EPUB
// ("" when it can't be found), and what identifies the highlight to KOReader.
//
// Newer KOReader versions save the CFI themselves. Otherwise the XPointers
// are followed; if the text between them isn't the highlighted text (the
// two readers disagree about a chapter's markup now and then), the text is
// looked for near where they point.
func koreaderEPUBAnchor(book *koBook, item *luaTable) (anchor, identity string) {
	pos0, pos1 := item.str("pos0"), item.str("pos1")
	if pos0 == "" {
		pos0 = item.str("page")
	}
	if pos1 == "" {
		pos1 = pos0
	}
	identity = pos0 + "\x00" + pos1

	if cfi := item.str("epubcfi"); cfi != "" {
		if idx, startPath, endPath, ok := splitCFI(cfi); ok && startPath != endPath {
			if c, _, err := book.chapter(idx); err == nil {
				_, ok1 := c.fromCFIPath(startPath)
				_, ok2 := c.fromCFIPath(endPath)
				if ok1 && ok2 {
					return cfi, identity
				}
			}
		}
	}

	fragment, ok := xpointerFragment(pos0)
	if !ok {
		return "", identity
	}
	idx, ok := book.itemForFragment(fragment)
	if !ok {
		return "", identity
	}
	c, sq, err := book.chapter(idx)
	if err != nil {
		return "", identity
	}
	start, exact0, ok0 := c.fromXPointer(pos0)
	end, exact1, ok1 := c.fromXPointer(pos1)
	near := 0
	if ok0 {
		near = sq.index(start)
	}
	from, to := -1, -1
	if ok0 && ok1 {
		from, to = sq.index(start), sq.index(end)
		if from > to { // selected backwards
			from, to = to, from
		}
	}
	needle := squeezeString(item.str("text"))
	matches := exact0 && exact1 && from >= 0 && from < to && slices.Equal(sq.text[from:to], needle)
	if !matches && len(needle) > 0 {
		if at := sq.find(needle, near); at >= 0 {
			from, to, matches = at, at+len(needle), true
		}
	}
	// Text that was edited in KOReader won't be found: trust exact positions.
	if !matches && !(exact0 && exact1 && from >= 0 && from < to) {
		return "", identity
	}
	a, b := sq.points(from, to)
	return cfiRange(book.spineStep, idx, a, b), identity
}

// koreaderPDFAnchor is the place of a KOReader highlight in a PDF, as JSON
// ("" when it has none), and what identifies the highlight to KOReader.
func koreaderPDFAnchor(item *luaTable) (anchor, identity string) {
	pos0, pos1 := item.table("pos0"), item.table("pos1")
	page, _ := item.num("page")
	if page < 1 && pos0 != nil {
		page, _ = pos0.num("page")
	}
	xy := func(t *luaTable) (x, y float64, ok bool) {
		if t == nil {
			return 0, 0, false
		}
		x, ok1 := t.num("x")
		y, ok2 := t.num("y")
		return x, y, ok1 && ok2
	}
	x0, y0, ok0 := xy(pos0)
	x1, y1, ok1 := xy(pos1)
	identity = fmt.Sprintf("%v\x00%v,%v\x00%v,%v", page, x0, y0, x1, y1)
	if page < 1 || page > 1e6 {
		return "", identity
	}
	round := func(f float64) float64 { return math.Round(f*10) / 10 }
	a := pdfAnchor{Page: int(page)}
	if boxes := item.table("pboxes"); boxes != nil {
		for _, v := range boxes.arr {
			b, _ := v.(*luaTable)
			if b == nil {
				continue
			}
			x, y, ok := xy(b)
			w, _ := b.num("w")
			h, _ := b.num("h")
			if ok && w > 0 && h > 0 && len(a.Rects) < 200 {
				a.Rects = append(a.Rects, [4]float64{round(x), round(y), round(x + w), round(y + h)})
			}
		}
	}
	if len(a.Rects) == 0 && ok0 && ok1 {
		// No boxes saved: one around the two ends, about a line high.
		a.Rects = append(a.Rects, [4]float64{
			round(math.Min(x0, x1)), round(math.Min(y0, y1) - 6), round(math.Max(x0, x1)), round(math.Max(y0, y1) + 6),
		})
	}
	if len(a.Rects) == 0 {
		return "", identity
	}
	out, _ := json.Marshal(a)
	return string(out), identity
}

func koreaderImportHandler(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	uuid, ok := requireBook(w, r)
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	file, err := bookFilePath(uuid, format)
	if !validFormat(format) || err != nil {
		http.Error(w, "this book has no such format", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKOReaderFile))
	if err != nil {
		http.Error(w, "the file is too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !utf8.Valid(body) {
		http.Error(w, "not a KOReader annotations file", http.StatusBadRequest)
		return
	}
	table, err := parseLuaTable(string(body))
	if err != nil {
		http.Error(w, "could not read the file: "+err.Error(), http.StatusBadRequest)
		return
	}
	recs, res, err := koreaderAnnotations(file, format, table)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// What is stored already: by id (tombstones too, so a highlight deleted
	// here isn't brought back by the same file), and by place, so one that
	// was made here, exported and comes back from KOReader isn't doubled.
	type stored struct {
		id        string
		updatedAt int64
		deleted   bool
	}
	byID, byAnchor := map[string]stored{}, map[string]stored{}
	live := 0
	rows, err := appDB.Query(`SELECT client_id, anchor, updated_at, deleted FROM annotations
		WHERE user_id = ? AND book_uuid = ? AND format = ?`, u.ID, uuid, format)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var s stored
		var anchor string
		if rows.Scan(&s.id, &anchor, &s.updatedAt, &s.deleted) == nil {
			byID[s.id] = s
			if !s.deleted {
				byAnchor[anchor] = s
				live++
			}
		}
	}
	rows.Close()

	for _, a := range recs {
		have, exists := byID[a.ID]
		if !exists {
			have, exists = byAnchor[a.Anchor]
		}
		switch {
		case !exists:
			if live >= maxAnnotationsBook {
				res.Skipped++
				continue
			}
			live++
			res.Added++
		case a.UpdatedAt > have.updatedAt:
			a.ID = have.id
			res.Updated++
		default:
			res.Unchanged++ // what is here is as new, or was deleted here since
			continue
		}
		if err := storeAnnotation(u.ID, uuid, a.ID, a); err != nil {
			http.Error(w, "could not save", http.StatusInternalServerError)
			return
		}
		byID[a.ID] = stored{a.ID, a.UpdatedAt, false}
		byAnchor[a.Anchor] = byID[a.ID]
	}
	writeJSON(w, http.StatusOK, res)
}
