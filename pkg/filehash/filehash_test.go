package filehash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	c := NewCache(2)
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if h, err := c.SHA256(p, info); err != nil || h != abc {
		t.Fatalf("%s, %v", h, err)
	}
	// Same size and time: from the cache, without reading.
	if err := os.WriteFile(p, []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if h, _ := c.SHA256(p, info); h != abc {
		t.Errorf("not cached: %s", h)
	}
	// A new modification time: read again.
	later := info.ModTime().Add(time.Second)
	if err := os.Chtimes(p, later, later); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(p)
	if h, _ := SHA256(p, info2); h == abc {
		t.Error("changed file not read again")
	}
	if _, err := SHA256(filepath.Join(t.TempDir(), "missing"), info2); err == nil {
		t.Error("missing file hashed")
	}
}

func TestCacheDropsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(2)
	paths := make([]string, 3)
	infos := make([]os.FileInfo, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, string(rune('a'+i)))
		if err := os.WriteFile(paths[i], []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		infos[i], _ = os.Stat(paths[i])
	}
	c.SHA256(paths[0], infos[0])
	c.SHA256(paths[1], infos[1])
	c.SHA256(paths[0], infos[0]) // a is now the most recently used
	c.SHA256(paths[2], infos[2]) // drops b
	if _, ok := c.m[paths[1]]; ok {
		t.Error("b kept")
	}
	if _, ok := c.m[paths[0]]; !ok {
		t.Error("a dropped")
	}
	if c.lru.Len() != 2 || len(c.m) != 2 {
		t.Errorf("%d entries", c.lru.Len())
	}
}
