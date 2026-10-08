package cli

import (
	"io"
	"testing"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
)

// The shim's own cost on a call that starts nothing: parse and resolve.
func BenchmarkShimPassThrough(b *testing.B) {
	r := newShimRig(b)
	env := Env{Args: []string{"docker", "ps"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, Environ: func() []string { return r.env },
		exec: func(string, []string, []string) error { return nil }}
	for b.Loop() {
		if Run(env) != 0 {
			b.Fatal("pass-through failed")
		}
	}
}

// A gated call: config load, the check round trip to a daemon with the
// real wiring (policy and lease book), and the exec hook.
func BenchmarkShimGatedCall(b *testing.B) {
	envMap, _ := serveDaemonWith(b, func(d *daemon.Daemon) { wireGate(d, config.Defaults("/x"), discardLog(), nil) })
	r := newShimRig(b)
	vars := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	env := Env{Args: []string{"docker", "run", "-m", "1m", "alpine"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, Environ: func() []string { return vars },
		exec: func(string, []string, []string) error { return nil }}
	for b.Loop() {
		if Run(env) != 0 {
			b.Fatal("gated call failed")
		}
	}
}
