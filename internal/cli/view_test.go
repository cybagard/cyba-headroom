package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

type hostReading struct{}

func (hostReading) Apply(s *protocol.Snapshot) {
	s.Host = &protocol.Host{TotalBytes: 64 << 30, Pressure: "normal"}
}

type hostSource struct{}

func (hostSource) Name() string { return "host" }
func (hostSource) Collect(context.Context) (daemon.Reading, error) {
	return hostReading{}, nil
}

// serveDaemon runs an in-process daemon with one fake source on the config
// dir's socket, ticked once. It returns the env pointing at it.
func serveDaemon(t *testing.T) (map[string]string, *daemon.Daemon) {
	t.Helper()
	return serveDaemonWith(t, nil)
}

// serveDaemonWith is serveDaemon with setup run on the daemon before it
// serves, as runDaemon sets its hooks.
func serveDaemonWith(t testing.TB, setup func(*daemon.Daemon)) (map[string]string, *daemon.Daemon) {
	t.Helper()
	return serveDaemonFrom(t, []daemon.Source{hostSource{}}, setup)
}

// serveDaemonFrom is serveDaemonWith with the given sources.
func serveDaemonFrom(t testing.TB, sources []daemon.Source, setup func(*daemon.Daemon)) (map[string]string, *daemon.Daemon) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d, err := daemon.New(sources, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(d)
	}
	d.Tick(context.Background())
	ln, err := daemon.Listen(dir + "/d.sock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = d.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return map[string]string{"HEADROOM_CONFIG_DIR": dir}, d
}

func TestViewRendersOnce(t *testing.T) {
	env, _ := serveDaemon(t)
	code, out, stderr := run(t, env, "headroom")
	if code != 0 || !strings.Contains(out, "total 64.0") || !strings.Contains(out, "worktrees unknown") {
		t.Fatalf("code=%d out=%q stderr=%q", code, out, stderr)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("escape codes off a terminal: %q", out)
	}
}

func TestViewDaemonDown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	code, _, stderr := run(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, "headroom", "--all")
	if code != 1 || !strings.Contains(stderr, "headroom daemon") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestViewRejectsUnknownFlag(t *testing.T) {
	code, _, stderr := run(t, nil, "headroom", "--watch", "--bogus")
	if code != 2 || !strings.Contains(stderr, "--bogus") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

// syncBuffer is a bytes.Buffer safe for a writer and a reader goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// watch runs `headroom --watch` until stop is closed, with a fast poll.
func watch(t *testing.T, env map[string]string, tty bool, stop <-chan struct{}) (*syncBuffer, <-chan int) {
	t.Helper()
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-stop; cancel() }()
	code := make(chan int, 1)
	go func() {
		code <- Run(Env{Args: []string{"headroom", "--watch"}, Stdout: &out, Stderr: io.Discard,
			Getenv: func(k string) string { return env[k] }, Context: ctx,
			Terminal: func() (bool, int) { return tty, 80 }, watchEvery: 10 * time.Millisecond})
	}()
	return &out, code
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatchRedrawsInPlaceOnATerminal(t *testing.T) {
	env, _ := serveDaemon(t)
	stop := make(chan struct{})
	out, code := watch(t, env, true, stop)
	waitFor(t, func() bool { return strings.Count(out.String(), clearScreen) >= 2 })
	close(stop)
	if c := <-code; c != 0 {
		t.Fatalf("exit %d", c)
	}
	s := out.String()
	if !strings.HasPrefix(s, enterScreen) || !strings.HasSuffix(s, leaveScreen) {
		t.Fatalf("terminal not set up and restored: %q ... %q", s[:min(len(s), 20)], s[max(0, len(s)-20):])
	}
	if !strings.Contains(s, "\x1b[32mnormal") {
		t.Error("no colour on a terminal")
	}
}

func TestWatchOffATerminalPrintsNewFramesOnly(t *testing.T) {
	env, d := serveDaemon(t)
	stop := make(chan struct{})
	out, code := watch(t, env, false, stop)
	waitFor(t, func() bool { return strings.Contains(out.String(), "total 64.0") })
	time.Sleep(50 * time.Millisecond) // several polls, no new snapshot
	if n := strings.Count(out.String(), "total 64.0"); n != 1 {
		t.Fatalf("%d frames before a new snapshot, want 1", n)
	}
	d.Tick(context.Background())
	waitFor(t, func() bool { return strings.Count(out.String(), "total 64.0") == 2 })
	close(stop)
	<-code
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("escape codes off a terminal")
	}
}

func TestWatchKeepsRetryingWhileTheDaemonIsDown(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	stop := make(chan struct{})
	out, code := watch(t, map[string]string{"HEADROOM_CONFIG_DIR": dir}, false, stop)
	waitFor(t, func() bool { return strings.Contains(out.String(), "not reachable") })
	time.Sleep(30 * time.Millisecond)
	close(stop)
	// Never reached the daemon: exit 1, as the one-shot view does.
	if c := <-code; c != 1 || strings.Count(out.String(), "not reachable") != 1 {
		t.Fatalf("exit %d, out %q: want one notice and exit 1", c, out.String())
	}
}

func TestTerminalOfUnknownSizeIsStillATerminal(t *testing.T) {
	// A pty without a size (script(1), some CI runners) reports 0 columns.
	if got := terminalWidth(0); got != 100 {
		t.Fatalf("width for 0 columns = %d, want the 100 fallback", got)
	}
	if got := terminalWidth(132); got != 132 {
		t.Fatalf("width = %d, want 132", got)
	}
}

func TestHelpListsViewFlags(t *testing.T) {
	_, out, _ := run(t, nil, "headroom", "help")
	for _, want := range []string{"--watch", "--all"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %s:\n%s", want, out)
		}
	}
}

func TestSuspendRestoresTheTerminal(t *testing.T) {
	env, _ := serveDaemon(t)
	var out syncBuffer
	suspend := make(chan os.Signal, 1)
	stopped := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() {
		code <- Run(Env{Args: []string{"headroom", "--watch"}, Stdout: &out, Stderr: io.Discard,
			Getenv: func(k string) string { return env[k] }, Context: ctx,
			Terminal:   func() (bool, int) { return true, 80 },
			watchEvery: 10 * time.Millisecond, suspend: suspend,
			stopSelf: func() { stopped <- struct{}{} }})
	}()
	waitFor(t, func() bool { return strings.Count(out.String(), clearScreen) >= 1 })
	suspend <- syscall.SIGTSTP
	<-stopped
	waitFor(t, func() bool { return strings.Count(out.String(), enterScreen) == 2 })
	cancel()
	<-code
	s := out.String()
	i, j := strings.Index(s, leaveScreen), strings.LastIndex(s, enterScreen)
	if i < 0 || j < i {
		t.Fatalf("want leave before the stop and enter after it: %q", s)
	}
}
