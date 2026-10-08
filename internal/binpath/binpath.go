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

	"golang.org/x/sys/unix"
)

// Find returns configured if set, else the first usable name on PATH or in
// fallbacks (see Search). Empty means not installed.
func Find(configured, name string, getenv func(string) string, fallbacks ...string) string {
	if configured != "" {
		return configured
	}
	return Search(name, getenv, fallbacks, nil)
}

// Search returns the first file called name on PATH, then in fallbacks, that
// this user can execute and skip (if set) does not reject. Empty and relative
// PATH entries are not searched: they name the current directory, where a
// repository could plant a fake binary. A fallback starting with "~/" is
// under HOME; only absolute paths are used. Empty means none.
func Search(name string, getenv func(string) string, fallbacks []string, skip func(path string, fi os.FileInfo) bool) string {
	var candidates []string
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	home := getenv("HOME")
	for _, p := range fallbacks {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		// A relative HOME or PATH entry would resolve against the cwd.
		if !filepath.IsAbs(p) {
			continue
		}
		fi, err := os.Stat(p) // follows symlinks
		if err != nil || !fi.Mode().IsRegular() || unix.Access(p, unix.X_OK) != nil {
			continue // missing, or not executable by this user, as the shell checks
		}
		if skip == nil || !skip(p, fi) {
			return p
		}
	}
	return ""
}

// Exec runs the binary at Path.
type Exec struct{ Path string }

// Run runs the binary with args and returns its standard output.
func (e Exec) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, e.Path, args...).Output()
}
