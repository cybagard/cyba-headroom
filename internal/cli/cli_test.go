package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func run(t *testing.T, env map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(Env{Args: args, Stdout: &out, Stderr: &errb, Getenv: func(k string) string { return env[k] }})
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, nil, "headroom", "version")
	if code != 0 || out != "headroom dev\n" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestShimDispatchOnArgv0(t *testing.T) {
	for _, name := range []string{"docker", "/Users/x/.config/headroom/shims/podman", "tart"} {
		code, _, stderr := run(t, nil, name, "run", "alpine")
		if code != 127 || !strings.Contains(stderr, "shim") {
			t.Errorf("%s: code=%d stderr=%q", name, code, stderr)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, nil, "headroom", "bogus")
	if code != 2 || !strings.Contains(stderr, `unknown command "bogus"`) {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestStatusRejectsExtraArgs(t *testing.T) {
	for _, args := range [][]string{{"--watch"}, {"--json", "foo"}} {
		code, _, stderr := run(t, nil, append([]string{"headroom", "status"}, args...)...)
		if code != 2 || !strings.Contains(stderr, "status") {
			t.Errorf("%v: code=%d stderr=%q", args, code, stderr)
		}
	}
}

func TestNotYetPointsAtIssue(t *testing.T) {
	code, _, stderr := run(t, nil, "headroom", "doctor")
	if code != 1 || !strings.Contains(stderr, "#32") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestConfigPrintsEffectiveConfig(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	code, out, stderr := run(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, "headroom", "config")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{dir + "/config.toml", `lease_timeout = "2m0s"`, "max_macos_vms = 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
