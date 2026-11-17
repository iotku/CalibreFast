package langcodes

import "testing"

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
		got, ok := DisplayName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("DisplayName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLanguageTableHasNoConflictingCodes(t *testing.T) {
	seen := map[string]string{}
	for _, l := range Table {
		for _, c := range l.Codes {
			if prev, dup := seen[c]; dup {
				t.Errorf("code %q maps to both %q and %q", c, prev, l.Name)
			}
			seen[c] = l.Name
		}
	}
	if len(Table) < 100 {
		t.Errorf("expected at least 100 languages, have %d", len(Table))
	}
}
