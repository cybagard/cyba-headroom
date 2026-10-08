package binpath_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/binpath"
)

func TestRelativeHOMEIsNotUsed(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	// "rel/bin/tool" exists relative to the cwd; a relative HOME must not reach it.
	if err := os.MkdirAll(filepath.Join(dir, "rel", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rel", "bin", "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": "rel"}
	if got := binpath.Find("", "tool", func(k string) string { return env[k] }, "~/bin/tool"); got != "" {
		t.Fatalf("Find = %q, want nothing for a relative HOME", got)
	}
}

func TestSkipsRelativePATHEntriesAndWhatCannotRun(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, p := range []string{"tool", "rel/tool", "others/tool", "abs/tool"} {
		mode := os.FileMode(0o755)
		if p == "others/tool" {
			mode = 0o011 // executable, but not by this user
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	path := strings.Join([]string{"", ".", "rel", filepath.Join(dir, "others"), filepath.Join(dir, "abs")}, string(os.PathListSeparator))
	env := map[string]string{"PATH": path}
	if got, want := binpath.Find("", "tool", func(k string) string { return env[k] }), filepath.Join(dir, "abs", "tool"); got != want {
		t.Fatalf("Find = %q, want %q", got, want)
	}
}

func TestSearchSkip(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, d, "tool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"PATH": filepath.Join(dir, "a") + string(os.PathListSeparator) + filepath.Join(dir, "b")}
	skipA := func(p string, _ os.FileInfo) bool { return filepath.Base(filepath.Dir(p)) == "a" }
	if got, want := binpath.Search("tool", func(k string) string { return env[k] }, nil, skipA), filepath.Join(dir, "b", "tool"); got != want {
		t.Fatalf("Search = %q, want %q", got, want)
	}
}
