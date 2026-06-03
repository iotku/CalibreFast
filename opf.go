package main

import (
	"encoding/xml"
	"os"
)

type OPF struct {
	XMLName  xml.Name    `xml:"http://www.idpf.org/2007/opf package"`
	Metadata OPFMetadata `xml:"metadata"`
}

type OPFMetadata struct {
	Title       string    `xml:"http://purl.org/dc/elements/1.1/ title"`
	Description string    `xml:"http://purl.org/dc/elements/1.1/ description"`
	Publisher   string    `xml:"http://purl.org/dc/elements/1.1/ publisher"`
	Date        string    `xml:"http://purl.org/dc/elements/1.1/ date"`
	Language    string    `xml:"http://purl.org/dc/elements/1.1/ language"`
	Identifiers []string  `xml:"http://purl.org/dc/elements/1.1/ identifier"`
	Subjects    []string  `xml:"http://purl.org/dc/elements/1.1/ subject"`
	Creators    []string  `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Meta        []opfMeta `xml:"meta"` // TODO Verifiy this is actually a part of the OPF spec
}

type opfMeta struct { // TODO: Verify this is actually a part of the OPF spec
	Name     string `xml:"name,attr"`
	Content  string `xml:"content,attr"`
	Property string `xml:"property,attr"` // EPUB3
	Value    string `xml:",chardata"`

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
