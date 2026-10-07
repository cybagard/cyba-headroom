package tart_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/tart"
)

func executable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocate(t *testing.T) {
	home := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	// launchd starts the daemon with a bare PATH, so tart.app's own install
	// location must be found without PATH.
	app := filepath.Join(home, "Applications/tart.app/Contents/MacOS/tart")
	executable(t, app)
	if got := tart.Locate("", env(map[string]string{"HOME": home, "PATH": "/usr/bin:/bin"})); got != app {
		t.Fatalf("Locate = %q, want %q", got, app)
	}

	// PATH wins over the fallbacks.
	onPath := filepath.Join(home, "bin", "tart")
	executable(t, onPath)
	if got := tart.Locate("", env(map[string]string{"HOME": home, "PATH": filepath.Dir(onPath)})); got != onPath {
		t.Fatalf("Locate = %q, want %q", got, onPath)
	}

	// Configured path wins over everything; nothing found means not installed.
	if got := tart.Locate("/opt/tart/tart", env(map[string]string{"HOME": home})); got != "/opt/tart/tart" {
		t.Fatalf("Locate = %q, want the configured path", got)
	}
	if got := tart.Locate("", env(map[string]string{"HOME": t.TempDir(), "PATH": ""})); got != "" && !filepath.IsAbs(got) {
		t.Fatalf("Locate = %q, want empty or a system install", got)
	}
}
