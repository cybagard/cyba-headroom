package tart

import "github.com/cybagard/cyba-headroom/internal/binpath"

// Locate finds the tart binary: the configured path, then PATH, then tart.app
// and the usual install locations. Empty means tart is not installed.
func Locate(configured string, getenv func(string) string) string {
	return binpath.Find(configured, "tart", getenv,
		"~/Applications/tart.app/Contents/MacOS/tart",
		"/Applications/tart.app/Contents/MacOS/tart",
		"~/.local/bin/tart",
		"/opt/homebrew/bin/tart",
		"/usr/local/bin/tart",
	)
}

// Exec runs the tart binary at Path.
type Exec = binpath.Exec
