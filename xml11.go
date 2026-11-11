package main

import (
	"bytes"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

// encoding/xml reads only XML 1.0: it refuses a document whose declaration
// says version="1.1", and it refuses character references to the C0 control
// characters that XML 1.1 allows (&#x1; to &#x1F;). Some EPUBs declare 1.1,
// so every XML document the server reads goes through xml11To10 first, which
// turns an XML 1.1 document into the XML 1.0 document that means the same
// thing as far as a reader of its text can tell:
//
//   - The declaration's version becomes "1.0".
//   - XML 1.1's extra line ends (NEL, U+0085, alone or after a CR, and LSEP,
//     U+2028) become "\n", as an XML 1.1 parser normalizes them (§2.11).
//   - References to the C0 controls XML 1.0 lacks are dropped: XML 1.0, and
//     so encoding/xml, cannot hold those characters, and they have no
//     visible text to keep. References inside CDATA sections, comments and
//     processing instructions are literal text there and are left alone.
//
// A document that isn't XML 1.1 is returned as is.

// xmlDeclRE matches an XML declaration at the very start of a document (after
// an optional UTF-8 byte order mark), capturing the version's value.
var xmlDeclRE = regexp.MustCompile(`^\x{FEFF}?<\?xml\s+version\s*=\s*(?:"([^"]*)"|'([^']*)')[^?]*\?>`)

// xmlEncodingRE captures the encoding named in an XML declaration.
var xmlEncodingRE = regexp.MustCompile(`\sencoding\s*=\s*(?:"([^"]*)"|'([^']*)')`)

func xml11To10(data []byte) []byte {
	m := xmlDeclRE.FindSubmatchIndex(data)
	if m == nil {
		return data
	}
	vs, ve := m[2], m[3]
	if vs < 0 {
		vs, ve = m[4], m[5]
	}
	if string(data[vs:ve]) != "1.1" {
		return data
	}
	decl := data[:m[1]]
	body := data[m[1]:]

	out := make([]byte, 0, len(data))
	out = append(out, decl[:vs]...)
	out = append(out, "1.0"...)
	out = append(out, decl[ve:]...)

	// NEL and LSEP can be told apart from other characters byte by byte only
	// in UTF-8; another encoding's bytes are left for its decoder.
	utf8 := true
	if em := xmlEncodingRE.FindSubmatch(decl); em != nil {
		enc := string(em[1]) + string(em[2])
		utf8 = strings.EqualFold(enc, "utf-8") || strings.EqualFold(enc, "utf8")
	}
	if utf8 {
		body = normalizeXML11LineEnds(body)
	}
	return append(out, dropC0CharRefs(body)...)
}

// normalizeXML11LineEnds turns CR NEL, NEL and LSEP into LF. (CR LF and a lone
// CR are XML 1.0 line ends too, which encoding/xml handles itself.)
func normalizeXML11LineEnds(b []byte) []byte {
	if !bytes.Contains(b, []byte("\u0085")) && !bytes.Contains(b, []byte(" ")) {
		return b
	}
	b = bytes.ReplaceAll(b, []byte("\r\u0085"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\u0085"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte(" "), []byte("\n"))
}

// dropC0CharRefs removes character references to U+0001 to U+001F other than
// tab, LF and CR, outside CDATA sections, comments and processing
// instructions.
func dropC0CharRefs(b []byte) []byte {
	if !bytes.Contains(b, []byte("&#")) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		rest := b[i:]
		skipTo := func(open, close string) bool {
			if !bytes.HasPrefix(rest, []byte(open)) {
				return false
			}
			n := bytes.Index(rest[len(open):], []byte(close))
			if n < 0 {
				n = len(rest)
			} else {
				n += len(open) + len(close)
			}
			out = append(out, rest[:n]...)
			i += n
			return true
		}
		if rest[0] == '<' {
			if skipTo("<![CDATA[", "]]>") || skipTo("<!--", "-->") || skipTo("<?", "?>") {
				continue
			}
		}
		if rest[0] == '&' && len(rest) > 2 && rest[1] == '#' {
			if n, ok := restrictedCharRef(rest); ok {
				i += n
				continue
			}
		}
		out = append(out, b[i])
		i++
	}
	return out
}

// restrictedCharRef reports whether ref starts with a character reference to
// a C0 control that XML 1.1 allows and XML 1.0 doesn't, and its length.
func restrictedCharRef(ref []byte) (int, bool) {
	end := bytes.IndexByte(ref, ';')
	if end < 0 || end > 12 {
		return 0, false
	}
	digits, base := string(ref[2:end]), 10
	if strings.HasPrefix(digits, "x") {
		digits, base = digits[1:], 16
	}
	n, err := strconv.ParseUint(digits, base, 32)
	if err != nil || n < 1 || n > 0x1F || n == '\t' || n == '\n' || n == '\r' {
		return 0, false
	}
	return end + 1, true
}

// xmlUnmarshal is xml.Unmarshal for XML 1.0 or 1.1.
func xmlUnmarshal(data []byte, v any) error {
	return xml.Unmarshal(xml11To10(data), v)
}
