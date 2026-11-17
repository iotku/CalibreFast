// Package epubzip reads the parts of an EPUB's zip archive that metadata
// tools need: where the package document (OPF) is, a size-bounded read of
// any entry, the resolution of manifest hrefs to zip paths, and the cover
// image the package declares.
//
// It reads XML 1.1 package documents too (see package xml11). It doesn't
// parse the OPF's metadata; package opf does that.
package epubzip
