package mobi

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"os"
)

// Limits caps what reading one book may allocate. The zero value of a
// field means its default.
type Limits struct {
	MaxFile     int64 // the book file (OpenFile): 512 MiB
	MaxText     int   // the decompressed text: 128 MiB
	MaxResource int   // one image or (decompressed) font: 32 MiB
}

func (l Limits) withDefaults() Limits {
	if l.MaxFile <= 0 {
		l.MaxFile = 512 << 20
	}
	if l.MaxText <= 0 {
		l.MaxText = 128 << 20
	}
	if l.MaxResource <= 0 {
		l.MaxResource = 32 << 20
	}
	return l
}

// Book is an opened MOBI-family book.
type Book struct {
	Metadata Metadata
	// KF8 is true for an AZW3 (KF8) book, or a joint file read through its
	// KF8 half; false for a MOBI 6 book.
	KF8 bool

	limits Limits
	p      *pdb
	h      *header // the header the book is read through
}

// Open reads a book from its file's bytes, with default limits.
func Open(data []byte) (*Book, error) { return OpenLimits(data, Limits{}) }

// OpenFile reads the book at path, refusing files over Limits.MaxFile.
func OpenFile(path string, lim Limits) (*Book, error) {
	lim = lim.withDefaults()
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, lim.MaxFile+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > lim.MaxFile {
		return nil, ErrTooLarge
	}
	return OpenLimits(data, lim)
}

// OpenLimits reads a book from its file's bytes. The book keeps data.
func OpenLimits(data []byte, lim Limits) (*Book, error) {
	p, err := parsePDB(data)
	if err != nil {
		return nil, err
	}
	h, err := parseHeader(p, 0)
	if err != nil {
		return nil, err
	}
	if !h.kf8() {
		if b, ok := h.exthUint(121); ok && b != noIndex {
			// A joint file: the KF8 half starts at (or just after) the boundary.
			for _, at := range []int{int(b), int(b) + 1} {
				if k8, err := parseHeader(p, at); err == nil && k8.kf8() {
					// The images are stored once, in the MOBI 6 half, and
					// kindle:embed numbers count from its first one.
					k8.firstResource = h.firstResource
					h = k8
					break
				}
			}
		}
	}
	if h.encryption != 0 {
		return nil, ErrDRM
	}
	switch h.compression {
	case compNone, compPalmDOC, compHuff:
	default:
		return nil, fmt.Errorf("mobi: unknown compression %#x", h.compression)
	}
	return &Book{Metadata: h.metadata(), KF8: h.kf8(), limits: lim.withDefaults(), p: p, h: h}, nil
}

// text returns the book's decompressed text (the "raw markup").
func (b *Book) text() ([]byte, error) {
	h := b.h
	max := b.limits.MaxText
	out := make([]byte, 0, min(int(h.textLength), max))
	var huff *huffcdic
	if h.compression == compHuff {
		if h.huffCount == noIndex || h.huffCount == 0 || h.huffCount > 256 {
			return nil, errCorrupt("HUFF records")
		}
		var cdics [][]byte
		for i := uint32(1); i < h.huffCount; i++ {
			cdics = append(cdics, b.p.recordAt(h.huffRecord+i))
		}
		var err error
		if huff, err = newHuffcdic(b.p.recordAt(h.huffRecord), cdics, 1<<20); err != nil {
			return nil, err
		}
	}
	for i := 1; i <= h.textRecords; i++ {
		rec := b.p.record(h.start + i)
		if rec == nil {
			return nil, errCorrupt("text record %d", i)
		}
		rec = rec[:len(rec)-trailingSize(rec, h.extraFlags)]
		var err error
		switch h.compression {
		case compNone:
			out = append(out, rec...)
		case compPalmDOC:
			out, err = palmdoc(out, rec, max)
		case compHuff:
			out, err = huff.unpack(out, rec, 0)
		}
		if err != nil {
			return nil, err
		}
		if len(out) > max {
			return nil, ErrTooLarge
		}
	}
	if int(h.textLength) < len(out) {
		out = out[:h.textLength]
	}
	return out, nil
}

// resource is an image or font from the book.
type resource struct {
	name, mediaType string
	data            []byte
}

// resources returns the book's images and fonts by their 1-based number
// (the XXXX of kindle:embed:XXXX, or a MOBI 6 recindex).
func (b *Book) resources() map[int]*resource {
	out := map[int]*resource{}
	first := b.h.firstResource
	if first == noIndex {
		return out
	}
	for i := int(first); i < b.p.count(); i++ {
		rec := b.p.record(i)
		if bytes.HasPrefix(rec, []byte("BOUNDARY")) {
			break // the end of a joint file's MOBI 6 half
		}
		n := i - int(first) + 1
		if len(rec) > b.limits.MaxResource {
			continue
		}
		if ext, mt := imageType(rec); ext != "" {
			out[n] = &resource{name: fmt.Sprintf("image%05d.%s", n, ext), mediaType: mt, data: rec}
		} else if bytes.HasPrefix(rec, []byte("FONT")) {
			if data, ext, mt := decodeFont(rec, b.limits.MaxResource); data != nil {
				out[n] = &resource{name: fmt.Sprintf("font%05d.%s", n, ext), mediaType: mt, data: data}
			}
		}
	}
	return out
}

func imageType(b []byte) (ext, mediaType string) {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg", "image/jpeg"
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png", "image/png"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "gif", "image/gif"
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp", "image/webp"
	}
	return "", ""
}

// decodeFont undoes a FONT record's obfuscation and compression.
func decodeFont(rec []byte, max int) ([]byte, string, string) {
	if len(rec) < 24 {
		return nil, "", ""
	}
	size, flags, start := be32(rec, 4), be32(rec, 8), int(be32(rec, 12))
	keyLen, keyStart := int(be32(rec, 16)), int(be32(rec, 20))
	if start < 24 || start > len(rec) || int64(size) > int64(max) {
		return nil, "", ""
	}
	data := bytes.Clone(rec[start:])
	if flags&2 != 0 {
		if keyLen <= 0 || keyStart < 0 || keyStart+keyLen > len(rec) {
			return nil, "", ""
		}
		key := rec[keyStart : keyStart+keyLen]
		for i := range min(len(data), 1040) {
			data[i] ^= key[i%keyLen]
		}
	}
	if flags&1 != 0 {
		zr, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, "", ""
		}
		data, err = io.ReadAll(io.LimitReader(zr, int64(max)+1))
		if err != nil || len(data) > max {
			return nil, "", ""
		}
	}
	switch {
	case bytes.HasPrefix(data, []byte("OTTO")):
		return data, "otf", "font/otf"
	case bytes.HasPrefix(data, []byte{0, 1, 0, 0}), bytes.HasPrefix(data, []byte("true")), bytes.HasPrefix(data, []byte("ttcf")):
		return data, "ttf", "font/ttf"
	case bytes.HasPrefix(data, []byte("wOFF")):
		return data, "woff", "font/woff"
	case bytes.HasPrefix(data, []byte("wOF2")):
		return data, "woff2", "font/woff2"
	}
	return nil, "", ""
}

// coverNumber is the resource number of the cover image, or 0.
func (b *Book) coverNumber(res map[int]*resource) int {
	if off, ok := b.h.exthUint(201); ok && off != noIndex {
		if r := res[int(off)+1]; r != nil && r.mediaType != "" && r.name[:5] == "image" {
			return int(off) + 1
		}
	}
	if s := b.h.exthString(129); s != "" {
		if n, ok := embedNumber(s); ok && res[n] != nil {
			return n
		}
	}
	return 0
}
