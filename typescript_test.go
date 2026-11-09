package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTypeScriptUpToDate type-checks web/ and fails if the committed
// static/*.js doesn't match what the TypeScript sources compile to — i.e.
// someone edited a .ts file without running `go generate`, or edited the
// generated .js by hand.
func TestTypeScriptUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles TypeScript; skipped with -short")
	}
	out := t.TempDir()
	cmd := exec.Command("go", "tool", "-modfile=tools/go.mod", "tsgo", "-p", "tsconfig.json", "--outDir", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(msg), "requires go >=") {
			t.Skipf("tsgo needs a newer Go than this one; skipping the TypeScript check:\n%s", msg)
		}
		t.Fatalf("TypeScript type-check/compile failed:\n%s", msg)
	}
	built, _ := filepath.Glob(filepath.Join(out, "*.js"))
	if len(built) == 0 {
		t.Fatal("tsgo produced no .js files")
	}
	for _, f := range built {
		name := filepath.Base(f)
		want, _ := os.ReadFile(f)
		got, err := os.ReadFile(filepath.Join("static", name))
		if err != nil {
			t.Errorf("static/%s is missing; run `go generate`", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("static/%s is out of date with web/%s.ts; run `go generate`", name, name[:len(name)-3])
		}
	}
}
