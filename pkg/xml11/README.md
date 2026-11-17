# xml11

`import "github.com/iotku/CalibreFast/pkg/xml11"`

Go's `encoding/xml` only reads XML 1.0. It rejects documents that declare
`version="1.1"` and character references to the C0 controls XML 1.1 allows
(`&#x1;` to `&#x1F;`). Some EPUBs declare XML 1.1 for their container, package
or content documents, so they can't be read as they are.

This package rewrites an XML 1.1 document as the XML 1.0 document a reader
would see:

- the declaration's version becomes `1.0`;
- XML 1.1's extra line ends (NEL `U+0085`, CR NEL, and LSEP `U+2028`) become
  `\n` (UTF-8 documents only);
- references to C0 controls other than tab, LF and CR are dropped, except
  inside CDATA sections, comments and processing instructions.

Documents that aren't XML 1.1 are returned unchanged, without copying.

## Usage

```go
// Decode in one step.
var pkg struct{ Title string `xml:"metadata>title"` }
err := xml11.Unmarshal(data, &pkg)

// Or feed a streaming decoder.
d := xml.NewDecoder(bytes.NewReader(xml11.XML11To10(data)))
```

## Limits

- The rewrite works on the raw bytes. Line-end normalization only happens for
  UTF-8 documents (the default, or `encoding="utf-8"`); other encodings are
  left to their decoder.
- Dropped control characters are lost, not replaced.
- It doesn't validate the document; `encoding/xml` still reports any other
  errors.
