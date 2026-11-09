// Command jsbuild builds the browser assets into static/dist/.
//
// Every script or stylesheet a template names with {{ asset "name" }} is an
// entry: web/<name>.ts for "<name>.js", web/<name>.css for "<name>.css".
// esbuild bundles each entry with everything it imports (one request per
// page instead of an import waterfall), minifies it, and names the file by
// its content hash, so the server can let browsers cache it forever.
// manifest.json maps each name to its file; the server reads it at startup.
//
// Type checking is tsgo's job (go generate runs it first); esbuild only
// strips the types.
//
// Usage, from the repository root (go generate does this):
//
//	go -C tools run ./jsbuild [-out dir]
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

func main() {
	root := flag.String("root", "..", "repository root")
	out := flag.String("out", "", "where to write the files (default <root>/static/dist)")
	flag.Parse()
	if err := build(*root, *out); err != nil {
		fmt.Fprintln(os.Stderr, "jsbuild:", err)
		os.Exit(1)
	}
}

// assetRe matches {{ asset "name" }} in templates.
var assetRe = regexp.MustCompile(`\basset\s+"([^"]+)"`)

// entries returns the asset names the templates use, sorted.
func entries(root string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(root, "templates", "*.html"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for _, m := range assetRe.FindAllSubmatch(b, -1) {
			names = append(names, string(m[1]))
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// source is the file an asset name is built from.
func source(name string) (string, error) {
	switch ext := filepath.Ext(name); ext {
	case ".js":
		return filepath.Join("web", strings.TrimSuffix(name, ext)+".ts"), nil
	case ".css":
		return filepath.Join("web", name), nil
	}
	return "", fmt.Errorf("asset %q: only .js and .css are built", name)
}

func build(root, out string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	// esbuild always builds "into" static/dist, so source map paths are the
	// same wherever the files are written (the up-to-date test writes them
	// elsewhere and compares).
	dist := filepath.Join(root, "static", "dist")
	if out == "" {
		out = dist
	}

	names, err := entries(root)
	if err != nil {
		return err
	}
	bySource := map[string]string{} // web/x.ts -> asset name
	var points []string
	for _, name := range names {
		src, err := source(name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(root, src)); err != nil {
			return fmt.Errorf("asset %q: %w", name, err)
		}
		bySource[filepath.ToSlash(src)] = name
		points = append(points, src)
	}

	res := api.Build(api.BuildOptions{
		AbsWorkingDir:     root,
		EntryPoints:       points,
		Bundle:            true,
		Outdir:            dist,
		EntryNames:        "[name]-[hash]",
		Format:            api.FormatESModule,
		Target:            api.ES2022,
		MinifyWhitespace:  true,
		MinifyIdentifiers: true,
		MinifySyntax:      true,
		Charset:           api.CharsetUTF8,
		Sourcemap:         api.SourceMapLinked, // only fetched by devtools
		External:          []string{"https://*"},
		Metafile:          true,
		LogLevel:          api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		msgs := api.FormatMessages(res.Errors, api.FormatMessagesOptions{Kind: api.ErrorMessage})
		return fmt.Errorf("esbuild:\n%s", strings.Join(msgs, ""))
	}

	// Name -> built file, from the metafile.
	var meta struct {
		Outputs map[string]struct {
			EntryPoint string `json:"entryPoint"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal([]byte(res.Metafile), &meta); err != nil {
		return err
	}
	manifest := map[string]string{}
	for path, o := range meta.Outputs {
		if name, ok := bySource[o.EntryPoint]; ok {
			manifest[name] = filepath.Base(path)
		}
	}
	if len(manifest) != len(names) {
		return fmt.Errorf("built %d of %d assets", len(manifest), len(names))
	}

	// Replace the previous build.
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, f := range res.OutputFiles {
		if err := os.WriteFile(filepath.Join(out, filepath.Base(f.Path)), f.Contents, 0o644); err != nil {
			return err
		}
	}
	m, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "manifest.json"), append(m, '\n'), 0o644); err != nil {
		return err
	}

	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(out, manifest[name]))
		if err != nil {
			return err
		}
		fmt.Printf("%-18s %-28s %6.1f KB  (%.1f KB gzip)\n", name, manifest[name], float64(len(b))/1024, float64(gzipSize(b))/1024)
	}
	return nil
}

func gzipSize(b []byte) int {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Len()
}
