package koreader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// TimeLayout is how KOReader writes dates and times (local time).
	TimeLayout = "2006-01-02 15:04:05"
	// IDPrefix starts the ids Annotations makes for KOReader's highlights.
	IDPrefix = "ko-"
	// Oldest dates the files File writes. On import KOReader removes its
	// own annotations that are older than the file and not in it; nothing is
	// older than this, so it only adds and updates.
	Oldest = "1970-01-01 00:00:00"
)

// Highlight is a highlight, perhaps with a note, as File writes and
// Annotations reads it.
type Highlight struct {
	ID string
	// Anchor is where it is, in a form that survives zoom and reflow:
	//   EPUB: an EPUB CFI range, "epubcfi(/6/8!/4/2,/1:0,/1:42)"
	//   PDF:  {"page":3,"rects":[[x0,y0,x1,y1],…]} in PDF points from the page's top left
	Anchor    string
	Text      string // the highlighted text
	Note      string
	Color     string // KOReader's colour name: "yellow", "red", …
	Chapter   string // the chapter's title when it was made
	Page      int    // the page number shown when it was made
	CreatedAt int64  // Unix ms
	UpdatedAt int64  // Unix ms
}

// Options configure File and Annotations. Every field is optional.
type Options struct {
	// DeviceID is the file's device_id. KOReader skips a file with its own
	// device's id, so it must not be one. Default "koreader-go".
	DeviceID string
	// Source names the program in the file's first-line comment.
	Source string
	// Limits on what Annotations keeps, in bytes: a longer anchor skips the
	// highlight; longer text, notes and chapter titles are clipped. 0: no
	// limit.
	MaxAnchorLen, MaxTextLen, MaxLabelLen int
	// Colors are the colours to keep; others become Colors[0]. Empty: keep
	// any.
	Colors []string
	// Now is the clock, for dates in the future. Default time.Now.
	Now func() time.Time
}

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

func koreaderDate(ms int64) string { return time.UnixMilli(ms).Format(TimeLayout) }

// Millis reads KOReader's local date and time (TimeLayout) as Unix ms; 0 if
// it isn't one.
func Millis(s string) int64 {
	t, err := time.ParseInLocation(TimeLayout, s, time.Local)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// ---- export ----------------------------------------------------------------------

// File writes highlights in a book file (file, of format "epub" or "pdf") as
// KOReader's annotations file, "<book file name>.annotations.lua". skipped
// counts those whose place couldn't be converted. lua is "" if the book
// couldn't be read.
func File(file, format string, list []Highlight, opts Options) (lua string, skipped int) {
	var book *koBook
	if format == "epub" {
		var err error
		if book, err = openKOBook(file); err != nil {
			return "", len(list)
		}
		defer book.Close()
	}
	out := &LuaTable{Hash: map[string]any{"datetime": Oldest, "device_id": opts.deviceID()}}
	if format == "pdf" {
		out.Hash["paging"] = true
	}
	items := &LuaTable{Hash: map[string]any{}}
	for _, a := range list {
		item := &LuaTable{Hash: map[string]any{
			"datetime": koreaderDate(a.CreatedAt),
			"drawer":   "lighten",
			"color":    a.Color,
			"text":     a.Text,
		}}
		if a.UpdatedAt > a.CreatedAt {
			item.Hash["datetime_updated"] = koreaderDate(a.UpdatedAt)
		}
		if a.Note != "" {
			item.Hash["note"] = a.Note
		}
		if a.Chapter != "" {
			item.Hash["chapter"] = a.Chapter
		}
		if a.Page > 0 {
			item.Hash["pageno"] = a.Page
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
		items.Arr = append(items.Arr, item)
	}
	out.Hash["annotations"] = items
	var b strings.Builder
	if opts.Source != "" {
		fmt.Fprintf(&b, "-- %s: highlights from %s, for KOReader to import\n", filepath.Base(file), opts.Source)
	} else {
		fmt.Fprintf(&b, "-- %s: highlights for KOReader to import\n", filepath.Base(file))
	}
	b.WriteString("return ")
	WriteLua(&b, out, 0)
	b.WriteString("\n")
	return b.String(), skipped
}

// koreaderEPUBPlace sets an item's page, pos0 and pos1 from a CFI range.
func koreaderEPUBPlace(book *koBook, anchor string, item *LuaTable) bool {
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
	item.Hash["page"], item.Hash["pos0"], item.Hash["pos1"] = pos0, pos0, pos1
	item.Hash["epubcfi"] = anchor // newer KOReader keeps one beside its XPointers
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
func koreaderPDFPlace(anchor string, item *LuaTable) bool {
	var a pdfAnchor
	if json.Unmarshal([]byte(anchor), &a) != nil || a.Page < 1 || len(a.Rects) == 0 {
		return false
	}
	pos := func(x, y float64) *LuaTable {
		return &LuaTable{Hash: map[string]any{"page": a.Page, "rotation": 0, "zoom": 1, "x": x, "y": y}}
	}
	boxes := &LuaTable{Hash: map[string]any{}}
	for _, r := range a.Rects {
		boxes.Arr = append(boxes.Arr, &LuaTable{Hash: map[string]any{"x": r[0], "y": r[1], "w": r[2] - r[0], "h": r[3] - r[1]}})
	}
	first, last := a.Rects[0], a.Rects[len(a.Rects)-1]
	inset := func(r [4]float64) float64 { return math.Min(2, (r[2]-r[0])/2) }
	item.Hash["page"] = a.Page
	item.Hash["pos0"] = pos(first[0]+inset(first), (first[1]+first[3])/2)
	item.Hash["pos1"] = pos(last[2]-inset(last), (last[1]+last[3])/2)
	item.Hash["pboxes"] = boxes
	return true
}

// ---- import ----------------------------------------------------------------------

// ImportResult counts what Annotations found; a caller merging highlights
// fills in Added, Updated and Unchanged.
type ImportResult struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Skipped   int `json:"skipped"`   // highlights whose place wasn't found in this copy of the book
	Bookmarks int `json:"bookmarks"` // page bookmarks, which aren't highlights
}

// Annotations reads a KOReader annotations file (as parsed by ParseLuaTable)
// into highlights for a book file (file, of format "epub" or "pdf"). Their ids are derived from KOReader's own identity for a
// highlight (its place and creation time), so importing a file again
// doesn't duplicate anything.
func Annotations(file, format string, t *LuaTable, opts Options) (recs []Highlight, res ImportResult, err error) {
	list := t.Table("annotations")
	if list == nil {
		return nil, res, errors.New("no annotations in this file: export them from KOReader's bookmark list (Export annotations)")
	}
	if paging, _ := t.Hash["paging"].(bool); paging != (format == "pdf") {
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
	now := opts.now().UnixMilli()
	for _, v := range list.Arr {
		item, ok := v.(*LuaTable)
		if !ok {
			continue
		}
		if item.Str("drawer") == "" {
			res.Bookmarks++ // a page bookmark
			continue
		}
		var anchor, identity string
		if format == "epub" {
			anchor, identity = koreaderEPUBAnchor(book, item)
		} else {
			anchor, identity = koreaderPDFAnchor(item)
		}
		if anchor == "" || (opts.MaxAnchorLen > 0 && len(anchor) > opts.MaxAnchorLen) {
			res.Skipped++
			continue
		}
		a := Highlight{
			Anchor:  anchor,
			Text:    clip(item.Str("text"), opts.MaxTextLen),
			Note:    clip(item.Str("note"), opts.MaxTextLen),
			Color:   item.Str("color"),
			Chapter: clip(strings.TrimSpace(item.Str("chapter")), opts.MaxLabelLen),
		}
		if len(opts.Colors) > 0 && !slices.Contains(opts.Colors, a.Color) {
			a.Color = opts.Colors[0]
		}
		if n, ok := item.Num("pageno"); ok && n > 0 && n < 1e7 {
			a.Page = int(n)
		}
		a.CreatedAt = Millis(item.Str("datetime"))
		a.UpdatedAt = Millis(item.Str("datetime_updated"))
		if a.CreatedAt <= 0 || a.CreatedAt > now {
			a.CreatedAt = now
		}
		if a.UpdatedAt < a.CreatedAt || a.UpdatedAt > now {
			a.UpdatedAt = a.CreatedAt
		}
		sum := sha256.Sum256([]byte(format + "\x00" + identity + "\x00" + item.Str("datetime")))
		a.ID = IDPrefix + hex.EncodeToString(sum[:20])
		recs = append(recs, a)
	}
	return recs, res, nil
}

// clip shortens s to at most max bytes (0: no limit), on a character
// boundary.
func clip(s string, max int) string {
	if max <= 0 || len(s) <= max {
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
func koreaderEPUBAnchor(book *koBook, item *LuaTable) (anchor, identity string) {
	pos0, pos1 := item.Str("pos0"), item.Str("pos1")
	if pos0 == "" {
		pos0 = item.Str("page")
	}
	if pos1 == "" {
		pos1 = pos0
	}
	identity = pos0 + "\x00" + pos1

	if cfi := item.Str("epubcfi"); cfi != "" {
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
	needle := squeezeString(item.Str("text"))
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
func koreaderPDFAnchor(item *LuaTable) (anchor, identity string) {
	pos0, pos1 := item.Table("pos0"), item.Table("pos1")
	page, _ := item.Num("page")
	if page < 1 && pos0 != nil {
		page, _ = pos0.Num("page")
	}
	xy := func(t *LuaTable) (x, y float64, ok bool) {
		if t == nil {
			return 0, 0, false
		}
		x, ok1 := t.Num("x")
		y, ok2 := t.Num("y")
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
	if boxes := item.Table("pboxes"); boxes != nil {
		for _, v := range boxes.Arr {
			b, _ := v.(*LuaTable)
			if b == nil {
				continue
			}
			x, y, ok := xy(b)
			w, _ := b.Num("w")
			h, _ := b.Num("h")
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

func (o Options) deviceID() string {
	if o.DeviceID == "" {
		return "koreader-go"
	}
	return o.DeviceID
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}
