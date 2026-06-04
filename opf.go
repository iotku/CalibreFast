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
