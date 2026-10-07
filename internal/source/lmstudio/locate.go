package lmstudio

import "github.com/cybagard/cyba-headroom/internal/binpath"

// Locate finds the lms CLI: the configured path, then PATH, then
// ~/.lmstudio/bin, where LM Studio installs it. Empty means not installed.
func Locate(configured string, getenv func(string) string) string {
	return binpath.Find(configured, "lms", getenv, "~/.lmstudio/bin/lms")
}

// Exec runs the lms CLI at Path.
type Exec = binpath.Exec
