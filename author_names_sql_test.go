package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// metadata.db keeps a comma in an author's name as "|" ("Ashton| Hugh"), so
// SQL must never read or match authors.name raw: it goes through
// authorNameSQL / authorNameSQLt (authors.go). This fails on a string
// literal that names the authors table and uses a.name or t.name outside
// replace(). See "Author names with commas" in CLAUDE.md.
func TestNoRawAuthorNameSQL(t *testing.T) {
	authorsTable := regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+authors\b`)
	rawName := regexp.MustCompile(`\b[at]\.name\b`)
	var files []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "TestData") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !authorsTable.MatchString(lit.Value) {
				return true
			}
			s := strings.ReplaceAll(lit.Value, "replace(a.name", "")
			s = strings.ReplaceAll(s, "replace(t.name", "")
			if rawName.MatchString(s) {
				t.Errorf("%s: SQL reads authors.name raw (\"|\" is a comma): use authorNameSQL", fset.Position(lit.Pos()))
			}
			return true
		})
	}
}
