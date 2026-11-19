package filehash

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

// DefaultMax is how many hashes the default cache keeps: one entry is about
// a hundred bytes, so a library's worth of files fits in a few megabytes.
const DefaultMax = 50000

// Cache remembers hashes by path, along with the size and modification
// time they were taken at. When it is full the least recently used one is
// dropped. It is safe for concurrent use.
type Cache struct {
	mu  sync.Mutex
	max int
	m   map[string]*list.Element // path -> its entry in lru
	lru list.List                // *entry, most recently used first
}

type entry struct {
	path    string
	size    int64
	modTime int64
	hash    string
}

// NewCache makes a cache that keeps at most max hashes.
func NewCache(max int) *Cache {
	return &Cache{max: max}
}

var defaultCache = NewCache(DefaultMax)

// SHA256 is the hex SHA-256 of the file at p, whose os.Stat is info, from
// the default cache.
func SHA256(p string, info os.FileInfo) (string, error) {
	return defaultCache.SHA256(p, info)
}

// SHA256 is the hex SHA-256 of the file at p, whose os.Stat is info. It is
// read only when the cache has no hash for p at that size and modification
// time.
func (c *Cache) SHA256(p string, info os.FileInfo) (string, error) {
	size, mod := info.Size(), info.ModTime().UnixNano()
	c.mu.Lock()
	if e, ok := c.m[p]; ok {
		if en := e.Value.(*entry); en.size == size && en.modTime == mod {
			c.lru.MoveToFront(e)
			c.mu.Unlock()
			return en.hash, nil
		}
	}
	c.mu.Unlock()

	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	h := hex.EncodeToString(sum.Sum(nil))

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*list.Element{}
	}
	if e, ok := c.m[p]; ok { // stale, or hashed meanwhile by another call
		*e.Value.(*entry) = entry{p, size, mod, h}
		c.lru.MoveToFront(e)
		return h, nil
	}
	c.m[p] = c.lru.PushFront(&entry{p, size, mod, h})
	for c.max > 0 && c.lru.Len() > c.max {
		old := c.lru.Back()
		c.lru.Remove(old)
		delete(c.m, old.Value.(*entry).path)
	}
	return h, nil
}
