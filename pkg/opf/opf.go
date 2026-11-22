package opf

import (
	"encoding/xml"
	"os"
	"slices"
	"strings"

	"github.com/iotku/CalibreFast/pkg/xml11"
)

// Package is an OPF package document, as far as its metadata.
type Package struct {
	XMLName  xml.Name `xml:"http://www.idpf.org/2007/opf package"`
	Metadata Metadata `xml:"metadata"`
}

// Metadata is the <metadata> of an OPF package: the Dublin Core elements
// Calibre reads and writes, and the <meta> elements, plus the series as
// FillSeries reads it. Its JSON form is used by CalibreFast's editor.
type Metadata struct {
	Title       string       `xml:"http://purl.org/dc/elements/1.1/ title"`
	Description string       `xml:"http://purl.org/dc/elements/1.1/ description"`
	Publisher   string       `xml:"http://purl.org/dc/elements/1.1/ publisher"`
	Date        string       `xml:"http://purl.org/dc/elements/1.1/ date"`
	Languages   []string     `xml:"http://purl.org/dc/elements/1.1/ language"` // in order; first is primary
	Identifiers []Identifier `xml:"http://purl.org/dc/elements/1.1/ identifier"`
	Subjects    []string     `xml:"http://purl.org/dc/elements/1.1/ subject"`
	Creators    []string     `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Meta        []Meta       `xml:"meta"`

	// The series, for the editor (JSON only; series.go): read from the
	// calibre:series meta (or EPUB 3's belongs-to-collection) by FillSeries,
	// written back by SyncSeriesMeta.
	Series      string `xml:"-" json:",omitempty"`
	SeriesIndex string `xml:"-" json:",omitempty"` // as typed: "2", "2.5"
	// SeriesSet: the editor sent a series (perhaps none): saving replaces
	// the book's. Otherwise the book's series is left as it is.
	SeriesSet bool `xml:"-" json:"-"`

	// The rating, 0–10 half-stars as Calibre keeps it (JSON only; rating.go):
	// read from calibre:rating by FillRating, written back by SyncRatingMeta.
	Rating int `xml:"-" json:",omitempty"`
	// RatingSet: the editor sent a rating (perhaps none): saving replaces
	// the book's. Otherwise it's left as it is.
	RatingSet bool `xml:"-" json:"-"`
}

// Identifier is a <dc:identifier>: an ISBN, a uuid, Calibre's book id, ….
type Identifier struct {
	Scheme string `xml:"scheme,attr"` // may be empty: such identifiers must still round-trip
	Value  string `xml:",chardata"`
	ID     string `xml:"id,attr"` // e.g. "uuid_id", "calibre_id"; preserved on rewrite
}

// Library identifiers are bookkeeping, which Calibre writes into the OPF
// and its exports: the book's id ("calibre") and uuid, and an EPUB's package
// uuid (no scheme, "urn:uuid:…"). The editor doesn't show them, and saving
// keeps the book's own as they are.
var libraryIdentifierSchemes = map[string]bool{"calibre": true, "uuid": true}

// IsLibraryIdentifier reports whether id is library bookkeeping (see above)
// rather than an identifier of the work, such as an ISBN.
func IsLibraryIdentifier(id Identifier) bool {
	scheme := strings.TrimSpace(id.Scheme)
	if scheme == "" {
		value := strings.TrimSpace(id.Value)
		return len(value) > 9 && strings.EqualFold(value[:9], "urn:uuid:")
	}
	return libraryIdentifierSchemes[strings.ToLower(scheme)]
}

// SplitLibraryIdentifiers separates library identifiers from the others,
// keeping each group's order.
func SplitLibraryIdentifiers(ids []Identifier) (library, others []Identifier) {
	for _, id := range ids {
		if IsLibraryIdentifier(id) {
			library = append(library, id)
		} else {
			others = append(others, id)
		}
	}
	return library, others
}

// Meta is a <meta> element: EPUB 2's name/content pairs, or EPUB 3's
// property/refines with the value as text.
type Meta struct {
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Property string `xml:"property,attr"` // EPUB3
	Value    string `xml:",chardata"`     // You probably want Content
	ID       string `xml:"id,attr"`       // EPUB3: what a refines="#id" points at
	Refines  string `xml:"refines,attr"`  // EPUB3

	FileAs string `xml:"file-as,attr"`
}

// Load reads the OPF file at path (XML 1.0 or 1.1) and fills in the series.
func Load(path string) (*Package, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var opf Package

	err = xml11.Unmarshal(data, &opf)
	if err != nil {
		return nil, err
	}
	opf.Metadata.FillSeries()
	return &opf, nil
}

// uuidIdentifierID is the id that package@unique-identifier points at. In
// Calibre's own metadata.opf it is on <dc:identifier opf:scheme="uuid">,
// whose value is the book's uuid in metadata.db.
const uuidIdentifierID = "uuid_id"

// Write writes opf to path. bookUUID is the book's uuid in metadata.db
// ("" if unknown); see identifiersForWrite for what it does to identifiers.
func Write(opf *Package, path string, bookUUID string) error {
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
			{Name: xml.Name{Local: "unique-identifier"}, Value: uuidIdentifierID},
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

	for _, id := range identifiersForWrite(opf.Metadata.Identifiers, bookUUID) {
		start := xml.StartElement{Name: xml.Name{Local: "dc:identifier"}}
		if id.Scheme != "" {
			start.Attr = append(start.Attr, xml.Attr{
				Name: xml.Name{Local: "opf:scheme"}, Value: id.Scheme,
			})
		}
		if id.ID != "" {
			start.Attr = append(start.Attr, xml.Attr{
				Name: xml.Name{Local: "id"}, Value: id.ID,
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
		if m.ID != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "id"}, Value: m.ID})
		}
		if m.Refines != "" {
			attrs = append(attrs, xml.Attr{Name: xml.Name{Local: "refines"}, Value: m.Refines})
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

// identifiersForWrite returns the identifiers to write, with id="uuid_id" on
// exactly one of them: the book's UUID identifier. It never goes on one typed
// as something else, such as an ISBN. Values are never changed or dropped,
// and other identifiers keep their ids, except that a second "uuid_id" is
// cleared, because ids must be unique. ids itself is not modified.
//
// With bookUUID (the book's uuid in metadata.db), the UUID identifier is the
// one holding that uuid, bare or as "urn:uuid:…". If none does, a
// Calibre-style <dc:identifier opf:scheme="uuid"> is added first, so the file
// always names the book it belongs to and Calibre can rebuild its database
// from it.
//
// Without bookUUID, it is the identifier already called uuid_id, else one
// whose value is "urn:uuid:…", else one with scheme "uuid". If there is none,
// no identifier gets uuid_id.
func identifiersForWrite(ids []Identifier, bookUUID string) []Identifier {
	out := slices.Clone(ids)
	bookUUID = strings.TrimSpace(bookUUID)
	hasUUIDScheme := func(id Identifier) bool { return strings.EqualFold(strings.TrimSpace(id.Scheme), "uuid") }
	isHolder := func(id Identifier) bool { return id.ID == uuidIdentifierID }

	var pick int
	if bookUUID != "" {
		holdsBook := func(id Identifier) bool {
			return !typedAsOther(id) && strings.EqualFold(bareUUID(id.Value), bookUUID)
		}
		pick = firstMatch(out,
			func(id Identifier) bool { return holdsBook(id) && isHolder(id) },
			func(id Identifier) bool { return holdsBook(id) && hasUUIDScheme(id) },
			holdsBook,
		)
		if pick == -1 {
			out = slices.Insert(out, 0, Identifier{Scheme: "uuid", Value: bookUUID})
			pick = 0
		}
	} else {
		pick = firstMatch(out,
			func(id Identifier) bool { return isHolder(id) && !typedAsOther(id) },
			func(id Identifier) bool { return !typedAsOther(id) && isURNUUID(id.Value) },
			hasUUIDScheme,
		)
	}

	for i := range out {
		switch {
		case i == pick:
			out[i].ID = uuidIdentifierID
		case isHolder(out[i]):
			out[i].ID = ""
		}
	}
	return out
}

// firstMatch returns the index of the first identifier matching the first
// predicate that matches any, or -1.
func firstMatch(ids []Identifier, preds ...func(Identifier) bool) int {
	for _, p := range preds {
		if i := slices.IndexFunc(ids, p); i != -1 {
			return i
		}
	}
	return -1
}

// typedAsOther reports whether an identifier says it is something other than
// a UUID: an opf:scheme other than "uuid" (isbn, calibre, …), or a URN of
// another kind ("urn:isbn:…").
func typedAsOther(id Identifier) bool {
	if s := strings.TrimSpace(id.Scheme); s != "" {
		return !strings.EqualFold(s, "uuid")
	}
	v := strings.ToLower(strings.TrimSpace(id.Value))
	return strings.HasPrefix(v, "urn:") && !isURNUUID(v)
}

const urnUUIDPrefix = "urn:uuid:"

// isURNUUID reports whether v starts with "urn:uuid:" (any case).
func isURNUUID(v string) bool {
	v = strings.TrimSpace(v)
	return len(v) >= len(urnUUIDPrefix) && strings.EqualFold(v[:len(urnUUIDPrefix)], urnUUIDPrefix)
}

// bareUUID trims v and strips a "urn:uuid:" prefix.
func bareUUID(v string) string {
	v = strings.TrimSpace(v)
	if isURNUUID(v) {
		return strings.TrimSpace(v[len(urnUUIDPrefix):])
	}
	return v
}

// GetMeta returns the content value for a named meta entry, or "" if not found.
func (m *Metadata) GetMeta(name string) string {
	for _, meta := range m.Meta {
		if meta.Name == name {
			return meta.Content
		}
	}
	return ""
}

// CleanLanguages trims, drops empties and "und", and de-duplicates
// (case-insensitively) while keeping the original order.
func CleanLanguages(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, l := range in {
		l = strings.TrimSpace(l)
		k := strings.ToLower(l)
		if l == "" || k == UndefinedLanguage || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out
}

// ParseLanguages splits the editor's "eng, fra" field. Language codes
// never contain commas, semicolons or spaces, so this is lossless.
func ParseLanguages(s string) []string {
	return CleanLanguages(strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}))
}
