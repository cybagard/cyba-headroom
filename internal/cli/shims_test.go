package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkShims(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "headroom")
	dir := filepath.Join(root, "shims")
	notes, err := linkShims(dir, bin)
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes %q, err %v", notes, err)
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if target, err := os.Readlink(filepath.Join(dir, n)); err != nil || target != bin {
			t.Fatalf("%s -> %q, %v", n, target, err)
		}
	}
	// Again: nothing changes.
	if notes, err := linkShims(dir, bin); err != nil || len(notes) != 0 {
		t.Fatalf("second run: %q, %v", notes, err)
	}
	// A link to another headroom build is replaced; a real file, or a link
	// to something else, is left with a note.
	_ = os.Remove(filepath.Join(dir, "docker"))
	_ = os.Symlink("/opt/old/headroom", filepath.Join(dir, "docker"))
	_ = os.Remove(filepath.Join(dir, "podman"))
	_ = os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.Remove(filepath.Join(dir, "tart"))
	realTart := filepath.Join(root, "brew", "tart")
	_ = os.MkdirAll(filepath.Dir(realTart), 0o755)
	_ = os.WriteFile(realTart, []byte("#!tart\n"), 0o755)
	_ = os.Symlink(realTart, filepath.Join(dir, "tart"))
	notes, err = linkShims(dir, bin)
	if target, _ := os.Readlink(filepath.Join(dir, "docker")); err != nil || target != bin {
		t.Fatalf("docker -> %q, %v", target, err)
	}
	if len(notes) != 2 || !strings.Contains(strings.Join(notes, "\n"), "podman") || !strings.Contains(strings.Join(notes, "\n"), "tart") {
		t.Fatalf("notes %q", notes)
	}
	// Uninstall removes only headroom's links.
	if _, _, err := unlinkShims(dir, bin, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "docker")); !os.IsNotExist(err) {
		t.Fatal("headroom's link stayed")
	}
	for _, n := range []string{"podman", "tart"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s was removed: %v", n, err)
		}
	}
	// An empty shim dir goes too.
	_ = os.Remove(filepath.Join(dir, "podman"))
	_ = os.Remove(filepath.Join(dir, "tart"))
	_, _ = linkShims(dir, bin)
	if _, _, err := unlinkShims(dir, bin, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the empty shim dir stayed")
	}
	if _, _, err := unlinkShims(dir, bin, true, false); err != nil {
		t.Fatalf("nothing to remove: %v", err)
	}
}

// A link whose target is gone is headroom's to replace or remove: it gates
// nothing. The gate is on when any shim leads to a binary that exists.
func TestDanglingShimsAndPartialShims(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "headroom")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!headroom\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "shims")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(filepath.Join(root, "gone", "headroom"), filepath.Join(dir, "docker")) // a headroom that is gone
	_ = os.Symlink(filepath.Join(root, "gone", "Docker.app"), filepath.Join(dir, "tart")) // someone else's, also gone
	if hasShims(dir) {
		t.Fatal("dangling links count as shims")
	}
	notes, err := linkShims(dir, bin)
	if target, _ := os.Readlink(filepath.Join(dir, "docker")); err != nil || target != bin {
		t.Fatalf("the gone headroom's link was not replaced: %q, %v", target, err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "tart")); target == bin || len(notes) != 1 {
		t.Fatalf("someone else's link was replaced: %q, notes %q", target, notes)
	}
	_ = os.Remove(filepath.Join(dir, "tart"))
	_, _ = linkShims(dir, bin)
	// One real file among the shims: the other two still gate.
	_ = os.Remove(filepath.Join(dir, "podman"))
	_ = os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\n"), 0o755)
	if !hasShims(dir) {
		t.Fatal("one foreign entry turned the whole gate off")
	}
}

func TestWithFirst(t *testing.T) {
	for _, c := range []struct{ path, want string }{
		{"/a:/s:/b", "/s:/a:/b"},
		{"/s/", "/s"},
		{"", "/s:/usr/bin:/bin:/usr/sbin:/sbin"}, // an empty PATH means the system default
	} {
		if got := withFirst(c.path, "/s"); got != c.want {
			t.Errorf("withFirst(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/dev/.local/bin/headroom": "/Users/dev/.local/bin/headroom",
		"/Users/Jane Doe/bin/headroom":   "'/Users/Jane Doe/bin/headroom'",
		"/x/it's/headroom":               `'/x/it'\''s/headroom'`,
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// A shim dir the user chose, not headroom's default, stays after uninstall;
// a link through another link to headroom counts as headroom's.
func TestUnlinkKeepsAChosenDirAndSeesChainedLinks(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "headroom")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	_ = os.WriteFile(bin, []byte("#!headroom\n"), 0o755)
	alias := filepath.Join(root, "bin", "hr")
	_ = os.Symlink(bin, alias)
	dir := filepath.Join(root, "mybin")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.Symlink(alias, filepath.Join(dir, "docker"))
	if !hasShims(dir) {
		t.Fatal("a link to a link to headroom is not a shim")
	}
	if _, _, err := unlinkShims(dir, bin, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "docker")); !os.IsNotExist(err) {
		t.Fatal("the chained link stayed")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the user's dir was removed: %v", err)
	}
}

// A shim link that someone else removes between uninstall's read and its
// remove is already gone: no error, not listed as removed, and the next
// shims are still removed (#193).
func TestUnlinkShimsTakesALinkGoneUnderItAsGone(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "headroom")
	dir := filepath.Join(root, "shims")
	if _, err := linkShims(dir, bin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeLink = os.Remove })
	removeLink = func(p string) error {
		if filepath.Base(p) == "docker" {
			_ = os.Remove(p) // the other daemon's remove, first
		}
		return os.Remove(p)
	}
	removed, _, err := unlinkShims(dir, bin, true, false)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if strings.Join(removed, " ") != "podman tart" {
		t.Errorf("removed %q, want podman and tart", removed)
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if _, err := os.Lstat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s stayed: %v", n, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the empty shim dir stayed: %v", err)
	}
}

// In the both state, Homebrew's daemon may link its shims into the folder
// between uninstall's emptiness check and the folder's removal: the folder
// stays, with no error, and its shims are reported as kept (#194).
func TestUnlinkShimsKeepsAFolderFilledUnderIt(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "headroom")
	opt := filepath.Join(root, "opt", "headroom", "bin", "headroom") // Homebrew's
	dir := filepath.Join(root, "shims")
	if _, err := linkShims(dir, bin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeShimDir = os.Remove })
	removeShimDir = func(p string) error {
		if _, err := linkShims(p, opt); err != nil { // the other daemon, first
			t.Fatal(err)
		}
		return os.Remove(p)
	}
	removed, kept, err := unlinkShims(dir, bin, true, true)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if strings.Join(removed, " ") != "docker podman tart" || strings.Join(kept, " ") != "docker podman tart" {
		t.Errorf("removed %q, kept %q; want all three each", removed, kept)
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if got, err := os.Readlink(filepath.Join(dir, n)); err != nil || got != opt {
			t.Errorf("%s -> %q, %v", n, got, err)
		}
	}
}
