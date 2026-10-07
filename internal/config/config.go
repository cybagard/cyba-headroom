// Package config loads headroom's settings from ~/.config/headroom/config.toml.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cybagard/cyba-headroom/internal/budget"
)

// FileName is the config file inside the config directory.
const FileName = "config.toml"

// maxSocketPath is the macOS sun_path limit (104 bytes including the NUL).
const maxSocketPath = 103

// Config is the full set of user settings. Zero thresholds mean "not enforced":
// defaults are set from the Phase 2 observe baseline (#23), not guessed here.
type Config struct {
	// Dir is the directory the config was resolved from. Not read from the file.
	Dir string `toml:"-"`

	// Socket is the daemon's Unix socket. Keep it short: see ADR 0001.
	Socket string `toml:"socket"`
	// ShimDir holds the docker/podman/tart symlinks that agents get first on PATH.
	ShimDir string `toml:"shim_dir"`

	Daemon   Daemon   `toml:"daemon"`
	Docker   Docker   `toml:"docker"`
	Tart     Tart     `toml:"tart"`
	Orca     Orca     `toml:"orca"`
	LMStudio LMStudio `toml:"lmstudio"`
	Policy   Policy   `toml:"policy"`
	Budget   Budget   `toml:"budget"`
}

// Daemon holds collection loop settings (R1).
type Daemon struct {
	// Interval is how often every source is read.
	Interval Duration `toml:"interval"`
	// SourceTimeout bounds one source's read; a slow source goes stale.
	SourceTimeout Duration `toml:"source_timeout"`
	// TrendWindow is how much host pressure history the trend covers (R4).
	TrendWindow Duration `toml:"trend_window"`
}

// Docker locates the Docker Engine API.
type Docker struct {
	// Socket is the engine's Unix socket. Empty means: DOCKER_HOST if it is a
	// unix:// URL, else Docker Desktop's ~/.docker/run/docker.sock.
	Socket string `toml:"socket"`
}

// Tart locates the tart CLI.
type Tart struct {
	// Path is the tart binary. Empty means: PATH, then tart.app and the usual
	// install locations.
	Path string `toml:"path"`
}

// Orca locates the orca CLI.
type Orca struct {
	// Path is the orca CLI. Empty means: PATH, then inside Orca.app.
	Path string `toml:"path"`
}

// LMStudio locates the lms CLI.
type LMStudio struct {
	// Path is the lms CLI. Empty means: PATH, then ~/.lmstudio/bin/lms.
	Path string `toml:"path"`
}

// Policy is the fixed-threshold policy (R8) and lease settings (R10).
type Policy struct {
	MinHeadroomGB    float64  `toml:"min_headroom_gb"`
	PerWorktreeCapGB float64  `toml:"per_worktree_cap_gb"`
	LeaseTimeout     Duration `toml:"lease_timeout"`
	DaemonTimeout    Duration `toml:"daemon_timeout"`
}

// Budget holds inputs to the budget model (R2). GB means GiB, as macOS
// reports memory. `headroom suggest` (#23) learns these from recorded samples.
type Budget struct {
	// HostBaselineGB is reserved for macOS, Orca and the agents' own processes.
	HostBaselineGB float64 `toml:"host_baseline_gb"`
	// DockerOverheadGB is the Docker VM's cost beyond its containers.
	DockerOverheadGB float64 `toml:"docker_overhead_gb"`
	// LMStudioIdleGB is LM Studio's footprint with no model loaded.
	LMStudioIdleGB float64 `toml:"lmstudio_idle_gb"`
	// MaxMacOSVMs is the macOS VM slot count (R6); Apple's licence allows two.
	MaxMacOSVMs int `toml:"max_macos_vms"`
}

// Params converts the settings to the budget model's inputs.
func (b Budget) Params() budget.Params {
	return budget.Params{
		HostBaselineBytes:   gib(b.HostBaselineGB),
		DockerOverheadBytes: gib(b.DockerOverheadGB),
		LMStudioIdleBytes:   gib(b.LMStudioIdleGB),
	}
}

func gib(v float64) uint64 { return uint64(v * (1 << 30)) }

// Duration is a time.Duration that reads TOML strings such as "2m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Defaults returns the configuration used when no file exists, rooted at dir.
func Defaults(dir string) Config {
	return Config{
		Dir:     dir,
		Socket:  filepath.Join(dir, "d.sock"),
		ShimDir: filepath.Join(dir, "shims"),
		Daemon: Daemon{
			Interval:      Duration{5 * time.Second},
			SourceTimeout: Duration{3 * time.Second},
			TrendWindow:   Duration{5 * time.Minute},
		},
		Policy: Policy{
			LeaseTimeout:  Duration{2 * time.Minute},
			DaemonTimeout: Duration{500 * time.Millisecond},
		},
		// Overheads measured in spike #9 and #16; #23 refines them.
		Budget: Budget{DockerOverheadGB: 1.6, LMStudioIdleGB: 0.6, MaxMacOSVMs: 2},
	}
}

// Dir resolves the config directory: $HEADROOM_CONFIG_DIR, then
// $XDG_CONFIG_HOME/headroom, then ~/.config/headroom. It deliberately does not
// use os.UserConfigDir, which is ~/Library/Application Support on macOS.
func Dir(getenv func(string) string) (string, error) {
	if d := getenv("HEADROOM_CONFIG_DIR"); d != "" {
		return d, nil
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "headroom"), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("config: cannot resolve config dir: HOME is not set")
	}
	return filepath.Join(home, ".config", "headroom"), nil
}

// Load reads the config from the resolved directory. A missing file yields
// defaults; settings left empty are resolved from the environment.
func Load(getenv func(string) string) (Config, error) {
	dir, err := Dir(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg, err := LoadDir(dir)
	if err != nil {
		return Config{}, err
	}
	if cfg.Docker.Socket == "" {
		cfg.Docker.Socket = dockerSocket(getenv)
	}
	return cfg, nil
}

// dockerSocket follows the docker CLI: a unix:// DOCKER_HOST, else Docker
// Desktop's per-user socket. A TCP DOCKER_HOST is a remote engine whose
// memory is not this Mac's, so it is ignored. Docker contexts (Colima,
// OrbStack) are not read; set [docker] socket for those. Without HOME it
// stays empty rather than becoming a path relative to the daemon's cwd.
func dockerSocket(getenv func(string) string) string {
	if p, ok := strings.CutPrefix(getenv("DOCKER_HOST"), "unix://"); ok && p != "" {
		return p
	}
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, ".docker", "run", "docker.sock")
	}
	return ""
}

// LoadDir reads dir/config.toml over the defaults. Unknown keys are an error,
// so a typo in a threshold cannot silently disable it.
func LoadDir(dir string) (Config, error) {
	cfg := Defaults(dir)
	path := filepath.Join(dir, FileName)
	md, err := toml.DecodeFile(path, &cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cfg, cfg.Validate()
	case err != nil:
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, len(un))
		for i, k := range un {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("config: %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	cfg.Dir = dir
	cfg.Socket = expandHome(cfg.Socket)
	cfg.ShimDir = expandHome(cfg.ShimDir)
	cfg.Docker.Socket = expandHome(cfg.Docker.Socket)
	cfg.Tart.Path = expandHome(cfg.Tart.Path)
	cfg.Orca.Path = expandHome(cfg.Orca.Path)
	cfg.LMStudio.Path = expandHome(cfg.LMStudio.Path)
	return cfg, cfg.Validate()
}

// Validate reports settings that cannot work.
func (c Config) Validate() error {
	var errs []error
	if len(c.Socket) > maxSocketPath {
		errs = append(errs, fmt.Errorf("socket path is %d bytes, macOS allows %d: %s", len(c.Socket), maxSocketPath, c.Socket))
	}
	if c.Daemon.Interval.Duration <= 0 {
		errs = append(errs, errors.New("daemon.interval must be > 0"))
	}
	if t := c.Daemon.SourceTimeout.Duration; t <= 0 || t >= c.Daemon.Interval.Duration {
		errs = append(errs, errors.New("daemon.source_timeout must be > 0 and < daemon.interval"))
	}
	if c.Daemon.TrendWindow.Duration < 3*c.Daemon.Interval.Duration {
		errs = append(errs, errors.New("daemon.trend_window must be at least 3 x daemon.interval"))
	}
	if c.Policy.MinHeadroomGB < 0 {
		errs = append(errs, errors.New("policy.min_headroom_gb must be >= 0"))
	}
	if c.Policy.PerWorktreeCapGB < 0 {
		errs = append(errs, errors.New("policy.per_worktree_cap_gb must be >= 0"))
	}
	if c.Policy.LeaseTimeout.Duration <= 0 {
		errs = append(errs, errors.New("policy.lease_timeout must be > 0"))
	}
	if c.Policy.DaemonTimeout.Duration <= 0 {
		errs = append(errs, errors.New("policy.daemon_timeout must be > 0"))
	}
	if c.Budget.HostBaselineGB < 0 {
		errs = append(errs, errors.New("budget.host_baseline_gb must be >= 0"))
	}
	if c.Budget.DockerOverheadGB < 0 {
		errs = append(errs, errors.New("budget.docker_overhead_gb must be >= 0"))
	}
	if c.Budget.LMStudioIdleGB < 0 {
		errs = append(errs, errors.New("budget.lmstudio_idle_gb must be >= 0"))
	}
	if c.Budget.MaxMacOSVMs < 0 {
		errs = append(errs, errors.New("budget.max_macos_vms must be >= 0"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
