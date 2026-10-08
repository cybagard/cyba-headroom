package tart

import "github.com/cybagard/cyba-headroom/internal/binpath"

// Fallbacks are where tart is usually installed, tried after PATH: tart.app
// and the usual install locations. The shim (#26) looks in the same places.
var Fallbacks = []string{
	"~/Applications/tart.app/Contents/MacOS/tart",
	"/Applications/tart.app/Contents/MacOS/tart",
	"~/.local/bin/tart",
	"/opt/homebrew/bin/tart",
	"/usr/local/bin/tart",
}

// Locate finds the tart binary: the configured path, then PATH, then
// Fallbacks. Empty means tart is not installed.
func Locate(configured string, getenv func(string) string) string {
	return binpath.Find(configured, "tart", getenv, Fallbacks...)
}

// Exec runs the tart binary at Path.
type Exec = binpath.Exec
