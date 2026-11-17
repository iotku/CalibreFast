// Package isbn finds, checks and ranks ISBNs in ebook files.
//
// Where it looks, in each format:
//   - EPUB: the package's dc:identifier values (and dc:source, the print
//     edition), then the text of the first and last few spine documents,
//     where the copyright page usually is.
//   - MOBI/AZW3: EXTH record 104.
//   - PDF: Go has no reliable PDF text extraction, so the caller supplies
//     the XMP packet and the text of the first and last few pages (CalibreFast
//     reads them in the browser with pdf.js). Parsing, checking and ranking
//     all happen here, so there's one implementation.
package isbn
