// Package mobi reads Kindle books (MOBI, AZW, AZW3/KF8, and the joint
// MOBI+KF8 files KindleGen writes) and converts them to EPUB 3.
//
// It handles the PDB container, the MOBI and EXTH headers, PalmDOC and
// HUFF/CDIC text compression, the KF8 skeleton, fragment, flow and NCX
// indexes, embedded images and (obfuscated, compressed) fonts, and the
// kindle:pos:fid, kindle:embed, kindle:flow and MOBI 6 filepos links. A joint
// file is read through its KF8 half.
//
// Every value in a book file is treated as hostile: each offset, count and
// length is checked, decompression output is capped (see [Limits]), and the
// markup is re-parsed and written out as well-formed XHTML without scripts,
// event handler attributes, frames, objects or javascript: URLs.
//
// DRM-protected books can't be read: [Open] returns [ErrDRM].
package mobi
