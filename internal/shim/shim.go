// Package shim finds the real docker, podman or tart behind headroom's shim
// (R5, #26). The shim is headroom itself, linked under those names first on
// PATH; it must never find, and so call, itself.
package shim

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound means no real binary of that name exists outside the shim.
var ErrNotFound = errors.New("not found on PATH (other than headroom's shim)")

// Target is the real binary a shim call goes to.
type Target struct {
	Path string
	// Engine is what the binary really is: docker, podman or tart. A docker
	// that is Podman (a symlink to it, or the podman-docker wrapper script)
	// is podman.
	Engine string
}

// Fallbacks are the usual install locations, tried after PATH. A path
// starting with "~/" is under HOME.
var Fallbacks = map[string][]string{
	"docker": {"/usr/local/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker", "/opt/homebrew/bin/docker"},
	"podman": {"/opt/homebrew/bin/podman", "/opt/podman/bin/podman", "/usr/local/bin/podman"},
	"tart":   {"/opt/homebrew/bin/tart", "~/.local/bin/tart", "/Applications/tart.app/Contents/MacOS/tart"},
}

// Resolve finds the real binary called name: the first executable of that
// name on PATH, then in fallbacks, that is not self (the running headroom,
// however it is linked). Empty and relative PATH entries are skipped: they
// name the current directory, where a repository could plant a fake binary.
func Resolve(name, self string, getenv func(string) string, fallbacks []string) (Target, error) {
	selfInfo, err := os.Stat(self)
	if err != nil {
		return Target{}, err
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
	for _, p := range candidates {
		fi, err := os.Stat(p) // follows symlinks
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 || os.SameFile(fi, selfInfo) {
			continue
		}
		return Target{Path: p, Engine: engine(name, p)}, nil
	}
	return Target{}, ErrNotFound
}

// engine tells what the binary at p, called name, really is.
func engine(name, p string) string {
	if name != "docker" {
		return name
	}
	if target, err := filepath.EvalSymlinks(p); err == nil && filepath.Base(target) == "podman" {
		return "podman"
	}
	// The podman-docker package installs docker as a script that execs
	// podman; a real docker is a large binary.
	f, err := os.Open(p)
	if err != nil {
		return "docker"
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 4096)
	n, _ := f.Read(head)
	head = head[:n]
	if bytes.HasPrefix(head, []byte("#!")) && bytes.Contains(head, []byte("podman")) {
		return "podman"
	}
	return "docker"
}
