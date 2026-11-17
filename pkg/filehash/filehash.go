package filehash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

// DefaultMax is how many hashes the default cache keeps.
const DefaultMax = 4096

// Cache remembers hashes by path, size and modification time. When it is
// full it is emptied. It is safe for concurrent use.
type Cache struct {
	mu  sync.Mutex
	max int
	m   map[string]string
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
	key := fmt.Sprintf("%s\x00%d\x00%d", p, info.Size(), info.ModTime().UnixNano())
	c.mu.Lock()
	h, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return h, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	h = hex.EncodeToString(sum.Sum(nil))
	c.mu.Lock()
	if c.m == nil || len(c.m) >= c.max {
		c.m = map[string]string{}
	}
	c.m[key] = h
	c.mu.Unlock()
	return h, nil
}
