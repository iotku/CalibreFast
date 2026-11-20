package hardcover

import "testing"

func TestCleanBio(t *testing.T) {
	for in, want := range map[string]string{
		"A writer.<sup>[1][2]</sup>\r\n\r\n\r\nB &amp; C.[3]": "A writer.\n\nB & C.",
		"One<br>two <b>bold</b>  <script>x</script>":          "One\ntwo bold x",
		"  \r\n ": "",
	} {
		if got := cleanBio(in); got != want {
			t.Errorf("cleanBio(%q) = %q, want %q", in, got, want)
		}
	}
}
