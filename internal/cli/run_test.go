package cli

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runRig is a config dir with shim links and a PATH holding an agent.
type runRig struct {
	cfg, shims, tools string
	env               []string
	execed            string
	argv, execEnv     []string
}

func newRunRig(t *testing.T, links bool) *runRig {
	t.Helper()
	cfg, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cfg) })
	r := &runRig{cfg: cfg, shims: filepath.Join(cfg, "shims"), tools: filepath.Join(cfg, "tools")}
	if err := os.MkdirAll(r.tools, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.tools, "agent"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if links {
		bin := filepath.Join(cfg, "bin", "headroom")
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("#!headroom\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := linkShims(r.shims, bin); err != nil {
			t.Fatal(err)
		}
	}
	r.env = []string{"HEADROOM_CONFIG_DIR=" + cfg, "PATH=" + r.tools + ":/usr/bin", "ORCA_WORKTREE_ID=repo::/Users/dev/src/a"}
	return r
}

func (r *runRig) run(args ...string) (int, string) {
	var errb strings.Builder
	code := Run(Env{Args: append([]string{"headroom", "run"}, args...), Stdout: io.Discard, Stderr: &errb,
		Getenv:  func(string) string { return "" }, // run reads Environ, as the shim does
		Environ: func() []string { return r.env },
		exec: func(path string, argv, env []string) error {
			r.execed, r.argv, r.execEnv = path, argv, env
			return nil
		}})
	return code, errb.String()
}

func pathOf(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}

func TestRunPutsTheShimsFirst(t *testing.T) {
	r := newRunRig(t, true)
	r.env[1] = "PATH=" + r.tools + ":" + r.shims + ":/usr/bin" // already there, but last
	code, stderr := r.run("--", "agent", "--flag", "x")
	if code != 0 || r.execed != filepath.Join(r.tools, "agent") || stderr != "" {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
	if got, want := pathOf(r.execEnv), r.shims+":"+r.tools+":/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if !slices.Equal(r.argv, []string{"agent", "--flag", "x"}) || !slices.Contains(r.execEnv, "ORCA_WORKTREE_ID=repo::/Users/dev/src/a") {
		t.Fatalf("argv %q, env %q", r.argv, r.execEnv)
	}
}

func TestRunWithoutShimsStillRuns(t *testing.T) {
	r := newRunRig(t, false)
	code, stderr := r.run("--", "agent")
	if code != 0 || r.execed == "" || pathOf(r.execEnv) != r.tools+":/usr/bin" ||
		strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "headroom install") {
		t.Fatalf("exit %d, PATH %q, stderr %q", code, pathOf(r.execEnv), stderr)
	}
}

func TestRunErrors(t *testing.T) {
	r := newRunRig(t, true)
	if code, stderr := r.run("--", "no-such-agent"); code != 127 || !strings.Contains(stderr, "no-such-agent") {
		t.Fatalf("missing: exit %d, %q", code, stderr)
	}
	for _, args := range [][]string{{}, {"agent"}, {"--"}} {
		if code, _ := r.run(args...); code != 2 {
			t.Errorf("%q: exit %d, want usage 2", args, code)
		}
	}
}

// The agent is found on the PATH it will get: with no PATH at all, the
// system default after the shims.
func TestRunWithNoPath(t *testing.T) {
	r := newRunRig(t, true)
	r.env[1] = "PATH="
	code, _ := r.run("--", "sh")
	if code != 0 || r.execed != "/bin/sh" && r.execed != "/usr/bin/sh" {
		t.Fatalf("exit %d, exec %q", code, r.execed)
	}
}

// A broken config does not take the shims off PATH: the default shim dir
// still gates, and the shims themselves fail open (R7).
func TestRunWithABrokenConfigKeepsTheShims(t *testing.T) {
	r := newRunRig(t, true)
	if err := os.WriteFile(filepath.Join(r.cfg, "config.toml"), []byte("not toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stderr := r.run("--", "agent")
	if code != 0 || !strings.HasPrefix(pathOf(r.execEnv), r.shims+":") || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("exit %d, PATH %q, stderr %q", code, pathOf(r.execEnv), stderr)
	}
}

// Shims linked to a headroom binary under another name (install --bin)
// still count, when it is the binary running.
func TestRunKnowsItsOwnBinaryByAnyName(t *testing.T) {
	r := newRunRig(t, false)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.shims, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(r.shims, "docker")); err != nil { // the test binary: not named headroom
		t.Fatal(err)
	}
	if code, stderr := r.run("--", "agent"); code != 0 || stderr != "" || !strings.HasPrefix(pathOf(r.execEnv), r.shims+":") {
		t.Fatalf("exit %d, stderr %q, PATH %q", code, stderr, pathOf(r.execEnv))
	}
}

// A broken config with no shims says the agent runs without the gate.
func TestRunSaysWhenTheGateIsOff(t *testing.T) {
	r := newRunRig(t, false)
	if err := os.WriteFile(filepath.Join(r.cfg, "config.toml"), []byte("not toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stderr := r.run("--", "agent"); code != 0 || !strings.Contains(stderr, "without the gate") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
