package textkey

import "testing"

func TestFold(t *testing.T) {
	for in, want := range map[string]string{
		"Caliban’s War":        "calibans war",
		"  Ender's   Game! ":   "enders game",
		"Pride & Prejudice":    "pride and prejudice",
		"Les Misérables":       "les miserables",
		"S.A. Corey—Leviathan": "sa corey leviathan",
	} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestName(t *testing.T) {
	want := Name("James S. A. Corey")
	for _, n := range []string{"James S.A. Corey", "james s. a. corey", "Corey, James S. A."} {
		if Name(n) != want {
			t.Errorf("Name(%q) = %q, want %q", n, Name(n), want)
		}
	}
}

func TestLooseName(t *testing.T) {
	for in, want := range map[string]string{
		"J. R. R. Tolkien": "tolkien j",
		"Tolkien, John":    "tolkien j",
		"Homer":            "",
	} {
		if got := LooseName(in); got != want {
			t.Errorf("LooseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitle(t *testing.T) {
	for in, want := range map[string][2]string{
		"The Expanse: Leviathan Wakes (The Expanse #1)": {"expanse", "expanse leviathan wakes"},
		"A Game of Thrones [Book 1]":                    {"game of thrones", "game of thrones"},
		"Doctor Who: Shada":                             {"doctor who", "doctor who shada"},
	} {
		if got := [2]string{Title(in), FullTitle(in)}; got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if Series("The Expanse") != "expanse" || Series("Doctor Who: Target Books") == Series("Doctor Who: BBC Radio") {
		t.Error("Series")
	}
	if !SharesName([]string{"Corey, James S. A."}, []string{"X", "James S.A. Corey"}) || SharesName([]string{""}, []string{""}) {
		t.Error("SharesName")
	}
}
