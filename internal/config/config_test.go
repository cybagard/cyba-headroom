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

func TestDockerSocket(t *testing.T) {
	dir := shortTempDir(t)
	cases := []struct {
		name, file string
		env        map[string]string
		want       string
	}{
		{"Docker Desktop default", "", map[string]string{"HOME": "/Users/dev"}, "/Users/dev/.docker/run/docker.sock"},
		{"DOCKER_HOST unix socket", "", map[string]string{"HOME": "/Users/dev", "DOCKER_HOST": "unix:///var/run/docker.sock"}, "/var/run/docker.sock"},
		{"DOCKER_HOST over TCP is not a local engine", "", map[string]string{"HOME": "/Users/dev", "DOCKER_HOST": "tcp://10.0.0.5:2375"}, "/Users/dev/.docker/run/docker.sock"},
		{"config file wins", "[docker]\nsocket = \"/tmp/d.sock\"\n", map[string]string{"HOME": "/Users/dev", "DOCKER_HOST": "unix:///var/run/docker.sock"}, "/tmp/d.sock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write(t, dir, tc.file)
			tc.env["HEADROOM_CONFIG_DIR"] = dir
			cfg, err := Load(env(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Docker.Socket != tc.want {
				t.Fatalf("docker socket = %q, want %q", cfg.Docker.Socket, tc.want)
			}
		})
	}
}

func TestDockerSocketWithoutHOMEIsLeftUnset(t *testing.T) {
	// Never a relative ".docker/run/docker.sock", which would resolve against
	// the daemon's cwd. Unset makes the docker source report the problem.
	cfg, err := Load(env(map[string]string{"HEADROOM_CONFIG_DIR": shortTempDir(t)}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Docker.Socket != "" {
		t.Fatalf("docker socket = %q, want unset", cfg.Docker.Socket)
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
		{"source timeout not below interval", "[daemon]\ninterval = \"2s\"\nsource_timeout = \"2s\"\n", "daemon.source_timeout must be > 0 and < daemon.interval"},
		{"trend window too short", "[daemon]\ninterval = \"1m\"\nsource_timeout = \"3s\"\ntrend_window = \"2m\"\n", "daemon.trend_window must be at least 3 x daemon.interval"},
		{"socket too long", "socket = \"/" + strings.Repeat("s", 110) + "\"\n", "socket path is"},
		{"negative docker overhead", "[budget]\ndocker_overhead_gb = -1\n", "budget.docker_overhead_gb must be >= 0"},
		{"negative lmstudio idle", "[budget]\nlmstudio_idle_gb = -0.5\n", "budget.lmstudio_idle_gb must be >= 0"},
		{"absurd baseline", "[budget]\nhost_baseline_gb = 1e10\n", "budget.host_baseline_gb must be <= 1024"},
		{"absurd docker overhead", "[budget]\ndocker_overhead_gb = 5000\n", "budget.docker_overhead_gb must be <= 1024"},
		{"retention under a day", "[samples]\nretention = \"12h\"\n", "samples.retention must be at least 24h"},
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

func TestBudgetParams(t *testing.T) {
	d := Defaults("/x").Budget
	if d.DockerOverheadGB != 1.6 || d.LMStudioIdleGB != 0.6 || d.HostBaselineGB != 0 {
		t.Fatalf("defaults = %+v, want docker 1.6, lmstudio 0.6, baseline 0 (set by #23)", d)
	}
	p := Budget{HostBaselineGB: 10, DockerOverheadGB: 1.5, LMStudioIdleGB: 0.25}.Params()
	if p.HostBaselineBytes != 10<<30 || p.DockerOverheadBytes != 3<<29 || p.LMStudioIdleBytes != 1<<28 {
		t.Fatalf("params = %+v; GB means GiB, as macOS reports memory", p)
	}
}

func TestSamplesSettings(t *testing.T) {
	d := Defaults("/x")
	if !d.Samples.Enabled || d.Samples.Retention.Duration != 30*24*time.Hour || d.SamplesDir() != "/x/samples" {
		t.Fatalf("defaults = %+v dir %q, want enabled, 720h, /x/samples", d.Samples, d.SamplesDir())
	}
	dir := shortTempDir(t)
	write(t, dir, "[samples]\nenabled = false\nretention = \"336h\"\n")
	cfg, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Samples.Enabled || cfg.Samples.Retention.Duration != 14*24*time.Hour {
		t.Fatalf("loaded %+v", cfg.Samples)
	}
}
