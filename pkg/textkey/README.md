# textkey

`import "github.com/iotku/CalibreFast/pkg/textkey"`

Keys for comparing and sorting text the way a reader would.

- `Fold(s)`: lower case, no accents or punctuation, `&` as "and", spaces
  collapsed; apostrophes and periods join what's around them
  (`"Caliban’s War"` → `"calibans war"`).
- `Name(name)`: a person's name with its words folded and sorted, so
  `"James S.A. Corey"` and `"Corey, James S. A."` match.
- `LooseName(name)`: surname and first initial (`"tolkien j"`), for near
  matches; `""` for one-word names.
