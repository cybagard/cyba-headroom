// Package shim finds the real docker, podman or tart behind headroom's shim
// (R5, #26). The shim is headroom itself, linked under those names first on
// PATH; it must never find, and so call, itself or another headroom.
package shim

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/binpath"
	"github.com/cybagard/cyba-headroom/internal/source/tart"
)

// ErrNotFound means no real binary of that name exists outside the shim.
var ErrNotFound = errors.New("not found on PATH (other than headroom's shim)")

// Fallbacks are the usual install locations, tried after PATH. A path
// starting with "~/" is under HOME. Tart's are the daemon's.
var Fallbacks = map[string][]string{
	"docker": {"/usr/local/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker", "/opt/homebrew/bin/docker"},
	"podman": {"/opt/homebrew/bin/podman", "/opt/podman/bin/podman", "/usr/local/bin/podman"},
	"tart":   tart.Fallbacks,
}

// Resolve finds the real binary called name: the first file of that name on
// PATH, then in fallbacks, that this user can execute and that is no
// headroom. It skips selves (the running headroom and any headroom whose
// shim exec'd it, however they are linked) and anything that resolves to a
// binary named headroom (another build's shim links). Empty and relative
// PATH entries are skipped: they name the current directory, where a
// repository could plant a fake binary.
func Resolve(name string, selves []string, getenv func(string) string, fallbacks []string) (string, error) {
	var skip []os.FileInfo
	for _, s := range selves {
		if fi, err := os.Stat(s); err == nil {
			skip = append(skip, fi)
		}
	}
	p := binpath.Search(name, getenv, fallbacks, func(p string, fi os.FileInfo) bool {
		if slices.ContainsFunc(skip, func(s os.FileInfo) bool { return os.SameFile(fi, s) }) {
			return true
		}
		target, err := filepath.EvalSymlinks(p)
		return err == nil && HeadroomName(target)
	})
	if p == "" {
		return "", ErrNotFound
	}
	return p, nil
}

// HeadroomName reports whether the binary at p is named as headroom is
// installed: the one way to tell another headroom build from a real tool.
func HeadroomName(p string) bool { return filepath.Base(p) == "headroom" }

// podmanExec matches a wrapper script line that runs podman.
var podmanExec = regexp.MustCompile(`(?m)^\s*exec\s+(\S*/)?podman(\s|$)`)

// Engine tells what the binary at p, called name, really is: docker, podman
// or tart. A docker that is Podman (a symlink to it, or the podman-docker
// wrapper script that execs it) is podman. It reads the file, so callers ask
// only when they need it. It labels a call; it is not a security boundary.
func Engine(name, p string) string {
	if name != "docker" {
		return name
	}
	if target, err := filepath.EvalSymlinks(p); err == nil && filepath.Base(target) == "podman" {
		return "podman"
	}
	f, err := os.Open(p)
	if err != nil {
		return "docker"
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	head = head[:n]
	if strings.HasPrefix(string(head), "#!") && podmanExec.Match(head) {
		return "podman"
	}
	return "docker"
}
