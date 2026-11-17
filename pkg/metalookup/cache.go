package metalookup

import (
	"sync"
	"time"
)

// Cache keeps answers in memory for a while, so looking the same book up
// again doesn't ask again. It is safe for concurrent use.
type Cache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]cacheEntry
}

type cacheEntry struct {
	res Result
	at  time.Time
}

// NewCache makes a cache that keeps answers for ttl, and at most max of
// them.
func NewCache(ttl time.Duration, max int) *Cache {
	return &Cache{ttl: ttl, max: max, entries: map[string]cacheEntry{}}
}

func (c *Cache) get(key string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > c.ttl {
		return Result{}, false
	}
	return e.res, true
}

func (c *Cache) put(key string, res Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if time.Since(e.at) > c.ttl {
				delete(c.entries, k)
			}
		}
		for k := range c.entries { // still full: drop any one
			if len(c.entries) < c.max {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{res: res, at: time.Now()}
}
