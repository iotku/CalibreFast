// Package koreader reads and writes what KOReader keeps on the device about
// a book, so that a server or another reader can exchange it:
//
//   - reading positions in EPUBs: [XPointerToCFI] and [CFIToXPointer]
//     convert between KOReader's (crengine's) XPointers and the EPUB CFIs
//     that web readers such as epub.js use;
//   - highlights: [File] writes KOReader's annotations file
//     ("<book file name>.annotations.lua") and [Annotations] reads one, for
//     EPUBs (positions converted at character level) and PDFs (positions in
//     points on both sides);
//   - KOReader's Lua data files: [ParseLuaTable] reads them as data (nothing
//     is executed) and [WriteLua] writes them as KOReader's own dump does.
//
// It doesn't depend on the sync protocol (package kosync), which carries
// positions but not highlights.
package koreader
