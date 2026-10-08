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
	_ = os.Symlink(filepath.Join(root, "gone", "hr"), filepath.Join(dir, "docker")) // dangling, any name
	if hasShims(dir) {
		t.Fatal("dangling links count as shims")
	}
	if notes, err := linkShims(dir, bin); err != nil || len(notes) != 0 {
		t.Fatalf("notes %q, err %v", notes, err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "docker")); target != bin {
		t.Fatalf("the dangling link was not replaced: %q", target)
	}
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
