package mobi

import "math/bits"

// indexEntry is one entry of an INDX index: its label and tag values.
type indexEntry struct {
	label []byte
	tags  map[uint8][]uint32
}

func (e indexEntry) tag(t uint8, i int) (uint32, bool) {
	v := e.tags[t]
	if i >= len(v) {
		return 0, false
	}
	return v[i], true
}

type tagDef struct {
	tag, perEntry, mask, end uint8
}

// Caps on what one index may hold.
const (
	maxIndexRecords = 4096
	maxIndexEntries = 1 << 20
	maxCNCXRecords  = 256
)

// vwi reads a forward variable-width integer (high bit marks the last
// byte). It returns the value and bytes consumed (0 when out of data).
func vwi(b []byte, off int) (uint32, int) {
	var v uint32
	for n := 0; n < 5 && off+n < len(b); n++ {
		c := b[off+n]
		v = v<<7 | uint32(c&0x7F)
		if c&0x80 != 0 {
			return v, n + 1
		}
	}
	return 0, 0
}

type indxHeader struct {
	length, start, count, nctoc uint32
}

func parseINDX(rec []byte) (indxHeader, bool) {
	if len(rec) < 56 || string(rec[:4]) != "INDX" {
		return indxHeader{}, false
	}
	return indxHeader{
		length: be32(rec, 4),
		start:  be32(rec, 20),
		count:  be32(rec, 24),
		nctoc:  be32(rec, 52),
	}, true
}

// readIndex reads the index whose main record is at idx: its entries and
// the CNCX strings they refer to (by offset).
func readIndex(p *pdb, idx uint32) ([]indexEntry, map[uint32][]byte) {
	main := p.recordAt(idx)
	hdr, ok := parseINDX(main)
	if !ok || hdr.count > maxIndexRecords || hdr.nctoc > maxCNCXRecords {
		return nil, nil
	}
	cncx := map[uint32][]byte{}
	for j := range hdr.nctoc {
		readCNCX(p.recordAt(idx+hdr.count+1+j), j<<16, cncx)
	}
	ctrlBytes, defs := readTAGX(main, int(hdr.length))
	var out []indexEntry
	for i := uint32(1); i <= hdr.count; i++ {
		rec := p.recordAt(idx + i)
		h, ok := parseINDX(rec)
		if !ok {
			continue
		}
		idxt := int(h.start)
		if idxt < 0 || idxt+4 > len(rec) || string(rec[idxt:idxt+4]) != "IDXT" {
			continue
		}
		n := int(h.count)
		if idxt+4+2*n > len(rec) {
			continue
		}
		for j := range n {
			start := int(be16(rec, idxt+4+2*j))
			end := idxt
			if j+1 < n {
				end = int(be16(rec, idxt+4+2*(j+1)))
			}
			if start >= end || end > len(rec) {
				continue
			}
			tl := int(rec[start])
			if start+1+tl > end {
				continue
			}
			e := indexEntry{label: rec[start+1 : start+1+tl]}
			e.tags = readTags(rec[:end], start+1+tl, ctrlBytes, defs)
			out = append(out, e)
			if len(out) >= maxIndexEntries {
				return out, cncx
			}
		}
	}
	return out, cncx
}

func readCNCX(rec []byte, base uint32, out map[uint32][]byte) {
	for off := 0; off < len(rec) && rec[off] != 0; {
		n, used := vwi(rec, off)
		if used == 0 || off+used+int(n) > len(rec) {
			return
		}
		out[base+uint32(off)] = rec[off+used : off+used+int(n)]
		off += used + int(n)
	}
}

func readTAGX(rec []byte, at int) (int, []tagDef) {
	if at < 0 || at+12 > len(rec) || string(rec[at:at+4]) != "TAGX" {
		return 0, nil
	}
	first := int(be32(rec, at+4))
	ctrl := int(be32(rec, at+8))
	if ctrl > 8 {
		return 0, nil
	}
	var defs []tagDef
	for i := 12; i+4 <= first && at+i+4 <= len(rec) && len(defs) < 64; i += 4 {
		b := rec[at+i:]
		defs = append(defs, tagDef{b[0], b[1], b[2], b[3]})
	}
	return ctrl, defs
}

// readTags decodes an entry's tag values (data ends at len(rec)).
func readTags(rec []byte, start, ctrlBytes int, defs []tagDef) map[uint8][]uint32 {
	type pending struct {
		tag      uint8
		count    int // values to read, times perEntry; -1: read bytes
		bytes    int
		perEntry int
	}
	var list []pending
	ctrl := 0
	data := start + ctrlBytes
	if data > len(rec) {
		return nil
	}
	for _, d := range defs {
		if d.end == 1 {
			ctrl++
			continue
		}
		if start+ctrl >= len(rec) || d.mask == 0 {
			continue
		}
		v := rec[start+ctrl] & d.mask
		if v == 0 {
			continue
		}
		if v == d.mask && bits.OnesCount8(d.mask) > 1 {
			n, used := vwi(rec, data)
			if used == 0 {
				return nil
			}
			data += used
			list = append(list, pending{tag: d.tag, count: -1, bytes: int(n), perEntry: int(d.perEntry)})
			continue
		}
		v >>= bits.TrailingZeros8(d.mask)
		list = append(list, pending{tag: d.tag, count: int(v), perEntry: int(d.perEntry)})
	}
	tags := map[uint8][]uint32{}
	for _, t := range list {
		var vals []uint32
		if t.count >= 0 {
			for range t.count * t.perEntry {
				v, used := vwi(rec, data)
				if used == 0 {
					break
				}
				data += used
				vals = append(vals, v)
			}
		} else {
			for got := 0; got < t.bytes; {
				v, used := vwi(rec, data)
				if used == 0 {
					break
				}
				data += used
				got += used
				vals = append(vals, v)
			}
		}
		tags[t.tag] = vals
	}
	return tags
}

// ncxOrder puts NCX entries in reading order with their depths. KF8
// stores them breadth first (all chapters, then their sections), linked
// by parent (tag 21) and first/last child (tags 22, 23); entries without
// those links keep index order and their level (tag 4).
type ncxItem struct {
	i, depth int
}

func ncxOrder(entries []indexEntry) []ncxItem {
	var out []ncxItem
	seen := make([]bool, len(entries))
	var visit func(i, depth int)
	visit = func(i, depth int) {
		if i < 0 || i >= len(entries) || seen[i] || depth > 16 {
			return
		}
		seen[i] = true
		d := depth
		if lvl, ok := entries[i].tag(4, 0); ok && depth == 0 && lvl < 16 {
			d = int(lvl)
		}
		out = append(out, ncxItem{i: i, depth: d})
		first, ok1 := entries[i].tag(22, 0)
		last, ok2 := entries[i].tag(23, 0)
		if ok1 && ok2 && first <= last {
			for c := int(first); c <= int(last) && c < len(entries); c++ {
				visit(c, d+1)
			}
		}
	}
	for i, e := range entries {
		if _, child := e.tag(21, 0); !child {
			visit(i, 0)
		}
	}
	for i := range entries { // orphans (a parent link to nowhere)
		visit(i, 0)
	}
	return out
}
