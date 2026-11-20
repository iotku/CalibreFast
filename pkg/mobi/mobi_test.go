package mobi

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The fixtures are Calibre conversions of testdata/tiny_source.epub (see
// testdata/README.md).
var fixtures = []struct {
	file string
	kf8  bool
}{
	{"testdata/tiny.azw3", true},
	{"testdata/tiny_joint.mobi", true},
	{"testdata/tiny_mobi6.mobi", false},
}

var reTestLink = regexp.MustCompile(`<a href="([^"]+)"[^>]*>the target in chapter three</a>`)

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func convert(t *testing.T, data []byte) []byte {
	t.Helper()
	b, err := Open(data)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := b.WriteEPUB(&out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// epubFiles unzips an EPUB, checking the mimetype entry comes first, stored.
func epubFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if f := zr.File[0]; f.Name != "mimetype" || f.Method != zip.Store {
		t.Fatalf("first entry %q (method %d), want stored mimetype", f.Name, f.Method)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name], _ = io.ReadAll(rc)
		rc.Close()
	}
	return files
}

type xmlDoc struct {
	ids   map[string]bool
	links []string // href and src values
	text  strings.Builder
}

// parseXML checks a document is well-formed XML and collects its ids,
// links and text.
func parseXML(t *testing.T, name string, data []byte) *xmlDoc {
	t.Helper()
	d := &xmlDoc{ids: map[string]bool{}}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s: not well-formed: %v", name, err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			for _, a := range tok.Attr {
				switch a.Name.Local {
				case "id":
					d.ids[a.Value] = true
				case "href", "src":
					d.links = append(d.links, a.Value)
				}
			}
		case xml.CharData:
			d.text.Write(tok)
		}
	}
	return d
}

func TestConvertFixtures(t *testing.T) {
	for _, fx := range fixtures {
		t.Run(path.Base(fx.file), func(t *testing.T) {
			data := readFixture(t, fx.file)
			b, err := Open(data)
			if err != nil {
				t.Fatal(err)
			}
			if b.KF8 != fx.kf8 {
				t.Errorf("KF8 = %v, want %v", b.KF8, fx.kf8)
			}
			m := b.Metadata
			if m.Title != "Tiny Fixture Book" || strings.Join(m.Authors, ";") != "Ada Example;Bo Sample" ||
				m.Publisher != "Fixture Press" || m.Language != "en" || m.ISBN != "9780306406157" {
				t.Errorf("metadata = %+v", m)
			}

			files := epubFiles(t, convert(t, data))
			docs := map[string]*xmlDoc{}
			var text strings.Builder
			for name, body := range files {
				if strings.HasSuffix(name, ".xhtml") || strings.HasSuffix(name, ".opf") ||
					strings.HasSuffix(name, ".ncx") || strings.HasSuffix(name, ".svg") || strings.HasSuffix(name, ".xml") {
					docs[name] = parseXML(t, name, body)
				}
			}
			// Every link and image resolves, anchors included.
			for name, d := range docs {
				if strings.HasSuffix(name, ".opf") {
					continue
				}
				for _, l := range d.links {
					if strings.HasPrefix(l, "http") || strings.HasPrefix(l, "mailto:") {
						continue
					}
					file, frag, _ := strings.Cut(l, "#")
					target := name
					if file != "" {
						target = path.Join(path.Dir(name), file)
					}
					if _, ok := files[target]; !ok {
						t.Errorf("%s: link %q: no %s", name, l, target)
						continue
					}
					if frag != "" && !docs[target].ids[frag] {
						t.Errorf("%s: link %q: no id %q in %s", name, l, frag, target)
					}
				}
			}
			// An SVG shown with <img> can't load its own images: SVG flows go inline.
			for name, body := range files {
				if strings.HasSuffix(name, ".xhtml") && regexp.MustCompile(`<img[^>]+src="flow\d+\.svg"`).Match(body) {
					t.Errorf("%s shows an SVG flow with <img>", name)
				}
			}
			// The package lists every spine document, and the cover.
			opf := string(files["OEBPS/content.opf"])
			if !strings.Contains(opf, `properties="cover-image"`) {
				t.Error("no cover-image in the package")
			}
			for name, d := range docs {
				if strings.HasPrefix(path.Base(name), "part") {
					if !strings.Contains(opf, `href="`+path.Base(name)+`"`) {
						t.Errorf("%s isn't in the manifest", name)
					}
					text.WriteString(d.text.String())
				}
			}
			for _, want := range []string{"naïve café", "“quoted”", "This is the link target.", "Filler paragraph 199", "Section 2.2"} {
				if !strings.Contains(text.String(), want) {
					t.Errorf("text lacks %q", want)
				}
			}
			// The chapter-one link lands just before the paragraph in chapter three.
			found := false
			for name, body := range files {
				m := reTestLink.FindSubmatch(body)
				if m == nil {
					continue
				}
				found = true
				file, frag, _ := strings.Cut(string(m[1]), "#")
				target := files[path.Join(path.Dir(name), file)]
				at := bytes.Index(target, []byte(`id="`+frag+`"`))
				if at < 0 || !bytes.Contains(target[at:min(at+300, len(target))], []byte("This is the link target")) {
					t.Errorf("link %q doesn't land on the target paragraph", m[1])
				}
			}
			if !found {
				t.Error("no link to the target")
			}
			nav := string(files["OEBPS/nav.xhtml"])
			order := []string{"Chapter One", "Chapter Two", "Section 2.1", "Section 2.2", "Chapter Three"}
			last := -1
			for _, label := range order {
				i := strings.Index(nav, ">"+label+"<")
				if i < last {
					t.Errorf("nav: %q missing or out of order", label)
				}
				last = i
			}
			if fx.kf8 && !strings.Contains(nav, "Chapter Two</a><ol>") {
				t.Errorf("nav doesn't nest the sections under Chapter Two:\n%s", nav)
			}
			imgs := 0
			for name := range files {
				if strings.HasPrefix(path.Base(name), "image") {
					imgs++
				}
			}
			if imgs < 2 {
				t.Errorf("%d images, want the dot and the cover", imgs)
			}
		})
	}
}

func TestConvertDeterministic(t *testing.T) {
	data := readFixture(t, "testdata/tiny.azw3")
	if !bytes.Equal(convert(t, data), convert(t, data)) {
		t.Error("two conversions differ")
	}
}

func TestConvertFile(t *testing.T) {
	dst := t.TempDir() + "/out.epub"
	if err := ConvertFile("testdata/tiny.azw3", dst, Limits{}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dst); err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o644 {
		t.Fatalf("stat %v %v", fi, err)
	}
	if _, err := OpenFile("testdata/tiny.azw3", Limits{MaxFile: 100}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("MaxFile: err = %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open([]byte("not a book")); !errors.Is(err, ErrNotMobi) {
		t.Errorf("garbage: %v", err)
	}
	if _, err := Open(readFixture(t, "testdata/tiny_source.epub")); !errors.Is(err, ErrNotMobi) {
		t.Errorf("epub: %v", err)
	}
	for _, fx := range fixtures {
		data := bytes.Clone(readFixture(t, fx.file))
		// Mark every MOBI header encrypted (a joint file has two).
		for i := range int(binary.BigEndian.Uint16(data[76:])) {
			off := int(binary.BigEndian.Uint32(data[78+8*i:]))
			if off+20 <= len(data) && string(data[off+16:off+20]) == "MOBI" {
				binary.BigEndian.PutUint16(data[off+12:], 2)
			}
		}
		if _, err := Open(data); !errors.Is(err, ErrDRM) {
			t.Errorf("%s encrypted: %v", fx.file, err)
		}
	}
}

func TestPalmDOC(t *testing.T) {
	src := []byte{'A', 'B', 3, 'x', 'y', 'z', 0xC1, 0x80, 0x18}
	got, err := palmdoc(nil, src, 100)
	if err != nil || string(got) != "ABxyz Az A" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := palmdoc(nil, []byte{'A', 0x80, 0x50}, 100); err == nil {
		t.Error("a back-reference before the start was accepted")
	}
	if _, err := palmdoc(nil, bytes.Repeat([]byte{0xC1}, 100), 50); !errors.Is(err, ErrTooLarge) {
		t.Errorf("limit: %v", err)
	}
}

func TestTrailingSize(t *testing.T) {
	rec := []byte("hello" + "M\x01" + "TT\x83")
	if n := trailingSize(rec, 3); string(rec[:len(rec)-n]) != "hello" {
		t.Errorf("trailingSize = %d", n)
	}
	if n := trailingSize([]byte{0x83}, 0xFFFF); n > 1 {
		t.Errorf("trailingSize past the record: %d", n)
	}
}

// testHuff builds HUFF/CDIC records in which every byte is an 8-bit code
// for the phrase "that byte", except the bytes in special, whose phrases
// are compressed text (or cycle).
func testHuff(special map[byte]string) (huff []byte, cdic []byte) {
	huff = make([]byte, 24+1024+256)
	copy(huff, "HUFF\x00\x00\x00\x18")
	binary.BigEndian.PutUint32(huff[8:], 24)
	binary.BigEndian.PutUint32(huff[12:], 24+1024)
	for i := range 256 {
		binary.BigEndian.PutUint32(huff[24+4*i:], 255<<8|0x80|8)
	}
	var entries bytes.Buffer
	offsets := make([]byte, 512)
	for r := range 256 {
		b := byte(255 - r)
		binary.BigEndian.PutUint16(offsets[2*r:], uint16(512+entries.Len()))
		if s, ok := special[b]; ok {
			binary.Write(&entries, binary.BigEndian, uint16(len(s)))
			entries.WriteString(s)
		} else {
			binary.Write(&entries, binary.BigEndian, uint16(0x8001))
			entries.WriteByte(b)
		}
	}
	cdic = make([]byte, 16)
	copy(cdic, "CDIC\x00\x00\x00\x10")
	binary.BigEndian.PutUint32(cdic[8:], 256)
	binary.BigEndian.PutUint32(cdic[12:], 8)
	cdic = append(append(cdic, offsets...), entries.Bytes()...)
	return huff, cdic
}

func TestHuffCDIC(t *testing.T) {
	huff, cdic := testHuff(map[byte]string{'X': "ab", 'Z': "XX", 'Y': "Y"})
	d, err := newHuffcdic(huff, [][]byte{cdic}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.unpack(nil, []byte("aXcZ"), 0)
	if err != nil || string(got) != "aabcabab" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := d.unpack(nil, []byte("Y"), 0); err == nil {
		t.Error("a phrase that expands to itself was accepted")
	}
	small, _ := newHuffcdic(huff, [][]byte{cdic}, 4)
	if _, err := small.unpack(nil, []byte("ZZZ"), 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("limit: %v", err)
	}
	if _, err := newHuffcdic(huff[:100], [][]byte{cdic}, 1<<10); err == nil {
		t.Error("a short HUFF record was accepted")
	}
	if _, err := newHuffcdic(huff, [][]byte{cdic[:600]}, 1<<10); err == nil {
		t.Error("a short CDIC record was accepted")
	}
}

func TestDecodeFont(t *testing.T) {
	font := append([]byte("OTTO"), bytes.Repeat([]byte("glyphs"), 400)...)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(font)
	zw.Close()
	key := []byte("0123456789abcdef")
	enc := z.Bytes()
	for i := range min(len(enc), 1040) {
		enc[i] ^= key[i%len(key)]
	}
	rec := make([]byte, 24)
	copy(rec, "FONT")
	binary.BigEndian.PutUint32(rec[4:], uint32(len(font)))
	binary.BigEndian.PutUint32(rec[8:], 3)
	binary.BigEndian.PutUint32(rec[12:], uint32(24+len(key)))
	binary.BigEndian.PutUint32(rec[16:], uint32(len(key)))
	binary.BigEndian.PutUint32(rec[20:], 24)
	rec = append(append(rec, key...), enc...)
	got, ext, mt := decodeFont(rec, 1<<20)
	if !bytes.Equal(got, font) || ext != "otf" || mt != "font/otf" {
		t.Errorf("got %d bytes, %q %q", len(got), ext, mt)
	}
	if got, _, _ := decodeFont(rec, 100); got != nil {
		t.Error("a font over the limit was decoded")
	}
}

func TestToXHTMLSanitizes(t *testing.T) {
	in := `<html><head><script>alert(1)</script><meta http-equiv="refresh" content="0;url=javascript:x"><base href="https://evil/"></head>
<body onload="x()"><p onclick="x()" style="color:red">Hi <a href="javascript:alert(1)">js</a> <a href="JaVaScRiPt:x">js2</a>
<a href="data:text/html,x">data</a> <a href="https://example.com/">web</a> <a href="#n">frag</a>
<iframe src="https://evil/"></iframe><object data="x"></object><form action="/x"><input name="a"></form>
<img src="ok.png" onerror="x()"><mbp:pagebreak/><p>after break</p>
<svg><script>alert(2)</script><a xlink:href="javascript:alert(3)"><text>t</text></a><set attributeName="href" to="javascript:x"/></svg>
<p bad"attr="1" x:y="2">odd</p>&#1;</body></html>`
	rw := rewriter{url: func(v string) string {
		if !safeURL(v) {
			return ""
		}
		return v
	}, css: func(v string) string { return v }, title: "T"}
	out := string(toXHTML([]byte(in), rw))
	parseXML(t, "out", []byte(out))
	for _, bad := range []string{"script", "alert", "onload", "onclick", "onerror", "javascript", "JaVaScRiPt",
		"data:", "iframe", "object", "<form", "<input", "<base", "http-equiv", "mbp:", "<set", "x:y", "\x01"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{`href="https://example.com/"`, `href="#n"`, `src="ok.png"`, "after break", `style="color:red"`, "<title>T</title>"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBase32(t *testing.T) {
	for s, want := range map[string]int{"0": 0, "V": 31, "10": 32, "000R": 27, "vv": 1023} {
		if n, ok := base32(s); !ok || n != want {
			t.Errorf("base32(%q) = %d, %v", s, n, ok)
		}
	}
	for _, s := range []string{"", "W", "-1", "VVVVVVVVVVV"} {
		if _, ok := base32(s); ok {
			t.Errorf("base32(%q) accepted", s)
		}
	}
	if toBase32(1023) != "VV" {
		t.Error(toBase32(1023))
	}
}

func FuzzConvert(f *testing.F) {
	for _, fx := range fixtures {
		f.Add(readFixture(f, fx.file))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := OpenLimits(data, Limits{MaxText: 1 << 20, MaxResource: 1 << 20})
		if err != nil {
			return
		}
		_ = b.WriteEPUB(io.Discard)
	})
}
