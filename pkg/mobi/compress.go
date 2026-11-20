package mobi

import (
	"encoding/binary"
)

// trailingSize is the size of the trailing entries the extra-data flags say
// a text record ends with (they aren't part of the compressed text).
func trailingSize(rec []byte, flags uint16) int {
	size := len(rec)
	num := 0
	entry := func(end int) int {
		result, shift := 0, 0
		for end > 0 {
			v := rec[end-1]
			result |= int(v&0x7F) << shift
			shift += 7
			end--
			if v&0x80 != 0 || shift >= 28 {
				break
			}
		}
		return result
	}
	for f := flags >> 1; f != 0; f >>= 1 {
		if f&1 != 0 && size-num > 0 {
			num += entry(size - num)
		}
	}
	if flags&1 != 0 && size-num-1 >= 0 {
		num += int(rec[size-num-1]&0x3) + 1
	}
	return min(num, size)
}

// palmdoc appends the PalmDOC (LZ77) decompression of src to dst. Output
// beyond max bytes is an error.
func palmdoc(dst, src []byte, max int) ([]byte, error) {
	start := len(dst)
	for i := 0; i < len(src); {
		c := src[i]
		i++
		switch {
		case c >= 1 && c <= 8:
			n := min(int(c), len(src)-i)
			dst = append(dst, src[i:i+n]...)
			i += n
		case c < 0x80:
			dst = append(dst, c)
		case c >= 0xC0:
			dst = append(dst, ' ', c^0x80)
		default:
			if i >= len(src) {
				return dst, nil
			}
			w := int(c)<<8 | int(src[i])
			i++
			dist := (w >> 3) & 0x7FF
			n := w&7 + 3
			if dist == 0 || dist > len(dst)-start {
				return dst, errCorrupt("PalmDOC back-reference")
			}
			for range n {
				dst = append(dst, dst[len(dst)-dist])
			}
		}
		if len(dst) > max {
			return dst, ErrTooLarge
		}
	}
	return dst, nil
}

// huffcdic is a HUFF/CDIC decompressor (Mobipocket's dictionary scheme).
type huffcdic struct {
	dict1   [256]huffCode
	mincode [33]uint64
	maxcode [33]uint64
	phrases []phrase
	max     int // most bytes one record (or phrase) may expand to
	budget  int // bytes all expanded phrases together may still take
}

type huffCode struct {
	codelen uint8
	term    bool
	maxcode uint64
}

type phrase struct {
	data []byte
	done bool // data is already expanded
	busy bool // being expanded (a cycle if seen again)
}

func newHuffcdic(huff []byte, cdics [][]byte, max int) (*huffcdic, error) {
	if len(huff) < 24 || string(huff[:8]) != "HUFF\x00\x00\x00\x18" {
		return nil, errCorrupt("HUFF header")
	}
	off1, off2 := int(be32(huff, 8)), int(be32(huff, 12))
	if off1 < 0 || off1+256*4 > len(huff) || off2 < 0 || off2+64*4 > len(huff) {
		return nil, errCorrupt("HUFF tables")
	}
	d := &huffcdic{max: max, budget: 64 * max}
	for i := range 256 {
		v := be32(huff, off1+4*i)
		codelen := uint8(v & 0x1F)
		if codelen == 0 {
			continue // used only by a hostile book; decoding it fails below
		}
		d.dict1[i] = huffCode{codelen: codelen, term: v&0x80 != 0, maxcode: (uint64(v>>8)+1)<<(32-codelen) - 1}
	}
	d.maxcode[0] = 1<<32 - 1
	for l := 1; l <= 32; l++ {
		lo := uint64(be32(huff, off2+8*(l-1)))
		hi := uint64(be32(huff, off2+8*(l-1)+4))
		d.mincode[l] = lo << (32 - l)
		d.maxcode[l] = (hi+1)<<(32-l) - 1
	}
	for _, c := range cdics {
		if len(c) < 16 || string(c[:8]) != "CDIC\x00\x00\x00\x10" {
			return nil, errCorrupt("CDIC header")
		}
		total, bits := int(be32(c, 8)), be32(c, 12)
		if bits > 16 {
			return nil, errCorrupt("CDIC header")
		}
		n := min(1<<bits, total-len(d.phrases))
		for i := range n {
			off := 16 + int(be16(c, 16+2*i))
			if 16+2*i+2 > len(c) || off+2 > len(c) {
				return nil, errCorrupt("CDIC entry")
			}
			blen := be16(c, off)
			end := off + 2 + int(blen&0x7FFF)
			if end > len(c) {
				return nil, errCorrupt("CDIC entry")
			}
			d.phrases = append(d.phrases, phrase{data: c[off+2 : end], done: blen&0x8000 != 0})
		}
	}
	return d, nil
}

func (d *huffcdic) unpack(dst, data []byte, depth int) ([]byte, error) {
	if depth > 64 {
		return dst, errCorrupt("HUFF/CDIC nesting")
	}
	start := len(dst)
	bitsLeft := len(data) * 8
	buf := make([]byte, len(data)+8)
	copy(buf, data)
	pos := 0
	x := binary.BigEndian.Uint64(buf)
	n := 32
	for {
		if n <= 0 {
			pos += 4
			if pos+8 > len(buf) {
				break
			}
			x = binary.BigEndian.Uint64(buf[pos:])
			n += 32
		}
		code := (x >> uint(n)) & 0xFFFFFFFF
		e := d.dict1[code>>24]
		codelen := int(e.codelen)
		if codelen == 0 {
			return dst, errCorrupt("HUFF code")
		}
		maxcode := e.maxcode
		if !e.term {
			for codelen <= 32 && code < d.mincode[codelen] {
				codelen++
			}
			if codelen > 32 {
				return dst, errCorrupt("HUFF code")
			}
			maxcode = d.maxcode[codelen]
		}
		n -= codelen
		bitsLeft -= codelen
		if bitsLeft < 0 {
			break
		}
		if maxcode < code {
			return dst, errCorrupt("HUFF code")
		}
		r := (maxcode - code) >> uint(32-codelen)
		if r >= uint64(len(d.phrases)) {
			return dst, errCorrupt("HUFF phrase")
		}
		p := &d.phrases[r]
		if !p.done {
			if p.busy {
				return dst, errCorrupt("HUFF/CDIC cycle")
			}
			p.busy = true
			out, err := d.unpack(nil, p.data, depth+1)
			p.busy = false
			if err != nil {
				return dst, err
			}
			if d.budget -= len(out); d.budget < 0 {
				return dst, ErrTooLarge
			}
			p.data, p.done = out, true
		}
		dst = append(dst, p.data...)
		if len(dst)-start > d.max {
			return dst, ErrTooLarge
		}
	}
	return dst, nil
}
