package koreader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iotku/CalibreFast/pkg/koreader/koreadertest"
)

func TestLuaTable(t *testing.T) {
	src := `-- /mnt/onboard/Book.epub.annotations.lua
return {
    ["annotations"] = {
        [1] = {
            ["note"] = "two\
lines \"quoted\" \\ \65\066 caf\195\169",
            ["pos0"] = { x = 12.5, ["y"] = -3, page = 0x10 },
            ["flag"] = false,
        },
        [2] = { "a", 'b', nil, true },
    },
    ["paging"] = true, -- trailing comment
    --[[ a block
         comment ]]
    [7] = "sparse",
}
`
	tab, err := ParseLuaTable(src)
	if err != nil {
		t.Fatal(err)
	}
	list := tab.Table("annotations")
	if list == nil || len(list.Arr) != 2 || tab.Hash["paging"] != true || tab.Hash["7"] != "sparse" {
		t.Fatalf("parsed: %+v", tab)
	}
	first := list.Arr[0].(*LuaTable)
	if got := first.Str("note"); got != "two\nlines \"quoted\" \\ AB café" {
		t.Errorf("note: %q", got)
	}
	pos := first.Table("pos0")
	if x, _ := pos.Num("x"); x != 12.5 {
		t.Errorf("x: %v", x)
	}
	if p, _ := pos.Num("page"); p != 16 {
		t.Errorf("hex page: %v", p)
	}
	if arr := list.Arr[1].(*LuaTable).Arr; len(arr) != 4 || arr[0] != "a" || arr[2] != nil || arr[3] != true {
		t.Errorf("array part: %v", arr)
	}

	// What we write reads back the same.
	var b strings.Builder
	b.WriteString("return ")
	WriteLua(&b, tab, 0)
	again, err := ParseLuaTable(b.String())
	if err != nil {
		t.Fatalf("our own output: %v\n%s", err, b.String())
	}
	if got := again.Table("annotations").Arr[0].(*LuaTable).Str("note"); got != first.Str("note") {
		t.Errorf("note after a round trip: %q", got)
	}

	// Only data is read: code is a syntax error, never run.
	for _, bad := range []string{
		``, `{}`, `return 5`, `return { a = os.execute("x") }`, `return { a = b }`, `return { f() }`,
		`return { "unfinished }`, `return { a = 1 } print(1)`, `return { a = 1 + 2 }`, `return {` + strings.Repeat("{", 100),
	} {
		if _, err := ParseLuaTable(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Places in koreadertest.KOChapter, as each reader writes them. The XPointers follow
// crengine's rules for text (see text.go), read from its source;
// they have not been checked on a device.
var koPlaces = []struct{ word, cfi, xpointer string }{
	{"Alpha", "/4/2/2/1:0", "/body/DocFragment[2]/body/div/p[1]/text().0"},
	{"gamma", "/4/2/2/1:14", "/body/DocFragment[2]/body/div/p[1]/text().11"}, // three spaces and a line break became two spaces
	{"One", "/4/2/4/1:0", "/body/DocFragment[2]/body/div/p[2]/text()[1].0"},
	{"two", "/4/2/4/2/1:0", "/body/DocFragment[2]/body/div/p[2]/b/text().0"},
	{"four", "/4/2/4/3:9", "/body/DocFragment[2]/body/div/p[2]/text()[2].9"},
	{"five", "/4/2/4/3:17", "/body/DocFragment[2]/body/div/p[2]/text()[2].16"}, // the emoji: two UTF-16 units, one character
	{"Lead", "/4/4/1:1", "/body/DocFragment[2]/body/p/text()[1].1"},
	{"tail", "/4/4/5:1", "/body/DocFragment[2]/body/p/text()[3].1"}, // the space between the two <i> is text()[2]
}

func TestKOReaderTextPositions(t *testing.T) {
	c, err := parseChapter([]byte(koreadertest.KOChapter))
	if err != nil {
		t.Fatal(err)
	}
	for _, place := range koPlaces {
		p, ok := c.fromCFIPath(place.cfi)
		if !ok || !strings.HasPrefix(string(p.node.text[p.off:]), place.word) {
			t.Errorf("%s: CFI %s points at %q", place.word, place.cfi, string(p.node.text))
			continue
		}
		if xp, ok := c.xpointer(2, p); !ok || xp != place.xpointer {
			t.Errorf("%s: XPointer %q, want %q", place.word, xp, place.xpointer)
		}
		back, exact, ok := c.fromXPointer(place.xpointer)
		if !ok || !exact || back != p {
			t.Errorf("%s: XPointer %s points at %q+%d", place.word, place.xpointer, string(back.node.text), back.off)
		}
		elements, text := cfiSteps(p)
		if got := strings.Join(elements, "") + text; got != place.cfi {
			t.Errorf("%s: CFI %q, want %q", place.word, got, place.cfi)
		}
	}
	// The index is optional where there is one of a kind, as newer KOReader writes it.
	if p, exact, ok := c.fromXPointer("/body[1]/DocFragment[2]/body[1]/div[1]/p[1]/text()[1].11"); !ok || !exact || !strings.HasPrefix(string(p.node.text[p.off:]), "gamma") {
		t.Errorf("explicit [1]: %v %v", exact, ok)
	}
	// A path this file doesn't have resolves as far as it goes.
	if _, exact, ok := c.fromXPointer("/body/DocFragment[2]/body/div/p[2]/span[4]/text().3"); !ok || exact {
		t.Errorf("unknown element: exact=%v ok=%v", exact, ok)
	}
	// White space that crengine drops has no XPointer of its own: the next text does.
	blank := c.root.children[1].children[0] // the line break after <body>
	if xp, ok := c.xpointer(2, textPoint{blank, 0}); !ok || xp != koPlaces[0].xpointer {
		t.Errorf("dropped white space: %q", xp)
	}

	start, _ := c.fromCFIPath("/4/2/4/2/1:0")
	end, _ := c.fromCFIPath("/4/2/4/3:13")
	if got := cfiRange(6, 1, start, end); got != "epubcfi(/6/4!/4/2/4,/2/1:0,/3:13)" {
		t.Errorf("range: %s", got)
	}
	if item, a, b, ok := splitCFI("epubcfi(/6/4[ch,ap]!/4/2[x^]y]/4,/2/1:0,/3:13)"); !ok || item != 1 || a != "/4/2/4/2/1:0" || b != "/4/2/4/3:13" {
		t.Errorf("splitCFI: %d %q %q %v", item, a, b, ok)
	}
}

// KOReader's own sample, when a checkout of koreader/test-data is at hand.
func TestKOReaderAnnotationsFixture(t *testing.T) {
	dir := os.Getenv("KOREADER_ANNOTATIONS")
	if dir == "" {
		t.Skip("set KOREADER_ANNOTATIONS to a folder with book.epub and book.epub.annotations.lua exported by KOReader")
	}
	src, err := os.ReadFile(filepath.Join(dir, "book.epub.annotations.lua"))
	if err != nil {
		t.Fatal(err)
	}
	tab, err := ParseLuaTable(string(src))
	if err != nil {
		t.Fatal(err)
	}
	recs, res, err := Annotations(filepath.Join(dir, "book.epub"), "epub", tab, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d highlights placed, %d not, %d bookmarks", len(recs), res.Skipped, res.Bookmarks)
	// Where KOReader saved a CFI (newer versions), ours from its XPointers must agree.
	book, err := openKOBook(filepath.Join(dir, "book.epub"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for _, v := range tab.Table("annotations").Arr {
		item := v.(*LuaTable)
		want := item.Str("epubcfi")
		if want == "" || item.Str("drawer") == "" {
			continue
		}
		delete(item.Hash, "epubcfi")
		got, _ := koreaderEPUBAnchor(book, item)
		_, a, b, _ := splitCFI(want)
		_, c, d, _ := splitCFI(got)
		if a != c || b != d {
			t.Errorf("%q: ours %s, KOReader's %s", item.Str("text"), got, want)
		}
	}
}

func TestFileAnnotationsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(path, koreadertest.KOEPUB(t), 0o600); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2025, 3, 1, 10, 0, 0, 0, time.Local).UnixMilli()
	in := []Highlight{
		{Anchor: "epubcfi(/6/4!/4/2/4,/2/1:0,/3:13)", Text: "two three & four", Note: "n", Color: "green", Page: 3, CreatedAt: created, UpdatedAt: created},
		{Anchor: "epubcfi(/6/40!/4/2,/1:0,/1:3)", Text: "lost", CreatedAt: created, UpdatedAt: created},
	}
	opts := Options{DeviceID: "test-device", Source: "Test", Colors: []string{"yellow", "green"}, MaxTextLen: 8}
	lua, skipped := File(path, "epub", in, opts)
	if skipped != 1 || !strings.HasPrefix(lua, "-- book.epub: highlights from Test, for KOReader to import\nreturn {") {
		t.Fatalf("skipped %d:\n%s", skipped, lua)
	}
	tab, err := ParseLuaTable(lua)
	if err != nil {
		t.Fatal(err)
	}
	if tab.Str("device_id") != "test-device" || tab.Str("datetime") != Oldest {
		t.Errorf("header: %v", tab.Hash)
	}
	out, res, err := Annotations(path, "epub", tab, opts)
	if err != nil || len(out) != 1 || res.Skipped != 0 {
		t.Fatalf("%+v %+v %v", out, res, err)
	}
	h := out[0]
	if h.Anchor != in[0].Anchor || h.Text != "two thre" || h.Color != "green" || h.Page != 3 ||
		h.CreatedAt != created || !strings.HasPrefix(h.ID, IDPrefix) {
		t.Errorf("read back %+v", h)
	}
	// The same file again gives the same id.
	again, _, _ := Annotations(path, "epub", tab, opts)
	if again[0].ID != h.ID {
		t.Error("ids differ between imports")
	}
	// A PDF's file isn't an EPUB's.
	if _, _, err := Annotations(path, "pdf", tab, opts); err == nil {
		t.Error("an EPUB's file read for a PDF")
	}
}
