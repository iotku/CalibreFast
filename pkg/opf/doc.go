// Package opf reads and writes the metadata of OPF package documents, as
// Calibre keeps them in each book's metadata.opf and as EPUBs carry them.
//
// It covers the Dublin Core elements Calibre uses (title, creators,
// description, publisher, date, languages, identifiers, subjects) and the
// <meta> elements, including Calibre's calibre:series/calibre:series_index
// and EPUB 3's belongs-to-collection, which [Metadata.FillSeries] and
// [Metadata.SyncSeriesMeta] map to and from Series and SeriesIndex.
//
// [Write] produces a Calibre-style OPF 2.0 file whose
// package@unique-identifier always points at the book's uuid identifier,
// never at an ISBN or other typed identifier.
//
// Spec reference: https://idpf.org/epub/20/spec/OPF_2.0_final_spec.html#Section2.2
// (this is not a complete or validating implementation).
package opf
