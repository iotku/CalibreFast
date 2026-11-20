package mobi

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Text compression schemes (PalmDOC header).
const (
	compNone    = 1
	compPalmDOC = 2
	compHuff    = 0x4448 // "DH"
)

// header is one MOBI header: record 0 of a book, or of the KF8 half of a
// joint file (start is then that record's index). Record indexes in it are
// absolute (start already added) or noIndex.
type header struct {
	start       int
	compression uint16
	textLength  uint32
	textRecords int
	encryption  uint16
	mobiType    uint32
	utf8        bool
	version     uint32
	fullName    []byte
	locale      uint32

	firstResource uint32
	huffRecord    uint32
	huffCount     uint32
	extraFlags    uint16

	// KF8 (version 8) only.
	fdst     uint32
	skeleton uint32
	fragment uint32
	guide    uint32
	// The NCX index; MOBI 6 books may have one too.
	ncx uint32

	exth map[uint32][][]byte
}

func (h *header) kf8() bool { return h.version >= 8 }

func parseHeader(p *pdb, start int) (*header, error) {
	rec := p.record(start)
	if len(rec) < 0x18 || string(rec[16:20]) != "MOBI" {
		return nil, ErrNotMobi
	}
	hdrLen := int(be32(rec, 20))
	end := min(16+hdrLen, len(rec))
	field := func(off int) uint32 {
		if off+4 > end {
			return noIndex
		}
		return be32(rec, off)
	}
	h := &header{
		start:       start,
		compression: be16(rec, 0),
		textLength:  be32(rec, 4),
		textRecords: int(be16(rec, 8)),
		encryption:  be16(rec, 12),
		mobiType:    field(0x18),
		utf8:        field(0x1C) == 65001,
		version:     field(0x24),
		locale:      field(0x5C),
		exth:        map[uint32][][]byte{},
	}
	if h.version == noIndex {
		h.version = 0
	}
	if off, n := int(field(0x54)), int(field(0x58)); off >= 0 && n >= 0 && off < len(rec) && n < 1<<16 {
		h.fullName = rec[off:min(off+n, len(rec))]
	}
	rel := func(v uint32) uint32 {
		if v == noIndex || int64(v)+int64(start) >= int64(p.count()) {
			return noIndex
		}
		return v + uint32(start)
	}
	h.firstResource = rel(field(0x6C))
	h.huffRecord = rel(field(0x70))
	h.huffCount = field(0x74)
	h.ncx = rel(field(0xF4))
	if hdrLen >= 0xE4 && h.version >= 5 {
		h.extraFlags = be16(rec, 0xF2)
	}
	if h.kf8() {
		h.fdst = rel(field(0xC0))
		h.fragment = rel(field(0xF8))
		h.skeleton = rel(field(0xFC))
		h.guide = rel(field(0x104))
	} else {
		h.fdst, h.fragment, h.skeleton, h.guide = noIndex, noIndex, noIndex, noIndex
	}
	if field(0x80)&0x40 != 0 {
		parseEXTH(rec, 16+hdrLen, h.exth)
	}
	return h, nil
}

func parseEXTH(rec []byte, at int, out map[uint32][][]byte) {
	if at < 0 || at+12 > len(rec) || string(rec[at:at+4]) != "EXTH" {
		return
	}
	n := int(be32(rec, at+8))
	pos := at + 12
	for i := 0; i < n && pos+8 <= len(rec); i++ {
		typ := be32(rec, pos)
		size := int(be32(rec, pos+4))
		if size < 8 || pos+size > len(rec) {
			return
		}
		out[typ] = append(out[typ], rec[pos+8:pos+size])
		pos += size
	}
}

// decode turns text in the book's encoding into valid UTF-8.
func (h *header) decode(b []byte) string {
	if !h.utf8 {
		s, err := charmap.Windows1252.NewDecoder().Bytes(b)
		if err == nil {
			return string(s)
		}
	}
	return strings.ToValidUTF8(string(b), "�")
}

func (h *header) exthString(typ uint32) string {
	v := h.exth[typ]
	if len(v) == 0 {
		return ""
	}
	return strings.TrimSpace(h.decode(v[0]))
}

func (h *header) exthStrings(typ uint32) []string {
	var out []string
	for _, v := range h.exth[typ] {
		if s := strings.TrimSpace(h.decode(v)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (h *header) exthUint(typ uint32) (uint32, bool) {
	v := h.exth[typ]
	if len(v) == 0 || len(v[0]) != 4 {
		return 0, false
	}
	return be32(v[0], 0), true
}

// Metadata is what a book's headers say about it.
type Metadata struct {
	Title       string
	Authors     []string
	Publisher   string
	Description string // as stored: often HTML
	ISBN        string
	ASIN        string
	Subjects    []string
	Date        string // as stored, usually ISO 8601
	Language    string // BCP 47 tag when the book says, else ""
	RTL         bool   // right-to-left page progression
}

func (h *header) metadata() Metadata {
	m := Metadata{
		Title:       h.exthString(503),
		Authors:     h.exthStrings(100),
		Publisher:   h.exthString(101),
		Description: h.exthString(103),
		ISBN:        h.exthString(104),
		ASIN:        h.exthString(113),
		Subjects:    h.exthStrings(105),
		Date:        h.exthString(106),
		Language:    h.exthString(524),
		RTL:         strings.EqualFold(h.exthString(527), "rtl"),
	}
	if m.Title == "" {
		m.Title = strings.TrimSpace(h.decode(h.fullName))
	}
	if m.Language == "" {
		m.Language = localeTag(h.locale)
	}
	m.Title = cleanText(m.Title)
	return m
}

// cleanText drops characters XML 1.0 can't hold.
func cleanText(s string) string {
	ok := true
	for _, r := range s {
		if !xmlChar(r) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if xmlChar(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func xmlChar(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' ||
		(r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD && r != utf8.RuneError) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

// localeTag maps the common Windows LANGIDs in a MOBI header's locale field.
func localeTag(locale uint32) string {
	if locale == noIndex {
		return ""
	}
	tags := map[uint32]string{
		0x01: "ar", 0x02: "bg", 0x03: "ca", 0x04: "zh", 0x05: "cs", 0x06: "da", 0x07: "de",
		0x08: "el", 0x09: "en", 0x0A: "es", 0x0B: "fi", 0x0C: "fr", 0x0D: "he", 0x0E: "hu",
		0x0F: "is", 0x10: "it", 0x11: "ja", 0x12: "ko", 0x13: "nl", 0x14: "no", 0x15: "pl",
		0x16: "pt", 0x18: "ro", 0x19: "ru", 0x1A: "hr", 0x1B: "sk", 0x1D: "sv", 0x1E: "th",
		0x1F: "tr", 0x21: "id", 0x22: "uk", 0x2A: "vi", 0x39: "hi",
	}
	return tags[locale&0xFF]
}
