package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// selfLink makes path a link to the running binary, as Homebrew's opt path
// is to the formula's.
func selfLink(t *testing.T, path string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// --link-shims links the shims to the path the daemon was started from, as
// given, for brew services (#185).
func TestDaemonLinksTheShimsToItsStartPath(t *testing.T) {
	dir := daemonDir(t)
	start := selfLink(t, filepath.Join(dir, "opt", "headroom", "bin", "headroom"))
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

// The running binary itself, not only a link to it, gets the shims (#187).
func TestDaemonStartedAsItselfLinksTheShims(t *testing.T) {
	dir := daemonDir(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "daemon.log")
	startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, logPath, self, "daemon", "--link-shims", "--log", logPath)
	if target, err := os.Readlink(filepath.Join(dir, "shims", "docker")); err != nil || target != self {
		t.Errorf("docker -> %q, %v; want %s", target, err, self)
	}
}

// An absolute argv[0] that is another file (a copy, /usr/bin/true) would
// send every shim call to it: nothing is linked, and the log says why (#187).
func TestDaemonLinksNothingFromAnotherBinary(t *testing.T) {
	for name, start := range map[string]func(dir string) string{
		"a copy": func(dir string) string {
			p := filepath.Join(dir, "copy", "headroom")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("#!binary v0"), 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		},
		"a missing file": func(dir string) string { return filepath.Join(dir, "gone", "headroom") },
	} {
		t.Run(name, func(t *testing.T) {
			dir := daemonDir(t)
			logPath := filepath.Join(dir, "daemon.log")
			log := startDaemon(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, logPath, start(dir), "daemon", "--link-shims", "--log", logPath)
			if entries, _ := os.ReadDir(filepath.Join(dir, "shims")); len(entries) != 0 {
				t.Errorf("linked %d shims", len(entries))
			}
			if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "not the running binary") {
				t.Errorf("log does not say why nothing was linked:\n%s", log)
			}
		})
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
	start := selfLink(t, filepath.Join(dir, "bin", "headroom"))
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

// A daemon that cannot take the socket links nothing: the running daemon's
// shims stay as they are (#185).
func TestASecondDaemonLeavesTheShimsAlone(t *testing.T) {
	dir := daemonDir(t)
	env := func(k string) string { return map[string]string{"HEADROOM_CONFIG_DIR": dir}[k] }
	first := selfLink(t, filepath.Join(dir, "first", "headroom"))
	logPath := filepath.Join(dir, "first.log")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan int, 1)
	go func() {
		done <- Run(Env{Args: []string{first, "daemon", "--link-shims", "--log", logPath},
			Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Getenv: env, Context: ctx})
	}()
	waitFor(t, func() bool { b, _ := os.ReadFile(logPath); return strings.Contains(string(b), "daemon started") })

	var errb syncBuffer
	second := selfLink(t, filepath.Join(dir, "second", "headroom"))
	if code := Run(Env{Args: []string{second, "daemon", "--link-shims"},
		Stdout: &errb, Stderr: &errb, Getenv: env, Context: ctx}); code == 0 {
		t.Errorf("second daemon exited 0: %s", errb.String())
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if target, err := os.Readlink(filepath.Join(dir, "shims", n)); err != nil || target != first {
			t.Errorf("%s -> %q, %v; want %s", n, target, err, first)
		}
	}
	cancel()
	<-done
}

// With --wait, a second daemon waits for the first to stop, logging once,
// then starts and links the shims, as brew services beside a plain install
// needs (#192).
func TestASecondDaemonWithWaitStartsWhenTheFirstStops(t *testing.T) {
	dir := daemonDir(t)
	env := func(k string) string { return map[string]string{"HEADROOM_CONFIG_DIR": dir}[k] }
	first := selfLink(t, filepath.Join(dir, "first", "headroom"))
	firstLog := filepath.Join(dir, "first.log")
	ctx1, stop1 := context.WithCancel(context.Background())
	t.Cleanup(stop1)
	done1 := make(chan int, 1)
	go func() {
		done1 <- Run(Env{Args: []string{first, "daemon", "--link-shims", "--log", firstLog},
			Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Getenv: env, Context: ctx1})
	}()
	waitFor(t, func() bool { b, _ := os.ReadFile(firstLog); return strings.Contains(string(b), "daemon started") })

	second := selfLink(t, filepath.Join(dir, "second", "headroom"))
	secondLog := filepath.Join(dir, "second.log")
	ctx2, stop2 := context.WithCancel(context.Background())
	t.Cleanup(stop2)
	done2 := make(chan int, 1)
	var errb syncBuffer
	go func() {
		done2 <- Run(Env{Args: []string{second, "daemon", "--wait", "--link-shims", "--log", secondLog},
			Stdout: &errb, Stderr: &errb, Getenv: env, Context: ctx2})
	}()
	read := func() string { b, _ := os.ReadFile(secondLog); return string(b) }
	waitFor(t, func() bool { return strings.Contains(read(), "waiting for it to stop") })
	time.Sleep(time.Second) // a few more tries, still waiting
	select {
	case code := <-done2:
		t.Fatalf("second daemon exited %d while the first runs: %s", code, errb.String())
	default:
	}
	if strings.Contains(read(), "daemon started") {
		t.Fatalf("second daemon started beside the first:\n%s", read())
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if target, err := os.Readlink(filepath.Join(dir, "shims", n)); err != nil || target != first {
			t.Errorf("while waiting: %s -> %q, %v; want %s", n, target, err, first)
		}
	}

	stop1()
	<-done1
	waitFor(t, func() bool { return strings.Contains(read(), "daemon started") })
	if n := strings.Count(read(), "waiting for it to stop"); n != 1 {
		t.Errorf("logged the wait %d times:\n%s", n, read())
	}
	for _, n := range []string{"docker", "podman", "tart"} {
		if target, err := os.Readlink(filepath.Join(dir, "shims", n)); err != nil || target != second {
			t.Errorf("after the first stopped: %s -> %q, %v; want %s", n, target, err, second)
		}
	}
	stop2()
	if code := <-done2; code != 0 {
		t.Fatalf("second daemon exit %d: %s", code, errb.String())
	}
}

// A daemon still waiting stops when told to.
func TestADaemonWaitingStopsOnSignal(t *testing.T) {
	dir := daemonDir(t)
	env := func(k string) string { return map[string]string{"HEADROOM_CONFIG_DIR": dir}[k] }
	firstLog := filepath.Join(dir, "first.log")
	ctx1, stop1 := context.WithCancel(context.Background())
	t.Cleanup(stop1)
	done1 := make(chan int, 1)
	go func() {
		done1 <- Run(Env{Args: []string{filepath.Join(dir, "headroom"), "daemon", "--log", firstLog},
			Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Getenv: env, Context: ctx1})
	}()
	t.Cleanup(func() { stop1(); <-done1 })
	waitFor(t, func() bool { b, _ := os.ReadFile(firstLog); return strings.Contains(string(b), "daemon started") })

	secondLog := filepath.Join(dir, "second.log")
	ctx2, stop2 := context.WithCancel(context.Background())
	done2 := make(chan int, 1)
	go func() {
		done2 <- Run(Env{Args: []string{filepath.Join(dir, "headroom"), "daemon", "--wait", "--log", secondLog},
			Stdout: &syncBuffer{}, Stderr: &syncBuffer{}, Getenv: env, Context: ctx2})
	}()
	waitFor(t, func() bool {
		b, _ := os.ReadFile(secondLog)
		return strings.Contains(string(b), "waiting for it to stop")
	})
	stop2()
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting daemon did not stop")
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
