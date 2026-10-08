package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cybagard/cyba-headroom/internal/shim"
)

// shimList is ShimNames in a stable order.
func shimList() []string { return slices.Sorted(maps.Keys(ShimNames)) }

// linkShims makes dir/docker, podman and tart symlinks to bin (R9, #31). A
// link that is already right stays; one to another headroom build is
// replaced; anything else there is left alone and noted.
func linkShims(dir, bin string) (notes []string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, n := range shimList() {
		p := filepath.Join(dir, n)
		target, err := os.Readlink(p)
		switch {
		case err == nil && target == bin:
			continue
		case err == nil && ownLink(p, target, bin):
			if err := os.Remove(p); err != nil {
				return notes, err
			}
		case err == nil:
			notes = append(notes, fmt.Sprintf("%s links to %s, not headroom: left as it is", p, target))
			continue
		case !errors.Is(err, fs.ErrNotExist):
			// Something that is not a link: a real file or dir.
			notes = append(notes, fmt.Sprintf("%s is not a link: left as it is, so %s is not gated", p, n))
			continue
		}
		if err := os.Symlink(bin, p); err != nil {
			return notes, err
		}
	}
	return notes, nil
}

// unlinkShims removes the shim links in dir that lead to a headroom binary,
// and, if removeDir (dir is headroom's own), dir itself once it is empty.
func unlinkShims(dir, bin string, removeDir bool) error {
	for _, n := range shimList() {
		p := filepath.Join(dir, n)
		if target, err := os.Readlink(p); err == nil && ownLink(p, target, bin) {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	if entries, err := os.ReadDir(dir); removeDir && err == nil && len(entries) == 0 {
		return os.Remove(dir)
	}
	return nil
}

// ownLink reports whether the link at p, to target, is headroom's to
// replace or remove: it leads to bin or another headroom binary, also one
// that is gone. Someone else's link, dangling or not, is not.
func ownLink(p, target, bin string) bool {
	return target == bin || isHeadroom(p, target, bin)
}

// isHeadroom reports whether the link at p, to target, leads to a headroom
// binary: one named headroom, or the same file as one of known (the
// installed or the running binary, whatever their names).
func isHeadroom(p, target string, known ...string) bool {
	if shim.HeadroomName(target) || shim.LeadsToHeadroom(p) {
		return true
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	for _, k := range known {
		if ki, err := os.Stat(k); err == nil && os.SameFile(fi, ki) {
			return true
		}
	}
	return false
}

// hasShims reports whether dir holds a shim that leads to a headroom
// binary that exists: then putting dir first on PATH gates something.
func hasShims(dir string) bool {
	self, _ := os.Executable()
	for _, n := range shimList() {
		p := filepath.Join(dir, n)
		target, err := os.Readlink(p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(p); err == nil && isHeadroom(p, target, self) {
			return true
		}
	}
	return false
}

// defaultPath is the PATH a shell starts with when it has none.
const defaultPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// withFirst is the PATH list with dir first and nowhere else. An empty
// PATH stands for the system default, which follows dir.
func withFirst(path, dir string) string {
	if path == "" {
		path = defaultPath
	}
	dir = filepath.Clean(dir)
	parts := []string{dir}
	for _, p := range filepath.SplitList(path) {
		if filepath.Clean(p) != dir {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(filepath.ListSeparator))
}

// shellWord quotes s for a shell command line if it needs it.
func shellWord(s string) string {
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%~", r)
	}
	plain := s != "" && strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
