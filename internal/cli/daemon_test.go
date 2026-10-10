package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// daemonDir is a short config dir whose daemon reads no Docker and keeps no
// samples.
func daemonDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(dir+"/config.toml", []byte("[docker]\nsocket = \""+dir+"/none.sock\"\n[samples]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startDaemon runs args until the log at logPath says the daemon started,
// then stops it, and returns the log.
func startDaemon(t *testing.T, env map[string]string, logPath string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	var errb syncBuffer
	go func() {
		done <- Run(Env{Args: args, Stdout: &errb, Stderr: &errb,
			Getenv: func(k string) string { return env[k] }, Context: ctx})
	}()
	waitFor(t, func() bool { b, _ := os.ReadFile(logPath); return strings.Contains(string(b), "daemon started") })
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, _ := os.ReadFile(logPath)
	return string(b)
}

// --link-shims links the shims to the path the daemon was started from, as
// given, for brew services (#185).
func TestDaemonLinksTheShimsToItsStartPath(t *testing.T) {
	dir := daemonDir(t)
	start := filepath.Join(dir, "opt", "headroom", "bin", "headroom")
	logPath := filepath.Join(dir, "daemon.log")
	startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, logPath, start, "daemon", "--link-shims", "--log", logPath)
	for _, n := range []string{"docker", "podman", "tart"} {
		if target, err := os.Readlink(filepath.Join(dir, "shims", n)); err != nil || target != start {
			t.Errorf("%s -> %q, %v; want %s", n, target, err, start)
		}
	}
}

func TestDaemonLinksNothingFromARelativeStartPath(t *testing.T) {
	dir := daemonDir(t)
	logPath := filepath.Join(dir, "daemon.log")
	log := startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, logPath, "headroom", "daemon", "--link-shims", "--log", logPath)
	if entries, _ := os.ReadDir(filepath.Join(dir, "shims")); len(entries) != 0 {
		t.Errorf("linked %d shims", len(entries))
	}
	if !strings.Contains(log, "not absolute") {
		t.Errorf("log does not say why nothing was linked:\n%s", log)
	}
}

func TestDaemonLeavesWhatIsNotAShimWithANote(t *testing.T) {
	dir := daemonDir(t)
	realDocker := filepath.Join(dir, "shims", "docker")
	if err := os.MkdirAll(filepath.Dir(realDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realDocker, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(dir, "bin", "headroom")
	logPath := filepath.Join(dir, "daemon.log")
	log := startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, logPath, start, "daemon", "--link-shims", "--log", logPath)
	if b, err := os.ReadFile(realDocker); err != nil || string(b) != "#!/bin/sh\n" {
		t.Errorf("docker changed: %q, %v", b, err)
	}
	if !strings.Contains(log, realDocker+" is not a link: left as it is") {
		t.Errorf("no note in the log:\n%s", log)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "shims", "podman")); target != start {
		t.Errorf("podman -> %q", target)
	}
}

func TestDaemonWithoutLinkShimsLinksNothing(t *testing.T) {
	dir := daemonDir(t)
	logPath := filepath.Join(dir, "daemon.log")
	startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, logPath, filepath.Join(dir, "headroom"), "daemon", "--log", logPath)
	if _, err := os.Stat(filepath.Join(dir, "shims")); !os.IsNotExist(err) {
		t.Errorf("shim dir made: %v", err)
	}
}

// --log ~/x logs to $HOME/x: brew services cannot name the home dir (#185).
func TestDaemonLogExpandsTilde(t *testing.T) {
	dir := daemonDir(t)
	home := filepath.Join(dir, "home")
	logPath := filepath.Join(home, "Library", "Logs", "headroom", "daemon.log")
	startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir, "HOME": home}, logPath,
		"headroom", "daemon", "--log", "~/Library/Logs/headroom/daemon.log")
}

func TestDaemonLogWithTildeNeedsHome(t *testing.T) {
	dir := daemonDir(t)
	wd, _ := os.Getwd()
	// Already cancelled: a daemon that starts stops at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errb strings.Builder
	code := Run(Env{Args: []string{"headroom", "daemon", "--log", "~/x.log"}, Stdout: &errb, Stderr: &errb,
		Getenv: func(k string) string { return map[string]string{"HEADROOM_CONFIG_DIR": dir}[k] }, Context: ctx})
	if code != 1 || !strings.Contains(errb.String(), "HOME") {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(wd, "~")); err == nil {
		_ = os.RemoveAll(filepath.Join(wd, "~"))
		t.Error("logged to a dir named ~")
	}
}
