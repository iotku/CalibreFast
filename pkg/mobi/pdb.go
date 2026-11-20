package mobi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	// ErrNotMobi: the data isn't a MOBI-family book (no BOOKMOBI PDB header).
	ErrNotMobi = errors.New("mobi: not a MOBI/AZW3 book")
	// ErrDRM: the book is encrypted.
	ErrDRM = errors.New("mobi: book is DRM-protected")
	// ErrTooLarge: some part of the book is larger than the Limits allow.
	ErrTooLarge = errors.New("mobi: book exceeds size limits")
)

// errCorrupt wraps a description of malformed data.
func errCorrupt(format string, a ...any) error {
	return fmt.Errorf("mobi: corrupt book: "+format, a...)
}

// noIndex is the "absent" value for record indexes in MOBI headers.
const noIndex = 0xFFFFFFFF

func be16(b []byte, off int) uint16 {
	if off < 0 || off+2 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint16(b[off:])
}

func be32(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[off:])
}

// pdb is a Palm database: a header and a list of records.
type pdb struct {
	data    []byte
	offsets []int // start of each record; record i ends where i+1 starts
	typ     string
}

func parsePDB(data []byte) (*pdb, error) {
	if len(data) < 78 {
		return nil, ErrNotMobi
	}
	typ := string(data[60:68])
	if typ != "BOOKMOBI" {
		return nil, ErrNotMobi
	}
	n := int(be16(data, 76))
	if n == 0 || 78+8*n > len(data) {
		return nil, errCorrupt("record list")
	}
	p := &pdb{data: data, offsets: make([]int, n), typ: typ}
	for i := range n {
		p.offsets[i] = int(be32(data, 78+8*i))
	}
	return p, nil
}

func (p *pdb) count() int { return len(p.offsets) }

// record returns record i, or nil when i is out of range or its bounds are
// bad (offsets past the end of the file or going backwards).
func (p *pdb) record(i int) []byte {
	if i < 0 || i >= len(p.offsets) {
		return nil
	}
	start := p.offsets[i]
	end := len(p.data)
	if i+1 < len(p.offsets) {
		end = p.offsets[i+1]
	}
	if start < 0 || start > end || end > len(p.data) {
		return nil
	}
	return p.data[start:end:end]
}

// recordAt is record(i) for a header's uint32 index, nil for noIndex.
func (p *pdb) recordAt(i uint32) []byte {
	if i == noIndex || i > 1<<16 {
		return nil
	}
	return p.record(int(i))
}
