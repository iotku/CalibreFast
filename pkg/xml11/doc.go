// Package xml11 lets encoding/xml read XML 1.1 documents.
//
// encoding/xml reads only XML 1.0: it refuses a document whose declaration
// says version="1.1", and it refuses character references to the C0 control
// characters that XML 1.1 allows (&#x1; to &#x1F;). Some EPUBs declare 1.1,
// so every XML document should go through [XML11To10] first (or be decoded
// with [Unmarshal]), which turns an XML 1.1 document into the XML 1.0
// document that means the same thing as far as a reader of its text can tell:
//
//   - The declaration's version becomes "1.0".
//   - XML 1.1's extra line ends (NEL, U+0085, alone or after a CR, and LSEP,
//     U+2028) become "\n", as an XML 1.1 parser normalizes them (§2.11).
//   - References to the C0 controls XML 1.0 lacks are dropped: XML 1.0, and
//     so encoding/xml, cannot hold those characters, and they have no
//     visible text to keep. References inside CDATA sections, comments and
//     processing instructions are literal text there and are left alone.
//
// A document that isn't XML 1.1 is returned as is.
package xml11
