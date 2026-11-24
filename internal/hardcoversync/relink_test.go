package hardcoversync

import (
	"strings"
	"testing"

	"github.com/iotku/CalibreFast/pkg/hardcover"
)

func TestJoinAnd(t *testing.T) {
	for want, in := range map[string][]string{"": nil, "a": {"a"}, "a and b": {"a", "b"}, "a, b and c": {"a", "b", "c"}} {
		if got := joinAnd(in); got != want {
			t.Errorf("joinAnd(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRelinkFeatures(t *testing.T) {
	if got := relinkFeatures(hardcover.SyncScopes); len(got) != 0 {
		t.Errorf("all scopes: %q", got)
	}
	got := relinkFeatures("")
	for _, f := range scopeFeatures {
		if strings.Contains(" "+hardcover.SyncScopes+" ", " "+f.scope+" ") && !strings.Contains(strings.Join(got, "|"), f.feature) {
			t.Errorf("no scopes: %q lacks %q", got, f.feature)
		}
	}
}
