package main

import (
	"encoding/xml"
	"os"
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
	Language    string          `xml:"http://purl.org/dc/elements/1.1/ language"`
	Identifiers []opfIdentifier `xml:"http://purl.org/dc/elements/1.1/ identifier"`
	Subjects    []string        `xml:"http://purl.org/dc/elements/1.1/ subject"`
	Creators    []string        `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Meta        []opfMeta       `xml:"meta"`
}

type opfIdentifier struct {
	Scheme string `xml:"scheme,attr"`
	Value  string `xml:",chardata"`
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
	writeDC("language", opf.Metadata.Language)

	for i, id := range opf.Metadata.Identifiers {
		start := xml.StartElement{
			Name: xml.Name{Local: "dc:identifier"},
			Attr: []xml.Attr{
				{Name: xml.Name{Local: "opf:scheme"}, Value: id.Scheme},
			},
		}
		if i == 0 {
			start.Attr = append(start.Attr, xml.Attr{
				Name:  xml.Name{Local: "id"},
				Value: "uuid_id",
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
