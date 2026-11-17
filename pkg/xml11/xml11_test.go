package xml11

import "testing"

func TestXML11To10(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"1.0 untouched", "<?xml version=\"1.0\"?><a>&#x1;\u0085</a>", "<?xml version=\"1.0\"?><a>&#x1;\u0085</a>"},
		{"no declaration untouched", "<a>&#x1;</a>", "<a>&#x1;</a>"},
		{"version rewritten", `<?xml version="1.1" encoding="UTF-8"?><a/>`, `<?xml version="1.0" encoding="UTF-8"?><a/>`},
		{"single quotes and BOM", "\xEF\xBB\xBF<?xml version='1.1'?><a/>", "\xEF\xBB\xBF<?xml version='1.0'?><a/>"},
		{"C0 refs dropped", `<?xml version="1.1"?><a b="x&#1;y">p&#x1F;q&#x9;r&#10;s&#X41;&#65;</a>`,
			`<?xml version="1.0"?><a b="xy">pq&#x9;r&#10;s&#X41;&#65;</a>`},
		{"refs in CDATA, comments, PIs kept", `<?xml version="1.1"?><a><![CDATA[&#x1;]]><!-- &#x2; --><?p &#x3;?>&#x4;</a>`,
			`<?xml version="1.0"?><a><![CDATA[&#x1;]]><!-- &#x2; --><?p &#x3;?></a>`},
		{"line ends", "<?xml version=\"1.1\"?><a>1\r\u00852\u00853 4</a>", "<?xml version=\"1.0\"?><a>1\n2\n3\n4</a>"},
		{"line ends left in other encodings", "<?xml version=\"1.1\" encoding=\"ISO-8859-1\"?><a>1\u00852</a>",
			"<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><a>1\u00852</a>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(XML11To10([]byte(tc.in))); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestXMLUnmarshal11(t *testing.T) {
	var v struct {
		T struct {
			Text string `xml:",chardata"`
			A    string `xml:"a,attr"`
		} `xml:"t"`
	}
	doc := "<?xml version=\"1.1\" encoding=\"utf-8\"?>\n<r><t a=\"q&#x1;r\">one&#x7;\u0085two &amp; &#xE9;</t></r>"
	if err := Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	if v.T.Text != "one\ntwo & é" || v.T.A != "qr" {
		t.Errorf("got %q, %q", v.T.Text, v.T.A)
	}
}
