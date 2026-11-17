package kosync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// patterned is P(n) from the kosync conformance vectors
// (github.com/pid1/kosync-conformance, SPEC.md §8.6): byte i is
// (i*31 + i>>8) mod 256.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i>>8)
	}
	return b
}

// sparseSample is the conformance suite's file S: 1 GiB + 1 KiB of zeros,
// except that at each of the twelve sample offsets the offset is written as
// 16 zero-padded decimal digits. It is served from memory.
type sparseSample struct{}

const sparseSize = 1<<30 + 1024

func (sparseSample) ReadAt(p []byte, off int64) (int, error) {
	if off >= sparseSize {
		return 0, io.EOF
	}
	n := len(p)
	if off+int64(n) > sparseSize {
		n = int(sparseSize - off)
	}
	clear(p[:n])
	for i := -1; i <= 10; i++ {
		var at int64
		if i >= 0 {
			at = 1024 << (2 * i)
		}
		mark := []byte(fmt.Sprintf("%016d", at))
		for j := range mark {
			if k := at + int64(j) - off; k >= 0 && k < int64(n) {
				p[k] = mark[j]
			}
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestPartialMD5(t *testing.T) {
	// Values from the conformance suite, computed there by a verbatim LuaJIT
	// transcription of KOReader's util.partialMD5 that also reproduces
	// KOReader's own test vectors.
	for _, tc := range []struct {
		n    int
		want string
	}{
		{500, "21f0df72cce9bc7da8dae2512ee5feed"},     // one short sample
		{1024, "f4cd1641040a17288bb6104d9e66bdb5"},    // one sample
		{1025, "170a888db01c00d8fc3e5d8d84de5838"},    // 0, then 1 byte at 1024
		{2048, "d0e59a0c7b893c3b8d6a9bddbd64e631"},    // 0, 1024
		{3000, "d0e59a0c7b893c3b8d6a9bddbd64e631"},    // same samples as 2048
		{200000, "1784b150454ef4cf6780ceb94af0386f"},  // 0 … 64 KiB
		{1050000, "0fca5c751677ecbc2dcfe80ea75ace32"}, // 0 … 1 MiB
	} {
		got, err := PartialMD5(bytes.NewReader(patterned(tc.n)))
		if err != nil || got != tc.want {
			t.Errorf("P(%d) = %s, %v; want %s", tc.n, got, err, tc.want)
		}
	}
	// All twelve samples (first at 0, last at 1 GiB).
	if got, _ := PartialMD5(sparseSample{}); got != "266ff27c24919f4cec96f90f9b3231ca" {
		t.Errorf("S = %s", got)
	}
	// From a file, as books are read.
	path := filepath.Join(t.TempDir(), "p.bin")
	if err := os.WriteFile(path, patterned(200000), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := PartialMD5File(path); err != nil || got != "1784b150454ef4cf6780ceb94af0386f" {
		t.Errorf("file: %s, %v", got, err)
	}
}

// With KOREADER_TEST_DATA set to a checkout of github.com/koreader/test-data,
// also check the values KOReader's own tests pin (spec/unit/util_spec.lua).
func TestPartialMD5KOReaderFixtures(t *testing.T) {
	dir := os.Getenv("KOREADER_TEST_DATA")
	if dir == "" {
		t.Skip("KOREADER_TEST_DATA not set")
	}
	for file, want := range map[string]string{
		"leaves.epub": "59d481d168cca6267322f150c5f6a2a3",
		"tall.pdf":    "41cce710f34e5ec21315e19c99821415",
	} {
		if got, err := PartialMD5File(filepath.Join(dir, file)); err != nil || got != want {
			t.Errorf("%s = %s, %v; want %s", file, got, err, want)
		}
	}
}

func TestFileNameDigest(t *testing.T) {
	// md5("leaves.epub"), as KOReader's filename matching method sends it.
	if got := FileNameDigest("leaves.epub"); got != MD5Hex("leaves.epub") || len(got) != 32 {
		t.Fatal(got)
	}
}

func TestGuessLimiter(t *testing.T) {
	g := NewGuessLimiter(3, time.Minute)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	g.Now = func() time.Time { return now }
	// The same wrong pair counts once.
	for range 10 {
		g.Fail("1.2.3.4", "a")
	}
	if g.Blocked("1.2.3.4") {
		t.Fatal("blocked after one distinct pair")
	}
	g.Fail("1.2.3.4", "b")
	g.Fail("1.2.3.4", "c")
	if !g.Blocked("1.2.3.4") {
		t.Fatal("not blocked after three distinct pairs")
	}
	if g.Blocked("5.6.7.8") {
		t.Fatal("another address is blocked")
	}
	now = now.Add(2 * time.Minute)
	if g.Blocked("1.2.3.4") {
		t.Fatal("still blocked after the window")
	}
}

func TestKeys(t *testing.T) {
	k := RandomKey()
	if len(k) != 16 || strings.ContainsAny(k, "lo0123456789") || strings.ToLower(k) != k {
		t.Errorf("key %q", k)
	}
	if RandomKey() == k {
		t.Error("two keys alike")
	}
	if h := KeyHash(MD5Hex(k)); len(h) != 64 || h == DummyKeyHash {
		t.Errorf("hash %q", h)
	}
}

func TestJSONFields(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
		ok   bool
	}{{json.Number("0.5"), 0.5, true}, {" 0.25 ", 0.25, true}, {"NaN", 0, false}, {true, 0, false}} {
		if got, ok := JSONFloat(tc.in); ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("JSONFloat(%v) = %v, %v", tc.in, got, ok)
		}
	}
	if s, ok := JSONString(json.Number("12")); !ok || s != "12" {
		t.Errorf("JSONString(number) = %q, %v", s, ok)
	}
	if _, ok := JSONString(nil); ok {
		t.Error("JSONString(nil) accepted")
	}
	for p, want := range map[string]bool{"/kosync/users/auth": true, "/syncs/progress/abc": true, "/users/auth": true, "/kosyncx": false, "/books": false} {
		if IsPath(p) != want {
			t.Errorf("IsPath(%q) = %v", p, !want)
		}
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnauthorized, ErrUnauthorized, "nope")
	if rec.Code != 401 || rec.Header().Get("Content-Type") != "application/json" ||
		strings.TrimSpace(rec.Body.String()) != `{"code":2001,"message":"nope"}` {
		t.Errorf("%d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}
