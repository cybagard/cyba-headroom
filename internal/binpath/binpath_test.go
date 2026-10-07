package binpath_test

import (
	"os"
	"path/filepath"
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
