package main

// Finding ISBNs in a book's own files, for the metadata editor's "Find
// ISBNs" (/metadata/{uuid}/isbns). Nothing here writes: the editor lists the
// candidates and the admin picks one, which is saved like any other edit.
//
// Where it looks, in each format:
//   - EPUB: the package's dc:identifier values (and dc:source, the print
//     edition), then the text of the first and last few spine documents,
//     where the copyright page usually is.
//   - MOBI/AZW3: EXTH record 104.
//   - PDF: the editor reads the XMP packet and the text of the first and
//     last few pages with pdf.js and posts them here, since Go has no
//     reliable PDF text extraction. Parsing, checking and ranking all
//     happen here, so there's one implementation.

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// isbnCandidate is one ISBN found in a book's files (TS: IsbnCandidate in
// web/types.ts).
type isbnCandidate struct {
	ISBN13 string `json:"isbn13"`
	File   string `json:"file"`
	Source string `json:"source"`          // "opf", "exth", "xmp" or "text"
	Label  string `json:"label,omitempty"` // edition named next to it: "ebook", "hardcover", …
}

// isbnResponse is what /metadata/{uuid}/isbns returns (TS: IsbnResponse).
type isbnResponse struct {
	Candidates []isbnCandidate `json:"candidates"`
	// PDF names the book's PDF when it has one. Only the editor can read it
	// (with pdf.js); it posts what it finds back as pdfISBNSources.
	PDF string `json:"pdf,omitempty"`
}

// pdfISBNSources is what the editor read from the book's PDF.
type pdfISBNSources struct {
	XMP   string   `json:"xmp"`   // the XMP metadata packet, if any
	Pages []string `json:"pages"` // text of the first and last few pages
}

// isbnCandidatesHandler: GET looks in the book's EPUB, MOBI and AZW3 files;
// POST does the same and also parses what the editor read from its PDF.
func isbnCandidatesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	uuid := r.PathValue("uuid")
	bookPath, err := resolveBookPath(uuid)
	if err != nil {
		http.Error(w, "book not found", http.StatusNotFound)
		return
	}
	var pdf *pdfISBNSources
	if r.Method == http.MethodPost {
		pdf = &pdfISBNSources{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(pdf); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	resp := findBookISBNs(filepath.Join(baseDir, bookPath), resolveFormats(uuid, baseDir, bookPath), pdf)
	w.Header().Set("Content-Type", "application/json")
	logErr(json.NewEncoder(w).Encode(resp), "could not encode ISBN candidates")
}

// findBookISBNs looks through a book's files (names in dir), best first.
func findBookISBNs(dir string, files []string, pdf *pdfISBNSources) isbnResponse {
	var resp isbnResponse
	var all []isbnCandidate
	for _, f := range files {
		p := filepath.Join(dir, f)
		var found []isbnCandidate
		var err error
		switch strings.ToLower(filepath.Ext(f)) {
		case ".epub":
			found, err = epubISBNs(p, f)
		case ".mobi", ".azw3":
			found, err = mobiISBNs(p, f)
		case ".pdf":
			// /download/<uuid>/pdf serves the first PDF, so that's the one
			// the editor reads.
			if resp.PDF == "" {
				resp.PDF = f
				if pdf != nil {
					found = pdfISBNs(*pdf, f)
				}
			}
		}
		if err != nil {
			log.Printf("isbns: %s: %v", p, err)
		}
		all = append(all, found...)
	}
	resp.Candidates = rankISBNs(all)
	return resp
}

// ---------------------------------------------------------------------------
// Checking and normalising
// ---------------------------------------------------------------------------

// isbnPrefixRe matches how ISBNs are introduced: "ISBN ", "ISBN-13: ",
// "urn:isbn:" and so on.
var isbnPrefixRe = regexp.MustCompile(`(?i)^(?:urn:)?isbn(?:-1[03]|1[03]\s*:)?[\s:]*`)

// isISBNSeparator: hyphens, dashes and spaces written between ISBN digits.
func isISBNSeparator(r rune) bool {
	return r == ' ' || r == ' ' || isHyphen(r)
}

func isHyphen(r rune) bool { return r == '-' || (r >= '‐' && r <= '–') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// normalizeISBN returns the ISBN-13 for an ISBN-10 or ISBN-13, written with
// or without separators and an "ISBN"/"urn:isbn:" prefix. It returns "" for
// anything else, including a wrong check digit.
func normalizeISBN(s string) string {
	s = isbnPrefixRe.ReplaceAllString(strings.TrimSpace(s), "")
	d := make([]byte, 0, 13)
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			d = append(d, byte(r))
		case r == 'X' || r == 'x':
			d = append(d, 'X')
		case isISBNSeparator(r):
		default:
			return ""
		}
	}
	switch {
	case len(d) == 13 && isbn13Valid(string(d)):
		return string(d)
	case len(d) == 10 && isbn10Valid(string(d)):
		return isbn10To13(string(d))
	}
	return ""
}

// isbn10Valid checks an ISBN-10's check digit: the digits weighted 10 down
// to 1 sum to a multiple of 11, with a final X standing for 10.
func isbn10Valid(s string) bool {
	if len(s) != 10 {
		return false
	}
	sum := 0
	for i := 0; i < 10; i++ {
		var v int
		switch c := s[i]; {
		case isDigit(c):
			v = int(c - '0')
		case (c == 'X' || c == 'x') && i == 9:
			v = 10
		default:
			return false
		}
		sum += v * (10 - i)
	}
	return sum%11 == 0
}

// isbn13Valid checks an ISBN-13 (an EAN-13 starting 978 or 979): the digits
// weighted 1, 3, 1, 3, … sum to a multiple of 10.
func isbn13Valid(s string) bool {
	if len(s) != 13 || !(strings.HasPrefix(s, "978") || strings.HasPrefix(s, "979")) {
		return false
	}
	return ean13Sum(s)%10 == 0
}

// ean13Sum is the weighted sum of s's digits (1, 3, 1, 3, …).
func ean13Sum(s string) int {
	sum := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return -1
		}
		d := int(s[i] - '0')
		if i%2 == 1 {
			d *= 3
		}
		sum += d
	}
	return sum
}

// isbn10To13 converts a valid ISBN-10: prefix 978, drop the old check
// digit, compute the EAN one.
func isbn10To13(s string) string {
	core := "978" + s[:9]
	return core + strconv.Itoa((10-ean13Sum(core)%10)%10)
}

// identifierISBN is the ISBN-13 an identifier holds: one typed as an ISBN,
// written "urn:isbn:…", or a bare ISBN with no type. Identifiers of other
// types are skipped (an Amazon ASIN can look like an ISBN-10).
func identifierISBN(scheme, value string) string {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "" && scheme != "isbn" && !isbnPrefixRe.MatchString(strings.TrimSpace(value)) {
		return ""
	}
	return normalizeISBN(value)
}

// ---------------------------------------------------------------------------
// Ranking
// ---------------------------------------------------------------------------

// isbnRank orders candidates: ebook editions first (copyright pages list
// the print ISBNs too), then ISBNs declared in metadata, then other ISBNs
// from the text, then those labelled as print or audio editions.
func isbnRank(c isbnCandidate) int {
	switch c.Label {
	case "ebook", "epub", "pdf", "kindle", "digital":
		return 0
	case "":
		if c.Source != "text" {
			return 1
		}
		return 2
	}
	return 3
}

// rankISBNs sorts best first and drops repeats, keeping the best of each.
func rankISBNs(cs []isbnCandidate) []isbnCandidate {
	slices.SortStableFunc(cs, func(a, b isbnCandidate) int { return isbnRank(a) - isbnRank(b) })
	out := make([]isbnCandidate, 0, len(cs))
	seen := map[string]bool{}
	for _, c := range cs {
		if !seen[c.ISBN13] {
			seen[c.ISBN13] = true
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Text (copyright pages)
// ---------------------------------------------------------------------------

const isbnSep = `[-\x{2010}-\x{2013} \x{00A0}]`

// isbnNumberRe matches an ISBN-13 (978/979 and 10 more digits) or an
// ISBN-10, with optional separators between the digits. Matches are checked
// further in isbnsInLine.
var isbnNumberRe = regexp.MustCompile(`(?:^|[^0-9])(97[89](?:` + isbnSep + `?[0-9]){10}|[0-9](?:` + isbnSep + `?[0-9]){8}` + isbnSep + `?[0-9Xx])`)

// isbnLabelRe matches the words copyright pages use to say which edition an
// ISBN is for; canonicalISBNLabel names them.
var isbnLabelRe = regexp.MustCompile(`(?i)\b(e-?isbn|e-?book|electronic|digital|epub|kindle|mobi|pdf|hard ?cover|hardback|hbk|hb|cloth|casebound|paperback|soft ?cover|pbk|pb|trade paper|print|alk\. paper|audio(?:book)?)\b`)

// isbnBracketRe: "(ebook)" or "[pbk. : alk. paper]" right after a number.
var isbnBracketRe = regexp.MustCompile(`^\s*[(\[]([^)\]]*)[)\]]`)

func canonicalISBNLabel(word string) string {
	switch strings.ToLower(strings.NewReplacer("-", "", " ", "", ".", "").Replace(word)) {
	case "eisbn", "ebook", "electronic":
		return "ebook"
	case "epub":
		return "epub"
	case "pdf":
		return "pdf"
	case "kindle", "mobi":
		return "kindle"
	case "digital":
		return "digital"
	case "hardcover", "hardback", "hbk", "hb", "cloth", "casebound":
		return "hardcover"
	case "paperback", "softcover", "pbk", "pb", "tradepaper":
		return "paperback"
	case "audio", "audiobook":
		return "audiobook"
	}
	return "print" // "print", "alk. paper"
}

func firstISBNLabel(s string) string {
	if m := isbnLabelRe.FindString(s); m != "" {
		return canonicalISBNLabel(m)
	}
	return ""
}

func lastISBNLabel(s string) string {
	if ms := isbnLabelRe.FindAllString(s, -1); len(ms) > 0 {
		return canonicalISBNLabel(ms[len(ms)-1])
	}
	return ""
}

// isbnsInText finds ISBNs in visible text, one block (paragraph, PDF line)
// per line.
func isbnsInText(text, file string) []isbnCandidate {
	var out []isbnCandidate
	for _, line := range strings.Split(text, "\n") {
		out = append(out, isbnsInLine(line, file)...)
	}
	return out
}

// labelReach is how far either side of a number (in bytes) a label or the
// word "ISBN" counts as belonging to it.
const labelReach = 40

// isbnsInLine finds the ISBNs in one line of text. A 13-digit number
// starting 978/979 with a valid check digit counts on its own; a 10-digit
// one only with "ISBN" shortly before it, since a phone number passes the
// ISBN-10 check one time in 11.
//
// Labels: a word in brackets right after the number wins ("… (ebook)").
// Otherwise the line is read in the style its first ISBN sets: a label
// before it ("eBook ISBN: …") means each label precedes its number;
// otherwise each follows its number ("ISBN … hardcover, ISBN … ebook").
func isbnsInLine(line, file string) []isbnCandidate {
	type match struct {
		start, end int
		isbn       string
	}
	var ms []match
	for _, m := range isbnNumberRe.FindAllStringSubmatchIndex(line, -1) {
		start, end := m[2], m[3]
		// Not part of a longer number ("12-978…", "…0440-5").
		if rest := line[end:]; rest != "" {
			r, n := utf8.DecodeRuneInString(rest)
			if isDigit(rest[0]) || (isHyphen(r) && n < len(rest) && isDigit(rest[n])) {
				continue
			}
		}
		if r, n := utf8.DecodeLastRuneInString(line[:start]); isHyphen(r) && start > n && isDigit(line[start-n-1]) {
			continue
		}
		isbn := normalizeISBN(line[start:end])
		if isbn == "" {
			continue
		}
		if len(strings.Map(keepDigitsX, line[start:end])) == 10 &&
			!strings.Contains(strings.ToUpper(line[max(0, start-labelReach):start]), "ISBN") {
			continue
		}
		ms = append(ms, match{start, end, isbn})
	}
	if len(ms) == 0 {
		return nil
	}

	prefixStyle := isbnLabelRe.MatchString(line[max(0, ms[0].start-labelReach):ms[0].start])
	out := make([]isbnCandidate, 0, len(ms))
	for i, m := range ms {
		after := line[m.end:]
		if i+1 < len(ms) {
			after = line[m.end:ms[i+1].start]
		}
		after = after[:min(len(after), labelReach)]
		label := ""
		if b := isbnBracketRe.FindStringSubmatch(after); b != nil {
			label = firstISBNLabel(b[1])
		}
		if label == "" {
			if prefixStyle {
				from := max(0, m.start-labelReach)
				if i > 0 {
					from = max(from, ms[i-1].end)
				}
				label = lastISBNLabel(line[from:m.start])
			} else {
				label = firstISBNLabel(after)
			}
		}
		out = append(out, isbnCandidate{ISBN13: m.isbn, File: file, Source: "text", Label: label})
	}
	return out
}

func keepDigitsX(r rune) rune {
	if (r >= '0' && r <= '9') || r == 'X' || r == 'x' {
		return r
	}
	return -1
}

// Elements whose text isn't shown, and block elements, which end a line.
var (
	textSkipElements  = map[string]bool{"head": true, "script": true, "style": true, "title": true}
	textBlockElements = map[string]bool{
		"address": true, "article": true, "aside": true, "blockquote": true, "body": true, "br": true,
		"dd": true, "div": true, "dl": true, "dt": true, "figcaption": true, "figure": true, "footer": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "header": true, "hr": true,
		"li": true, "nav": true, "ol": true, "p": true, "pre": true, "section": true, "table": true,
		"td": true, "th": true, "tr": true, "ul": true,
	}
)

// xhtmlText is the visible text of an (X)HTML document, one block per line
// and whitespace collapsed. It's lenient: a malformed document gives the
// text up to the error.
func xhtmlText(data []byte) string {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	d.AutoClose = xml.HTMLAutoClose
	d.Entity = xml.HTMLEntity
	d.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var b strings.Builder
	skip := 0
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if textSkipElements[name] {
				skip++
			}
			if textBlockElements[name] {
				b.WriteByte('\n')
			}
		case xml.EndElement:
			name := strings.ToLower(t.Name.Local)
			if textSkipElements[name] && skip > 0 {
				skip--
			}
			if textBlockElements[name] {
				b.WriteByte('\n')
			}
		case xml.CharData:
			if skip == 0 {
				writeCollapsed(&b, string(t))
			}
		}
	}
	return b.String()
}

// writeCollapsed writes s with each run of whitespace (newlines included)
// as one space.
func writeCollapsed(b *strings.Builder, s string) {
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	if space {
		b.WriteByte(' ')
	}
}

// edgeIndexes returns the first `first` and last `last` indexes of n, each
// once.
func edgeIndexes(n, first, last int) []int {
	var out []int
	for i := 0; i < n; i++ {
		if i < first || i >= n-last {
			out = append(out, i)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Formats
// ---------------------------------------------------------------------------

// epubISBNs reads the package's identifiers, then the text of the first 5
// and last 3 spine documents.
func epubISBNs(p, file string) ([]isbnCandidate, error) {
	zr, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	opfPath, err := epubPackagePath(&zr.Reader)
	if err != nil {
		return nil, err
	}
	data, err := readZipEntry(&zr.Reader, opfPath, 16<<20)
	if err != nil {
		return nil, err
	}
	var pkg struct {
		Identifiers []opfIdentifier `xml:"metadata>identifier"`
		Sources     []string        `xml:"metadata>source"`
		Items       []struct {
			ID   string `xml:"id,attr"`
			Href string `xml:"href,attr"`
		} `xml:"manifest>item"`
		Spine []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"spine>itemref"`
	}
	if err := xml.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}

	var out []isbnCandidate
	for _, id := range pkg.Identifiers {
		if isbn := identifierISBN(id.Scheme, id.Value); isbn != "" {
			out = append(out, isbnCandidate{ISBN13: isbn, File: file, Source: "opf"})
		}
	}
	// EPUB 3 names the print edition an ebook was made from in dc:source.
	for _, s := range pkg.Sources {
		if isbn := identifierISBN("", s); isbn != "" {
			out = append(out, isbnCandidate{ISBN13: isbn, File: file, Source: "opf", Label: "print"})
		}
	}

	hrefs := make(map[string]string, len(pkg.Items))
	for _, it := range pkg.Items {
		hrefs[it.ID] = it.Href
	}
	var docs []string
	for _, ref := range pkg.Spine {
		if h := hrefs[ref.IDRef]; h != "" {
			docs = append(docs, h)
		}
	}
	for _, i := range edgeIndexes(len(docs), 5, 3) {
		doc, err := readZipEntry(&zr.Reader, epubHrefPath(opfPath, docs[i]), 4<<20)
		if err != nil {
			continue // a missing or huge chapter doesn't stop the rest
		}
		out = append(out, isbnsInText(xhtmlText(doc), file)...)
	}
	return out, nil
}

// epubHrefPath resolves a manifest href against the package document.
func epubHrefPath(opfPath, href string) string {
	href, _, _ = strings.Cut(href, "#")
	if u, err := url.PathUnescape(href); err == nil {
		href = u
	}
	return path.Join(path.Dir(opfPath), href)
}

// mobiISBNs reads EXTH record 104 (ISBN) of a MOBI or AZW3 file.
func mobiISBNs(p, file string) ([]isbnCandidate, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	values, err := mobiEXTH(f, 104)
	if err != nil {
		return nil, err
	}
	var out []isbnCandidate
	for _, v := range values {
		if isbn := normalizeISBN(v); isbn != "" {
			out = append(out, isbnCandidate{ISBN13: isbn, File: file, Source: "exth"})
		}
	}
	return out, nil
}

// mobiEXTH returns the values of EXTH records of type typ. A MOBI file is a
// Palm database; its first record holds the PalmDOC and MOBI headers and,
// when the MOBI header's flags say so, the EXTH block right after them.
func mobiEXTH(r io.ReaderAt, typ uint32) ([]string, error) {
	var hdr [78 + 16]byte // database header and the first two record entries
	n, err := r.ReadAt(hdr[:], 0)
	if n < 78+8 {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if string(hdr[60:68]) != "BOOKMOBI" {
		return nil, errors.New("not a MOBI file")
	}
	records := binary.BigEndian.Uint16(hdr[76:78])
	if records == 0 {
		return nil, errors.New("no records")
	}
	start := int64(binary.BigEndian.Uint32(hdr[78:82]))
	size := int64(1 << 20)
	if records > 1 && n >= 78+16 {
		if next := int64(binary.BigEndian.Uint32(hdr[86:90])); next > start && next-start < size {
			size = next - start
		}
	}
	rec := make([]byte, size)
	n, err = r.ReadAt(rec, start)
	if n == 0 && err != nil {
		return nil, err
	}
	return exthRecords(rec[:n], typ)
}

// exthRecords reads EXTH records of type typ from a MOBI file's first
// record.
func exthRecords(rec []byte, typ uint32) ([]string, error) {
	be := binary.BigEndian
	if len(rec) < 0x84 || string(rec[16:20]) != "MOBI" {
		return nil, errors.New("no MOBI header")
	}
	if be.Uint32(rec[0x80:0x84])&0x40 == 0 {
		return nil, nil // no EXTH block
	}
	p := 16 + int64(be.Uint32(rec[20:24])) // after the MOBI header
	if p+12 > int64(len(rec)) || string(rec[p:p+4]) != "EXTH" {
		return nil, errors.New("EXTH block missing")
	}
	count := be.Uint32(rec[p+8 : p+12])
	p += 12
	var out []string
	for i := uint32(0); i < count && p+8 <= int64(len(rec)); i++ {
		t, l := be.Uint32(rec[p:p+4]), int64(be.Uint32(rec[p+4:p+8]))
		if l < 8 || p+l > int64(len(rec)) {
			break
		}
		if t == typ {
			out = append(out, string(rec[p+8:p+l]))
		}
		p += l
	}
	return out, nil
}

// pdfISBNs parses what the editor read from a PDF: its XMP packet, then its
// pages' text.
func pdfISBNs(src pdfISBNSources, file string) []isbnCandidate {
	out := xmpISBNs(src.XMP, file)
	for _, page := range src.Pages {
		out = append(out, isbnsInText(page, file)...)
	}
	return out
}

// xmpISBNs finds ISBNs in an XMP packet: properties named like an ISBN
// (prism:isbn, prism:eIsbn, pdfx:ISBN, …), as elements or attributes, and
// dc:identifier.
func xmpISBNs(xmp, file string) []isbnCandidate {
	var out []isbnCandidate
	add := func(name, value string) {
		lname := strings.ToLower(name)
		var isbn string
		switch {
		case strings.Contains(lname, "isbn"):
			isbn = normalizeISBN(value)
		case lname == "identifier":
			isbn = identifierISBN("", value)
		}
		if isbn == "" {
			return
		}
		label := ""
		switch {
		case strings.Contains(lname, "eisbn"):
			label = "ebook"
		case strings.Contains(lname, "print"):
			label = "print"
		}
		out = append(out, isbnCandidate{ISBN13: isbn, File: file, Source: "xmp", Label: label})
	}

	d := xml.NewDecoder(strings.NewReader(xmp))
	d.Strict = false
	var stack []string
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			for _, a := range t.Attr {
				if a.Name.Space != "xmlns" {
					add(a.Name.Local, a.Value)
				}
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			// The property is the nearest element that isn't an RDF
			// container: dc:identifier > rdf:Bag > rdf:li.
			for i := len(stack) - 1; i >= 0; i-- {
				if n := stack[i]; n != "li" && n != "Bag" && n != "Seq" && n != "Alt" {
					add(n, string(t))
					break
				}
			}
		}
	}
	return out
}
