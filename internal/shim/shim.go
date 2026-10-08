// Package shim finds the real docker, podman or tart behind headroom's shim
// (R5, #26). The shim is headroom itself, linked under those names first on
// PATH; it must never find, and so call, itself or another headroom.
package shim

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrNotFound means no real binary of that name exists outside the shim.
var ErrNotFound = errors.New("not found on PATH (other than headroom's shim)")

// Target is the real binary a shim call goes to.
type Target struct {
	Path string
}

// Fallbacks are the usual install locations, tried after PATH. A path
// starting with "~/" is under HOME.
var Fallbacks = map[string][]string{
	"docker": {"/usr/local/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker", "/opt/homebrew/bin/docker"},
	"podman": {"/opt/homebrew/bin/podman", "/opt/podman/bin/podman", "/usr/local/bin/podman"},
	"tart":   {"/opt/homebrew/bin/tart", "~/.local/bin/tart", "/Applications/tart.app/Contents/MacOS/tart"},
}

// Resolve finds the real binary called name: the first file of that name on
// PATH, then in fallbacks, that this user can execute and that is none of
// selves (the running headroom and any headroom whose shim exec'd it, however
// they are linked). Empty and relative PATH entries are skipped: they name
// the current directory, where a repository could plant a fake binary.
func Resolve(name string, selves []string, getenv func(string) string, fallbacks []string) (Target, error) {
	var skip []os.FileInfo
	for _, s := range selves {
		if fi, err := os.Stat(s); err == nil {
			skip = append(skip, fi)
		}
	}
	var candidates []string
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if filepath.IsAbs(dir) {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, p := range fallbacks {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(getenv("HOME"), rest)
		}
		if filepath.IsAbs(p) {
			candidates = append(candidates, p)
		}
	}
next:
	for _, p := range candidates {
		fi, err := os.Stat(p) // follows symlinks
		if err != nil || !fi.Mode().IsRegular() || unix.Access(p, unix.X_OK) != nil {
			continue
		}
		for _, s := range skip {
			if os.SameFile(fi, s) {
				continue next
			}
		}
		return Target{Path: p}, nil
	}
	return Target{}, ErrNotFound
}

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
