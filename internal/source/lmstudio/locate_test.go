package lmstudio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/source/lmstudio"
)

func TestLocateFindsLMSInItsHomeDir(t *testing.T) {
	home := t.TempDir()
	lms := filepath.Join(home, ".lmstudio/bin/lms")
	if err := os.MkdirAll(filepath.Dir(lms), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lms, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "PATH": "/usr/bin:/bin"}
	if got := lmstudio.Locate("", func(k string) string { return env[k] }); got != lms {
		t.Fatalf("Locate = %q, want %q", got, lms)
	}
}
