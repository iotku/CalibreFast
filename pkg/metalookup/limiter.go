package metalookup

import (
	"fmt"
	"sync"
	"time"
)

// Limiter keeps a client well inside a service's limits: a token
// bucket (burst, then perSecond), a daily cap, and a pause after a 429.
// Over the limit a request is refused, not queued.
type Limiter struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	daily     int
	tokens    float64
	last      time.Time
	day       string // the date today counts
	today     int
	pausedTil time.Time
}

// NewLimiter allows burst requests at once, then perSecond, and at most
// daily a day (by the local date).
func NewLimiter(perSecond, burst float64, daily int) *Limiter {
	return &Limiter{perSecond: perSecond, burst: burst, daily: daily, tokens: burst}
}

// Take uses up one request at now, or says why not (and when to try again).
func (s *Limiter) Take(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Before(s.pausedTil) {
		return fmt.Errorf("asked to slow down; try again in %s", s.pausedTil.Sub(now).Round(time.Second))
	}
	if d := now.Format(time.DateOnly); d != s.day {
		s.day, s.today = d, 0
	}
	if s.today >= s.daily {
		return fmt.Errorf("the limit of %d requests a day is used up; try again tomorrow", s.daily)
	}
	if !s.last.IsZero() {
		s.tokens = min(s.burst, s.tokens+now.Sub(s.last).Seconds()*s.perSecond)
	}
	s.last = now
	if s.tokens < 1 {
		wait := time.Duration((1 - s.tokens) / s.perSecond * float64(time.Second))
		return fmt.Errorf("too many requests just now; try again in %s", max(wait, time.Second).Round(time.Second))
	}
	s.tokens--
	s.today++
	return nil
}

// Pause refuses every request until until, as after an HTTP 429.
func (s *Limiter) Pause(until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.After(s.pausedTil) {
		s.pausedTil = until
	}
}

// Usage is a Limiter's state, for a status page.
type Usage struct {
	Used      int     // requests taken today
	Daily     int     // the day's cap
	Tokens    float64 // what the bucket holds now
	Burst     float64
	PerSecond float64
	// ResetsAt is the next local midnight, when the day's count starts over.
	ResetsAt time.Time
	// PausedUntil is when a 429's pause ends; zero when not paused.
	PausedUntil time.Time
}

// Usage reports the state at now without taking anything or changing it.
func (s *Limiter) Usage(now time.Time) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := Usage{Daily: s.daily, Burst: s.burst, PerSecond: s.perSecond, Tokens: s.tokens}
	if s.day == now.Format(time.DateOnly) {
		u.Used = s.today
	}
	if !s.last.IsZero() && now.After(s.last) {
		u.Tokens = min(s.burst, s.tokens+now.Sub(s.last).Seconds()*s.perSecond)
	}
	y, m, d := now.Date()
	u.ResetsAt = time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
	if now.Before(s.pausedTil) {
		u.PausedUntil = s.pausedTil
	}
	return u
}
