package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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
	if !strings.Contains(errb.String(), "headroom: docker → "+filepath.Join(dir, "docker")+" (podman; pass)") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

func TestMemoryText(t *testing.T) {
	for b, want := range map[uint64]string{512: "512 B", 512 << 10: "512 KB", 32 << 20: "32 MB", 3000 << 10: "3 MB", 3 << 29: "1.5 GB"} {
		if got := memoryText(b); got != want {
			t.Errorf("memoryText(%d) = %q, want %q", b, got, want)
		}
	}
}

func TestShimDebugLineNamesAGatedCall(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var errb strings.Builder
	Run(Env{Args: []string{"docker", "run", "--rm", "-m", "512m", "-e", "TOKEN=x", "alpine", "true"}, Stdout: io.Discard, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"PATH=" + dir, "HEADROOM_SHIM_DEBUG=1"} },
		exec:    func(string, []string, []string) error { return nil }})
	if want := "(docker; gate: docker run alpine, 512 MB)"; !strings.Contains(errb.String(), want) {
		t.Fatalf("stderr = %q, want %q", errb.String(), want)
	}
}

func TestShimReplacesTheSelvesItWasGiven(t *testing.T) {
	// The child must see one HEADROOM_SHIM_SELVES: Go and libc getenv read
	// the first of duplicates, and would miss this headroom.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var gotEnv []string
	Run(Env{Args: []string{"docker", "ps"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"HEADROOM_SHIM_SELVES=/opt/earlier/headroom", "PATH=" + dir} },
		exec:    func(_ string, _, env []string) error { gotEnv = env; return nil }})
	var selves []string
	for _, kv := range gotEnv {
		if v, ok := strings.CutPrefix(kv, "HEADROOM_SHIM_SELVES="); ok {
			selves = append(selves, v)
		}
	}
	self, _ := os.Executable()
	if len(selves) != 1 || !strings.Contains(selves[0], self) || !strings.Contains(selves[0], "/opt/earlier/headroom") {
		t.Fatalf("HEADROOM_SHIM_SELVES entries = %q", selves)
	}
}

func TestShimPassesTheBareName(t *testing.T) {
	// Called by the shim's full path, the real binary still sees "podman",
	// not a path back into the shim dir.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte("#!real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var gotArgv []string
	Run(Env{Args: []string{"/Users/dev/.config/headroom/shims/podman", "ps"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"PATH=" + dir} },
		exec:    func(_ string, argv, _ []string) error { gotArgv = argv; return nil }})
	if !slices.Equal(gotArgv, []string{"podman", "ps"}) {
		t.Fatalf("argv = %q", gotArgv)
	}
}

func TestShimTargetGoneIsNotFound(t *testing.T) {
	// Removed between lookup and exec (an upgrade): "not found", as the shell
	// would say, not "not runnable".
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!real\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code := Run(Env{Args: []string{"docker", "ps"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"PATH=" + dir} },
		exec:    func(string, []string, []string) error { return syscall.ENOENT }})
	if code != 127 {
		t.Fatalf("exit %d, want 127", code)
	}
}
