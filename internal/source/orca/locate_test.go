package orca_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/orca"
)

func TestLocateFindsTheAppBundleWithoutPATH(t *testing.T) {
	home := t.TempDir()
	// Orca ships its CLI inside the app; launchd gives the daemon a bare PATH.
	app := filepath.Join(home, "Applications/Orca.app/Contents/Resources/bin/orca")
	if err := os.MkdirAll(filepath.Dir(app), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "PATH": "/usr/bin:/bin"}
	if got := orca.Locate("", func(k string) string { return env[k] }); got != app {
		t.Fatalf("Locate = %q, want %q", got, app)
	}
	if got := orca.Locate("/opt/orca", func(k string) string { return env[k] }); got != "/opt/orca" {
		t.Fatalf("Locate = %q, want the configured path", got)
	}
}
