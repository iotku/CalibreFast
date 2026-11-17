package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// coverHue must match coverHue in web/utils.ts (values from running it in a
// browser), so the book page's placeholder matches the library card.
func TestCoverHueMatchesJS(t *testing.T) {
	for uuid, want := range map[string]int{
		"1fb32edc-688c-461b-94fb-6fd25800189a": 143,
		"cb771cce-d1f7-4953-b432-3c1baef06ed9": 286,
		"afb0981a-7ab9-4475-aad1-3327c80d5462": 93,
		"f28b0d18-24c5-4921-84eb-ec719f344b17": 278,
		"a":                                    97,
		"":                                     0,
	} {
		if got := coverHue(uuid); got != want {
			t.Errorf("coverHue(%q) = %d, want %d", uuid, got, want)
		}
	}
}

// The book page shows the first 10 tags and 5 authors; the rest are marked
// for "+N more" (user.ts and style.css hide them until it's clicked).
func TestBookPageLimitsTagsAndAuthors(t *testing.T) {
	render := func(tags, authors int) string {
		t.Helper()
		data := BookPageData{UUID: "u", Title: "T"}
		for i := range tags {
			data.Tags = append(data.Tags, fmt.Sprintf("tag%d", i))
		}
		for i := range authors {
			data.Authors = append(data.Authors, fmt.Sprintf("author%d", i))
		}
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, "book.html", data); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	html := render(13, 7)
	for _, want := range []string{`+3 more</button>`, `and 2 more</button>`, `tag meta-link more-extra">tag10<`, `tag meta-link">tag9<`} {
		if !strings.Contains(html, want) {
			t.Errorf("13 tags, 7 authors: missing %s", want)
		}
	}
	if got := strings.Count(html, "more-extra"); got != 3+2 {
		t.Errorf("13 tags, 7 authors: %d hidden, want 5", got)
	}

	html = render(10, 5)
	if strings.Contains(html, "more-extra") || strings.Contains(html, "more-toggle") {
		t.Error("10 tags, 5 authors: nothing should be hidden")
	}
}
