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
		case err == nil && filepath.Base(target) == "headroom":
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
		if target, err := os.Readlink(p); err == nil && (target == bin || filepath.Base(target) == "headroom") {
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

// hasShims reports whether dir holds a link for every shim name.
func hasShims(dir string) bool {
	for _, n := range shimList() {
		if fi, err := os.Lstat(filepath.Join(dir, n)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return false
		}
	}
	return true
}

// withFirst is the PATH list with dir first and nowhere else.
func withFirst(path, dir string) string {
	parts := []string{dir}
	for _, p := range filepath.SplitList(path) {
		if filepath.Clean(p) != filepath.Clean(dir) {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, string(filepath.ListSeparator))
}
