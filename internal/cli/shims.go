package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// shimList is ShimNames in a stable order.
func shimList() []string {
	var names []string
	for n := range ShimNames {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

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
		case err == nil && ownLink(p, target):
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
// and dir itself once it is empty.
func unlinkShims(dir, bin string) error {
	for _, n := range shimList() {
		p := filepath.Join(dir, n)
		if target, err := os.Readlink(p); err == nil && (target == bin || ownLink(p, target)) {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		return os.Remove(dir)
	}
	return nil
}

// ownLink reports whether the link at p, to target, is headroom's to
// replace or remove: it leads to a binary named headroom, or to nothing
// (a link whose binary is gone gates nothing).
func ownLink(p, target string) bool {
	if filepath.Base(target) == "headroom" {
		return true
	}
	_, err := os.Stat(p) // follows the link
	return errors.Is(err, fs.ErrNotExist)
}

// hasShims reports whether dir holds a shim that leads to a headroom
// binary that exists: then putting dir first on PATH gates something.
func hasShims(dir string) bool {
	for _, n := range shimList() {
		p := filepath.Join(dir, n)
		target, err := os.Readlink(p)
		if err != nil || filepath.Base(target) != "headroom" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
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
