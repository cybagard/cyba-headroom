package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDir(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"explicit override wins", map[string]string{"HEADROOM_CONFIG_DIR": "/x", "XDG_CONFIG_HOME": "/y", "HOME": "/h"}, "/x"},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": "/y", "HOME": "/h"}, "/y/headroom"},
		{"home, not Library/Application Support", map[string]string{"HOME": "/h"}, "/h/.config/headroom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Dir(env(tt.env))
			if err != nil || got != tt.want {
				t.Fatalf("Dir() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := Dir(env(nil)); err == nil {
		t.Fatal("Dir() with no HOME: want error")
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	dir := shortTempDir(t)
	cfg, err := Load(env(map[string]string{"HEADROOM_CONFIG_DIR": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != filepath.Join(dir, "d.sock") || cfg.ShimDir != filepath.Join(dir, "shims") {
		t.Errorf("paths = %q, %q", cfg.Socket, cfg.ShimDir)
	}
	if cfg.Policy.LeaseTimeout.Duration != 2*time.Minute {
		t.Errorf("lease timeout = %v, want 2m (R10)", cfg.Policy.LeaseTimeout)
	}
	if cfg.Policy.DaemonTimeout.Duration != 500*time.Millisecond {
		t.Errorf("daemon timeout = %v, want 500ms (R7)", cfg.Policy.DaemonTimeout)
	}
	if cfg.Policy.MinHeadroomGB != 0 || cfg.Policy.PerWorktreeCapGB != 0 {
		t.Error("thresholds must default to 0 (unset) until the baseline (#23)")
	}
}

func TestLoadFile(t *testing.T) {
	dir := shortTempDir(t)
	write(t, dir, `
socket = "/tmp/hr.sock"

[policy]
min_headroom_gb = 6
per_worktree_cap_gb = 12.5
lease_timeout = "90s"

[budget]
host_baseline_gb = 10
`)
	cfg, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "/tmp/hr.sock" || cfg.Policy.MinHeadroomGB != 6 || cfg.Policy.PerWorktreeCapGB != 12.5 ||
		cfg.Policy.LeaseTimeout.Duration != 90*time.Second || cfg.Budget.HostBaselineGB != 10 {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if cfg.Policy.DaemonTimeout.Duration != 500*time.Millisecond || cfg.Budget.MaxMacOSVMs != 2 {
		t.Error("keys absent from the file must keep their defaults")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, file, wantErr string
	}{
		{"typo in key", "[policy]\nmin_headroom = 6\n", "unknown keys: policy.min_headroom"},
		{"negative threshold", "[policy]\nmin_headroom_gb = -1\n", "min_headroom_gb must be >= 0"},
		{"bad duration", "[policy]\nlease_timeout = \"soon\"\n", "lease_timeout"},
		{"socket too long", "socket = \"/" + strings.Repeat("s", 110) + "\"\n", "socket path is"},
		{"malformed toml", "socket = \n", "config:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := shortTempDir(t)
			write(t, dir, tt.file)
			_, err := LoadDir(dir)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestExpandHome(t *testing.T) {
	t.Setenv("HOME", "/h")
	if got := expandHome("~/x/d.sock"); got != "/h/x/d.sock" {
		t.Errorf("expandHome = %q", got)
	}
	if got := expandHome("/abs"); got != "/abs" {
		t.Errorf("expandHome = %q", got)
	}
}

// shortTempDir keeps socket paths under the macOS limit; t.TempDir() can exceed it.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
