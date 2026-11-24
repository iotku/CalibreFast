package hardcoverseries

import "testing"

func TestTitleKeys(t *testing.T) {
	// A short title shared by several books doesn't name any of them.
	common := map[string]bool{"doctor who": true}
	if k := titleKeys("Doctor Who: The Myth Makers", common); len(k) != 1 || k[0] != "doctor who the myth makers" {
		t.Errorf("keys %q", k)
	}
	if k := titleKeys("Caliban's War: The Expanse Book Two", nil); len(k) != 2 || k[1] != "calibans war" {
		t.Errorf("keys %q", k)
	}
}
