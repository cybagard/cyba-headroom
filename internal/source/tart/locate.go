package tart

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Locate finds the tart binary: the configured path, then PATH, then the
// usual install locations. The daemon runs under launchd with a bare PATH,
// so the fallbacks matter. Empty means tart is not installed.
func Locate(configured string, getenv func(string) string) string {
	if configured != "" {
		return configured
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if p := filepath.Join(dir, "tart"); isExecutable(p) {
			return p
		}
	}
	home := getenv("HOME")
	for _, p := range []string{
		filepath.Join(home, "Applications/tart.app/Contents/MacOS/tart"),
		"/Applications/tart.app/Contents/MacOS/tart",
		filepath.Join(home, ".local/bin/tart"),
		"/opt/homebrew/bin/tart",
		"/usr/local/bin/tart",
	} {
		if !strings.HasPrefix(p, "/") {
			continue // HOME unset
		}
		if isExecutable(p) {
			return p
		}
	}
	return ""
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

// Exec runs the tart binary at Path.
type Exec struct{ Path string }

// Run implements CLI.
func (e Exec) Run(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, e.Path, args...).Output()
}
