package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestLanguageDisplayName(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"eng", "English", true},
		{"en", "English", true},
		{"EN", "English", true},
		{"fre", "French", true}, // 639-2/B
		{"fra", "French", true}, // 639-2/T
		{"ger", "German", true},
		{"iw", "Hebrew", true}, // deprecated 639-1
		{"en-US", "English (US)", true},
		{"en_gb", "English (GB)", true},
		{"pt-br", "Portuguese (BR)", true},
		{"zh-hant", "Chinese (Hant)", true},
		{"zh-Hant-TW", "Chinese (Hant-TW)", true},
		{"und", "Undetermined", true},
		{"xyz", "xyz", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := languageDisplayName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("languageDisplayName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLanguageTableHasNoConflictingCodes(t *testing.T) {
	seen := map[string]string{}
	for _, l := range languageTable {
		for _, c := range l.codes {
			if prev, dup := seen[c]; dup {
				t.Errorf("code %q maps to both %q and %q", c, prev, l.name)
			}
			seen[c] = l.name
		}
	}
	if len(languageTable) < 100 {
		t.Errorf("expected at least 100 languages, have %d", len(languageTable))
	}
}

func TestLanguageCodesHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	languageCodesHandler(rec, httptest.NewRequest("GET", "/api/languages/codes", nil))
	var got map[string]languageCode
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for code, want := range map[string]languageCode{
		"en":  {"English", "eng"},
		"eng": {"English", "eng"},
		"fre": {"French", "fra"},
		"fil": {"Filipino", "fil"},
	} {
		if got[code] != want {
			t.Errorf("%s = %+v; want %+v", code, got[code], want)
		}
	}
}
