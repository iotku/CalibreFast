package hardcover

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// Budget keeps a client inside Hardcover's plan limits: a token bucket
// (Burst at once, refilling at PerMinute) and a daily count that resets at
// midnight UTC, as Hardcover's does. Unlike metalookup.Limiter it never
// refuses: Reserve says when to try again, and the caller holds the work
// until then. Hardcover's own RateLimit headers lower what it thinks is
// left (the same token may be used by other tools), and a 429 pauses it.
//
// The free plan is 60 a minute, burst 10, 5,000 a day; every top-level
// field of a query counts as one request.
type Budget struct {
	mu        sync.Mutex
	perMinute float64
	burst     float64
	daily     int

	tokens float64
	last   time.Time
	day    string // the UTC date used counts
	used   int
	// remote: what Hardcover last said was left today (-1: not known).
	remote    int
	pausedTil time.Time
}

// NewBudget allows burst requests at once, then perMinute, and at most
// daily a (UTC) day.
func NewBudget(perMinute, burst float64, daily int) *Budget {
	return &Budget{perMinute: perMinute, burst: burst, daily: daily, tokens: burst, remote: -1}
}

// FreePlan is the budget for Hardcover's free plan, with headroom under its
// 5,000 a day for the owner's other tools.
func FreePlan() *Budget { return NewBudget(60, 10, 4500) }

// nextReset is midnight UTC after now.
func nextReset(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// rollLocked starts a new day's count when the UTC date has changed, and
// refills the bucket for the time since last.
func (b *Budget) rollLocked(now time.Time) {
	if d := now.UTC().Format(time.DateOnly); d != b.day {
		b.day, b.used, b.remote = d, 0, -1
	}
	if !b.last.IsZero() && now.After(b.last) {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Minutes()*b.perMinute)
	}
	if now.After(b.last) {
		b.last = now
	}
}

// Reserve takes n requests at now. It answers the zero time when they
// were taken, else when to ask again (nothing is taken then).
func (b *Budget) Reserve(now time.Time, n int) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(now)
	if now.Before(b.pausedTil) {
		return b.pausedTil
	}
	if b.used+n > b.daily || (b.remote >= 0 && b.remote < n) {
		return nextReset(now)
	}
	if b.tokens < float64(n) {
		need := (float64(n) - b.tokens) / b.perMinute
		return now.Add(max(time.Duration(need*float64(time.Minute)), time.Second))
	}
	b.tokens -= float64(n)
	b.used += n
	if b.remote >= 0 {
		b.remote -= n
	}
	return time.Time{}
}

// Pause holds every request until until (after a 429).
func (b *Budget) Pause(until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if until.After(b.pausedTil) {
		b.pausedTil = until
	}
}

// Observe reads Hardcover's RateLimit header, e.g.
//
//	"Free";r=8;t=0, "daily";r=4231;t=51234
//
// and lowers what is left to what it says: r is requests remaining, t
// seconds until that bucket resets. An entry that's used up pauses the
// budget for its t.
func (b *Budget) Observe(now time.Time, header string) {
	if header == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(now)
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(entry, ";")
		name := strings.Trim(strings.TrimSpace(parts[0]), `"`)
		r, t := -1, -1
		for _, p := range parts[1:] {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if !ok {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				continue
			}
			switch k {
			case "r":
				r = n
			case "t":
				t = n
			}
		}
		if r < 0 || name == "" {
			continue
		}
		if strings.EqualFold(name, "daily") {
			if b.remote < 0 || r < b.remote {
				b.remote = r
			}
			if r == 0 && t > 0 {
				b.pausedTil = later(b.pausedTil, now.Add(time.Duration(t)*time.Second))
			}
			continue
		}
		b.tokens = min(b.tokens, float64(r))
		if r == 0 && t > 0 {
			b.pausedTil = later(b.pausedTil, now.Add(time.Duration(min(t, 3600))*time.Second))
		}
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Usage is the budget as the server keeps it across restarts and shows it.
type Usage struct {
	Day         string    `json:"day"` // UTC date
	Used        int       `json:"used"`
	Limit       int       `json:"limit"`
	Remote      int       `json:"remote"` // Hardcover's own count of what's left; -1 not known
	PausedUntil time.Time `json:"pausedUntil"`
	ResetsAt    time.Time `json:"resetsAt"`
}

// Usage reports the day's use at now.
func (b *Budget) Usage(now time.Time) Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(now)
	u := Usage{Day: b.day, Used: b.used, Limit: b.daily, Remote: b.remote, ResetsAt: nextReset(now)}
	if now.Before(b.pausedTil) {
		u.PausedUntil = b.pausedTil
	}
	return u
}

// Restore takes back a day's use saved before a restart. Another day's is
// ignored, except a pause that hasn't ended.
func (b *Budget) Restore(now time.Time, u Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked(now)
	if u.Day == b.day {
		b.used = max(b.used, u.Used)
		if u.Remote >= 0 && (b.remote < 0 || u.Remote < b.remote) {
			b.remote = u.Remote
		}
	}
	if now.Before(u.PausedUntil) {
		b.pausedTil = later(b.pausedTil, u.PausedUntil)
	}
}
