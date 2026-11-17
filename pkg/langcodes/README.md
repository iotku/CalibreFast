# langcodes

`import "github.com/iotku/CalibreFast/pkg/langcodes"`

Display names and canonical codes for the language codes found in ebook
metadata.

```go
langcodes.DisplayName("eng")        // "English", true
langcodes.DisplayName("zh_hant-tw") // "Chinese (Hant-TW)", true
langcodes.DisplayName("xyz")        // "xyz", false

langcodes.Canonical("en")  // "eng"
langcodes.Canonical("ger") // "deu" (ISO 639-2/T, as Calibre stores it)
langcodes.Canonical("XX")  // "xx"  (unknown: kept, lowercased)

langcodes.Codes()["fre"] // Code{Name: "French", Canonical: "fra"}
```

`Codes()` is the whole table as a map, with JSON tags (`name`, `code`),
suitable for sending to a browser.

## Coverage and limits

- About 100 languages (the most widely spoken, plus ones common in book
  collections such as Latin, Ancient Greek and Esperanto), and the special
  codes `und`, `mul` and `zxx`. Unknown codes aren't errors: `DisplayName`
  returns the code itself with `ok == false`.
- Subtags after the language (`-US`, `-Hant`) are shown in conventional case
  but not looked up or validated.
- Names are English only.
