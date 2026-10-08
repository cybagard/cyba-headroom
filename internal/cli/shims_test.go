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
	_ = os.Symlink("/opt/homebrew/bin/tart", filepath.Join(dir, "tart"))
	notes, err = linkShims(dir, bin)
	if target, _ := os.Readlink(filepath.Join(dir, "docker")); err != nil || target != bin {
		t.Fatalf("docker -> %q, %v", target, err)
	}
	if len(notes) != 2 || !strings.Contains(strings.Join(notes, "\n"), "podman") || !strings.Contains(strings.Join(notes, "\n"), "tart") {
		t.Fatalf("notes %q", notes)
	}
	// Uninstall removes only headroom's links.
	if err := unlinkShims(dir, bin); err != nil {
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
	if err := unlinkShims(dir, bin); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the empty shim dir stayed")
	}
	if err := unlinkShims(dir, bin); err != nil {
		t.Fatalf("nothing to remove: %v", err)
	}
}
