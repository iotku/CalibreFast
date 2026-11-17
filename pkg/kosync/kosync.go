package kosync

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prefix is where CalibreFast serves the API; IsPath also accepts the
// unprefixed paths KOReader uses when the server address is entered
// without it.
const Prefix = "/kosync"

// MaxDeviceNameLen bounds the device and device_id fields of a position.
const MaxDeviceNameLen = 200

// DocumentRe checks a document id. KOReader's are 32 hex digits; it allows a
// little more for other clients, but nothing a GET path couldn't carry back.
var DocumentRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Error codes are those of the reference server; KOReader shows the message
// that comes with them.
const (
	ErrUnknown        = 2000 // a server error
	ErrUnauthorized   = 2001 // wrong credentials (with HTTP 401)
	ErrInvalidRequest = 2003 // a malformed body or fields
	ErrDocument       = 2004 // no or a bad document id
	ErrNoRegistration = 2005 // registration refused (with HTTP 402)
)

// WriteError writes an error as the protocol has it: {"code": …,
// "message": …} with the given HTTP status.
func WriteError(w http.ResponseWriter, status, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message})
}

// Sync keys are typed on e-reader keyboards: 16 lower-case letters (no l or
// o, which look like 1 and 0), about 73 bits.
const (
	keyLen      = 16
	keyAlphabet = "abcdefghijkmnpqrstuvwxyz"
)

// RandomKey makes a new sync key.
func RandomKey() string {
	out := make([]byte, 0, keyLen)
	buf := make([]byte, 32)
	for len(out) < keyLen {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			// Reject the top of the byte range so each letter is equally likely.
			if int(b) < 256/len(keyAlphabet)*len(keyAlphabet) && len(out) < keyLen {
				out = append(out, keyAlphabet[int(b)%len(keyAlphabet)])
			}
		}
	}
	return string(out)
}

// KeyHash is what to store for a key: the SHA-256 (hex) of what KOReader
// sends as x-auth-key, which is the lower-case hex MD5 of the key, so
// KeyHash(MD5Hex(key)). Compare it in constant time.
func KeyHash(authKey string) string {
	sum := sha256.Sum256([]byte(authKey))
	return hex.EncodeToString(sum[:])
}

// MD5Hex is the lower-case hex MD5 of s.
func MD5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// DummyKeyHash is a key hash to compare against when a user has no key, so
// that unknown users take as long to refuse as wrong keys.
var DummyKeyHash = KeyHash(MD5Hex("calibrefast-timing-dummy"))

// GuessLimiter stops a client address guessing keys: it may try a limited
// number of different wrong (username, key) pairs per window. A device that
// keeps sending the same stale key (KOReader pushes every 25 seconds while
// reading) counts once, so it can't lock out other devices behind the same
// address.
type GuessLimiter struct {
	mu     sync.Mutex
	fails  map[string]map[string]time.Time // address → pair → first failure
	max    int
	window time.Duration
	// Now is the clock; tests may replace it.
	Now func() time.Time
}

// NewGuessLimiter allows each address max different wrong pairs within
// window.
func NewGuessLimiter(max int, window time.Duration) *GuessLimiter {
	return &GuessLimiter{fails: map[string]map[string]time.Time{}, max: max, window: window, Now: time.Now}
}

func (g *GuessLimiter) prune(addr string) {
	cut := g.Now().Add(-g.window)
	for k, t := range g.fails[addr] {
		if t.Before(cut) {
			delete(g.fails[addr], k)
		}
	}
	if len(g.fails[addr]) == 0 {
		delete(g.fails, addr)
	}
}

// Blocked reports whether addr has used up its wrong guesses.
func (g *GuessLimiter) Blocked(addr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prune(addr)
	return len(g.fails[addr]) >= g.max
}

// Fail records a wrong guess from addr. pair identifies what was tried (for
// example the user name and a hash of the key), so that repeating the same
// wrong pair counts once.
func (g *GuessLimiter) Fail(addr, pair string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.fails) > 10000 {
		for a := range g.fails {
			g.prune(a)
		}
	}
	if g.fails[addr] == nil {
		g.fails[addr] = map[string]time.Time{}
	}
	if _, seen := g.fails[addr][pair]; !seen {
		g.fails[addr][pair] = g.Now()
	}
}

// JSONFloat reads a position's percentage: it accepts a JSON number or a numeric string (as the reference
// server does).
func JSONFloat(v any) (float64, bool) {
	var s string
	switch t := v.(type) {
	case json.Number:
		s = t.String()
	case string:
		s = strings.TrimSpace(t)
	default:
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// JSONString reads a position's progress: it accepts a string, or a number (stored as written).
func JSONString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// IsPath reports whether p is a sync API path, under Prefix or (as when
// the server address was entered without it) at the root.
func IsPath(p string) bool {
	return strings.HasPrefix(p, Prefix+"/") || p == "/users/auth" || p == "/users/create" ||
		p == "/syncs/progress" || strings.HasPrefix(p, "/syncs/progress/")
}
