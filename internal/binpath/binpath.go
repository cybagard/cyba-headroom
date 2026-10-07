// Package binpath finds the external CLIs headroom runs (tart, orca). The
// daemon runs under launchd with a bare PATH, so each tool's usual install
// locations are tried after PATH.
package binpath

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Find returns configured if set, else name on PATH, else the first
// executable fallback. A fallback starting with "~/" is under HOME; only
// absolute results are used. Empty means not installed.
func Find(configured, name string, getenv func(string) string, fallbacks ...string) string {
	if configured != "" {
		return configured
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if p := filepath.Join(dir, name); executable(p) {
			return p
		}
	}
	home := getenv("HOME")
	for _, p := range fallbacks {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		// A relative HOME would resolve against the daemon's cwd.
		if filepath.IsAbs(p) && executable(p) {
			return p
		}
	}
	return ""
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// Exec runs the binary at Path.
type Exec struct{ Path string }

// Run runs the binary with args and returns its standard output.
func (e Exec) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, e.Path, args...).Output()
}
