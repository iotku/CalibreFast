package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTypeScriptUpToDate type-checks web/ and fails if the committed
// static/dist/ isn't what the sources build to: someone edited web/ (or a
// template's {{ asset }} list) without running `go generate`, or edited the
// built files by hand.
func TestTypeScriptUpToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the browser code; skipped with -short")
	}
	run := func(what string, args ...string) {
		t.Helper()
		if msg, err := exec.Command("go", args...).CombinedOutput(); err != nil {
			if strings.Contains(string(msg), "requires go >=") {
				t.Skipf("the build tools need a newer Go than this one; skipping:\n%s", msg)
			}
			t.Fatalf("%s failed:\n%s", what, msg)
		}
	}
	run("TypeScript type-check", "tool", "-modfile=tools/go.mod", "tsgo", "-p", "tsconfig.json")
	out := t.TempDir()
	run("tools/jsbuild", "-C", "tools", "run", "./jsbuild", "-out", out)

	list := func(dir string) []string {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}
	want, got := list(out), list(distDir)
	if !slices.Equal(got, want) {
		t.Fatalf("%s is out of date; run `go generate`\n have: %v\n want: %v", distDir, got, want)
	}
	for _, name := range want {
		w, _ := os.ReadFile(filepath.Join(out, name))
		g, _ := os.ReadFile(filepath.Join(distDir, name))
		if !bytes.Equal(g, w) {
			t.Errorf("%s/%s is out of date; run `go generate`", distDir, name)
		}
	}
}
