package main

import (
	"encoding/xml"
	"os"
	"strings"
)

// This is unlikely to be compliant, but for spec reference see below.
// https://idpf.org/epub/20/spec/OPF_2.0_final_spec.html#Section2.2

type OPF struct {
	XMLName  xml.Name    `xml:"http://www.idpf.org/2007/opf package"`
	Metadata OPFMetadata `xml:"metadata"`
}

type OPFMetadata struct {
	Title       string          `xml:"http://purl.org/dc/elements/1.1/ title"`
	Description string          `xml:"http://purl.org/dc/elements/1.1/ description"`
	Publisher   string          `xml:"http://purl.org/dc/elements/1.1/ publisher"`
	Date        string          `xml:"http://purl.org/dc/elements/1.1/ date"`
	Languages   []string        `xml:"http://purl.org/dc/elements/1.1/ language"` // in order; first is primary
	Identifiers []opfIdentifier `xml:"http://purl.org/dc/elements/1.1/ identifier"`
	Subjects    []string        `xml:"http://purl.org/dc/elements/1.1/ subject"`
	Creators    []string        `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Meta        []opfMeta       `xml:"meta"`
}

type opfIdentifier struct {
	Scheme string `xml:"scheme,attr"` // may be empty: such identifiers must still round-trip
	Value  string `xml:",chardata"`
	ID     string `xml:"id,attr"` // e.g. "uuid_id", "calibre_id"; preserved on rewrite
}

// Library identifiers are bookkeeping, which Calibre writes into the OPF
// and its exports: the book's id ("calibre") and uuid, and an EPUB's package
// uuid (no scheme, "urn:uuid:…"). The editor doesn't show them, and saving
// keeps the book's own as they are.
var libraryIdentifierSchemes = map[string]bool{"calibre": true, "uuid": true}

func isLibraryIdentifier(id opfIdentifier) bool {
	scheme := strings.TrimSpace(id.Scheme)
	if scheme == "" {
		value := strings.TrimSpace(id.Value)
		return len(value) > 9 && strings.EqualFold(value[:9], "urn:uuid:")
	}
	return libraryIdentifierSchemes[strings.ToLower(scheme)]
}

// splitLibraryIdentifiers separates library identifiers from the others,
// keeping each group's order.
func splitLibraryIdentifiers(ids []opfIdentifier) (library, others []opfIdentifier) {
	for _, id := range ids {
		if isLibraryIdentifier(id) {
			library = append(library, id)
		} else {
			others = append(others, id)
		}
	}
	return library, others
}

type opfMeta struct {
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Property string `xml:"property,attr"` // EPUB3
	Value    string `xml:",chardata"`     // You probably want Content

	FileAs string `xml:"file-as,attr"`
}

func loadOPF(path string) (*OPF, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var opf OPF

	err = xml.Unmarshal(data, &opf)
	if err != nil {
		return nil, err
	}

	return &opf, nil
}

func writeOPF(opf *OPF, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	f.WriteString(`<?xml version='1.0' encoding='utf-8'?>` + "\n")

	enc := xml.NewEncoder(f)
	enc.Indent("", "    ")

	// <package>
	enc.EncodeToken(xml.StartElement{
		Name: xml.Name{Space: "http://www.idpf.org/2007/opf", Local: "package"},
		Attr: []xml.Attr{
			{Name: xml.Name{Local: "unique-identifier"}, Value: "uuid_id"},
			{Name: xml.Name{Local: "version"}, Value: "2.0"},
		},
	})

	// <metadata>
	enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "metadata"}, Attr: []xml.Attr{
		{Name: xml.Name{Local: "xmlns:dc"}, Value: "http://purl.org/dc/elements/1.1/"},
		{Name: xml.Name{Local: "xmlns:opf"}, Value: "http://www.idpf.org/2007/opf"},
	}})

	writeDC := func(local, value string) {
		if value == "" {
			return
		}
		start := xml.StartElement{Name: xml.Name{Local: "dc:" + local}}
		enc.EncodeToken(start)
		enc.EncodeToken(xml.CharData(value))
		enc.EncodeToken(xml.EndElement{Name: start.Name})
	}

	writeDC("title", opf.Metadata.Title)
	writeDC("description", opf.Metadata.Description)
	writeDC("publisher", opf.Metadata.Publisher)
	writeDC("date", opf.Metadata.Date)
	for _, l := range opf.Metadata.Languages {
		writeDC("language", l)
	}

	// package@unique-identifier points at "uuid_id"; make sure exactly one
	// identifier carries it without clobbering ids already present.
	uuidIdx := -1
	for i, id := range opf.Metadata.Identifiers {
		if id.ID == "uuid_id" {
			uuidIdx = i
			break
		}
	}
	if uuidIdx == -1 {
		for i, id := range opf.Metadata.Identifiers {
			if id.ID == "" && strings.EqualFold(id.Scheme, "uuid") {
				uuidIdx = i
				break
			}
		}
	}
	if uuidIdx == -1 {
		for i, id := range opf.Metadata.Identifiers {
			if id.ID == "" {
				uuidIdx = i
				break
			}
		}
	}

	for i, id := range opf.Metadata.Identifiers {
		start := xml.StartElement{Name: xml.Name{Local: "dc:identifier"}}
		if id.Scheme != "" {
			start.Attr = append(start.Attr, xml.Attr{
				Name: xml.Name{Local: "opf:scheme"}, Value: id.Scheme,
			})
		}
		idAttr := id.ID
		if i == uuidIdx {
			idAttr = "uuid_id"
		}
		if idAttr != "" {
			start.Attr = append(start.Attr, xml.Attr{
				Name: xml.Name{Local: "id"}, Value: idAttr,
			})
		}
		enc.EncodeToken(start)
		enc.EncodeToken(xml.CharData(id.Value))
		enc.EncodeToken(xml.EndElement{Name: start.Name})
	}

	for _, s := range opf.Metadata.Subjects {
		writeDC("subject", s)
	}
	for _, c := range opf.Metadata.Creators {
		writeDC("creator", c)
	}

	/* NOTE: xml marshaling doesn't support self-closing elements, but Calibre uses them.
	   (e.g. <meta name="..." content="..."/> instead of <meta name="..." content="..."></meta>)
	   in the future, we might want to rewrite the xml to self-close,
	   but I don't think any decoders really care about the difference
	*/
	for _, m := range opf.Metadata.Meta {
		attrs := []xml.Attr{}
		if m.Name != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "name"}, Value: m.Name})
		}
		if m.Content != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "content"}, Value: m.Content})
		}
		if m.Property != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "property"}, Value: m.Property})
		}
		if m.FileAs != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "file-as"}, Value: m.FileAs})
		}
		start := xml.StartElement{Name: xml.Name{Local: "meta"}, Attr: attrs}
		enc.EncodeToken(start)
		if m.Value != "" {
			enc.EncodeToken(xml.CharData(m.Value))
		}
		enc.EncodeToken(xml.EndElement{Name: start.Name})
	}

	enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "metadata"}})
	enc.EncodeToken(xml.EndElement{Name: xml.Name{Space: "http://www.idpf.org/2007/opf", Local: "package"}})

	return enc.Flush()
}

// getMeta returns the content value for a named meta entry, or "" if not found.
func (m *OPFMetadata) getMeta(name string) string {
	for _, meta := range m.Meta {
		if meta.Name == name {
			return meta.Content
		}
	}
	return ""
}

// cleanLanguages trims, drops empties and "und", and de-duplicates
// (case-insensitively) while keeping the original order.
func cleanLanguages(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.TrimSpace(l)
		k := strings.ToLower(l)
		if l == "" || k == undefinedLanguage || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out
}

// parseLanguagesString splits the editor's "eng, fra" field. Language codes
// never contain commas, semicolons or spaces, so this is lossless.
func parseLanguagesString(s string) []string {
	return cleanLanguages(strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}))
}
