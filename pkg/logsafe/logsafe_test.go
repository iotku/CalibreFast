package logsafe

import (
	"errors"
	"testing"
)

func TestText(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
	}{
		{"plain title", "plain title"},
		{"x\n2026/01/01 admin signed in", `x\n2026/01/01 admin signed in`},
		{"a\r\nb", `a\r\nb`},
		{errors.New("open book\n.epub: no such file"), `open book\n.epub: no such file`},
		{nil, "<nil>"},
		{42, "42"},
	} {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
