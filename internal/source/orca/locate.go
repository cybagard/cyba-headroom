package orca

import "github.com/cybagard/cyba-headroom/internal/binpath"

// Locate finds the orca CLI: the configured path, then PATH, then inside
// Orca.app (per-user install first, as for tart). Empty means not installed.
func Locate(configured string, getenv func(string) string) string {
	return binpath.Find(configured, "orca", getenv,
		"~/Applications/Orca.app/Contents/Resources/bin/orca",
		"/Applications/Orca.app/Contents/Resources/bin/orca",
	)
}

// Exec runs the orca CLI at Path.
type Exec = binpath.Exec
