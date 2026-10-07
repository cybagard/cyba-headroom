package orca

import "github.com/cybagard/cyba-headroom/internal/binpath"

// Locate finds the orca CLI: the configured path, then inside Orca.app
// (per-user install first), then PATH. The bundle wins over PATH because
// other tools are also called orca (e.g. plotly's). Empty means not installed.
func Locate(configured string, getenv func(string) string) string {
	noPATH := func(k string) string {
		if k == "PATH" {
			return ""
		}
		return getenv(k)
	}
	if p := binpath.Find(configured, "orca", noPATH,
		"~/Applications/Orca.app/Contents/Resources/bin/orca",
		"/Applications/Orca.app/Contents/Resources/bin/orca",
	); p != "" {
		return p
	}
	return binpath.Find("", "orca", getenv)
}

// Exec runs the orca CLI at Path.
type Exec = binpath.Exec
