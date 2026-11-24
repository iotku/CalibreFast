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
