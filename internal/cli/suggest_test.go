package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/samples"
)

// writeSamples writes hours of working samples (5 s apart, 6 GiB in use
// outside every component) for each of the given days before now.
func writeSamples(t *testing.T, dir string, daysAgo []int, hours float64) {
	t.Helper()
	sdir := filepath.Join(dir, "samples")
	_ = os.MkdirAll(sdir, 0o700)
	for _, d := range daysAgo {
		start := time.Now().Add(-time.Duration(d) * 24 * time.Hour).Truncate(time.Hour)
		var b strings.Builder
		for i := 0; i < int(hours*720); i++ {
			un := int64(6 << 30)
			hr := int64(50 << 30)
			s := samples.Sample{V: samples.Version, T: start.Add(time.Duration(i) * 5 * time.Second),
				Host:      &samples.Host{TotalBytes: 64 << 30, Pressure: "normal", FreePercent: 70},
				Budget:    &protocol.Budget{TotalBytes: 64 << 30, UnaccountedBytes: &un, HeadroomBytes: &hr},
				Worktrees: []samples.Worktree{{ID: "w", Path: "/Users/dev/w/a", Agents: []string{"working"}}}}
			line, _ := json.Marshal(s)
			b.Write(line)
			b.WriteByte('\n')
		}
		name := start.Format("2006-01-02") + ".jsonl"
		f, err := os.OpenFile(filepath.Join(sdir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(b.String())
		_ = f.Close()
	}
}

func suggestEnv(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	return dir, map[string]string{"HEADROOM_CONFIG_DIR": dir}
}

func TestSuggestWithTooLittleData(t *testing.T) {
	dir, env := suggestEnv(t)
	writeSamples(t, dir, []int{1}, 2)
	code, out, stderr := run(t, env, "headroom", "suggest")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"DAY", "1440 samples", "# host_baseline_gb: not enough data", "[budget]", "[policy]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSuggestAndWrite(t *testing.T) {
	dir, env := suggestEnv(t)
	writeSamples(t, dir, []int{3, 2, 1}, 2)
	orig := "# mine\n[budget]\nhost_baseline_gb = 2 # old guess\n"
	_ = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(orig), 0o600)

	code, out, stderr := run(t, env, "headroom", "suggest")
	if code != 0 || !strings.Contains(out, "\nhost_baseline_gb = 6 ") {
		t.Fatalf("exit %d, stderr %s, out:\n%s", code, stderr, out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(b) != orig {
		t.Fatal("suggest without --write changed the config")
	}

	code, out, stderr = run(t, env, "headroom", "suggest", "--write")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !strings.Contains(string(b), "host_baseline_gb = 6 # old guess") || !strings.HasPrefix(string(b), "# mine\n") {
		t.Fatalf("config:\n%s", b)
	}
	if bak, _ := os.ReadFile(filepath.Join(dir, "config.toml.bak")); string(bak) != orig {
		t.Fatalf("backup = %q", bak)
	}
	cfg, err := config.LoadDir(dir)
	if err != nil || cfg.Budget.HostBaselineGB != 6 {
		t.Fatalf("config after write: %+v, %v", cfg.Budget, err)
	}
	if !strings.Contains(out, "headroom install") {
		t.Errorf("no restart hint:\n%s", out)
	}
}

func TestSuggestSinceLeavesOutOlderDays(t *testing.T) {
	dir, env := suggestEnv(t)
	writeSamples(t, dir, []int{10, 9, 8}, 2)
	_, out, _ := run(t, env, "headroom", "suggest", "--since", "5d")
	if !strings.Contains(out, "no samples") {
		t.Fatalf("out:\n%s", out)
	}
	_, out, _ = run(t, env, "headroom", "suggest", "--since", "336h")
	if !strings.Contains(out, "host_baseline_gb = 6") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestSuggestArgs(t *testing.T) {
	_, env := suggestEnv(t)
	for _, args := range [][]string{{"--since"}, {"--since", "soon"}, {"--since", "-1d"}, {"--bogus"}} {
		code, _, stderr := run(t, env, append([]string{"headroom", "suggest"}, args...)...)
		if code != 2 {
			t.Errorf("%v: exit %d, %s", args, code, stderr)
		}
	}
}
