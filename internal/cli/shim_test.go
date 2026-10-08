package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestShimExecsTheRealBinary(t *testing.T) {
	dir := t.TempDir()
	realBin := filepath.Join(dir, "docker")
	if err := os.WriteFile(realBin, []byte("#!real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var gotPath string
	var gotArgv, gotEnv []string
	code := Run(Env{
		Args: []string{"docker", "run", "-it", "--rm", "alpine", "sh"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv:  func(string) string { return "" }, // the shim reads its environment from Environ
		Environ: func() []string { return []string{"PATH=" + dir, "HEADROOM_WORKTREE=w1"} },
		exec: func(path string, argv, env []string) error {
			gotPath, gotArgv, gotEnv = path, argv, env
			return nil
		},
	})
	if code != 0 || gotPath != realBin {
		t.Fatalf("exit %d, exec %q", code, gotPath)
	}
	if !slices.Equal(gotArgv, []string{"docker", "run", "-it", "--rm", "alpine", "sh"}) {
		t.Errorf("argv = %q", gotArgv)
	}
	if !slices.Contains(gotEnv, "HEADROOM_WORKTREE=w1") {
		t.Errorf("env = %q", gotEnv)
	}
	// The next shim on the way must skip this headroom.
	self, _ := os.Executable()
	if !slices.ContainsFunc(gotEnv, func(kv string) bool {
		return strings.HasPrefix(kv, "HEADROOM_SHIM_SELVES=") && strings.Contains(kv, self)
	}) {
		t.Errorf("env lacks this headroom in HEADROOM_SHIM_SELVES: %q", gotEnv)
	}
}

func TestShimExecFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tart"), []byte("#!real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var errb strings.Builder
	code := Run(Env{Args: []string{"tart", "list"}, Stdout: io.Discard, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"PATH=" + dir} },
		exec:    func(string, []string, []string) error { return errors.New("exec format error") }})
	if code != 126 || !strings.Contains(errb.String(), "exec format error") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

func TestShimDebugLine(t *testing.T) {
	dir := t.TempDir()
	podman := filepath.Join(dir, "podman")
	if err := os.WriteFile(podman, []byte("#!podman\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(podman, filepath.Join(dir, "docker")); err != nil {
		t.Fatal(err)
	}
	var errb strings.Builder
	Run(Env{Args: []string{"docker", "ps"}, Stdout: io.Discard, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"PATH=" + dir, "HEADROOM_SHIM_DEBUG=1"} },
		exec:    func(string, []string, []string) error { return nil }})
	if !strings.Contains(errb.String(), "headroom: docker → "+filepath.Join(dir, "docker")+" (podman)") {
		t.Fatalf("stderr = %q", errb.String())
	}
}
