package main

// The launch options can live in a file (-config FILE) instead of on the
// command line: calibrefast.conf.example shows the format. The systemd unit
// (deploy/systemd/) starts CalibreFast with -config, so changing an option
// is an edit to that file and a restart.

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// notInConfig are the flags that run a one-off task and exit, or name the
// config file itself: on the command line only.
var notInConfig = map[string]bool{
	"config":            true,
	"create-user":       true,
	"admin":             true,
	"empty-recycle-bin": true,
}

// applyConfigFile sets fs's flags from the file at path. Each line is
// "name = value" (the flag's name, with or without its dash), or a bare
// "name" for a flag that's on; blank lines and lines starting with # are
// skipped. A value may be in double quotes (Go syntax). Flags given on the
// command line (set) win over the file.
func applyConfigFile(fs *flag.FlagSet, path string, set map[string]bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, hasValue := strings.Cut(line, "=")
		name = strings.TrimLeft(strings.TrimSpace(name), "-")
		value = strings.TrimSpace(value)
		where := fmt.Sprintf("%s:%d", path, n)
		fl := fs.Lookup(name)
		switch {
		case fl == nil:
			return fmt.Errorf("%s: unknown option %q", where, name)
		case notInConfig[name]:
			return fmt.Errorf("%s: -%s only works on the command line", where, name)
		case !hasValue:
			if b, ok := fl.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				return fmt.Errorf("%s: %q needs a value (%s = …)", where, name, name)
			}
			value = "true"
		case strings.HasPrefix(value, `"`):
			uq, err := strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf("%s: bad quoted value for %q: %v", where, name, err)
			}
			value = uq
		}
		if set[name] {
			continue // the command line wins
		}
		if err := fs.Set(name, value); err != nil {
			return fmt.Errorf("%s: %s: %v", where, name, err)
		}
	}
	return sc.Err()
}
