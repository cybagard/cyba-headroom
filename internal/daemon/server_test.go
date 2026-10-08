package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/client"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/protocol"
)

// sockPath returns a socket path short enough for macOS's 104-byte sun_path;
// t.TempDir() under /var/folders is too long.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// serve starts a daemon with no sources on a fresh socket and stops it at cleanup.
func serve(t *testing.T, path string) *daemon.Daemon {
	t.Helper()
	d, err := daemon.New(nil, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := daemon.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return d
}

func TestStatusRoundTrip(t *testing.T) {
	path := sockPath(t)
	d := serve(t, path)
	d.Tick(context.Background())

	snap, err := client.Status(context.Background(), path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Seq != 1 || snap.CollectedAt.IsZero() {
		t.Fatalf("snapshot = %+v", snap)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestPingAndBadRequests(t *testing.T) {
	path := sockPath(t)
	serve(t, path)
	ctx := context.Background()

	if _, err := client.Do(ctx, path, time.Second, protocol.Request{Op: protocol.OpPing}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := client.Do(ctx, path, time.Second, protocol.Request{Op: "launch"}); err == nil || !strings.Contains(err.Error(), `unknown op "launch"`) {
		t.Fatalf("unknown op err = %v", err)
	}

	// Wrong version, sent raw because client.Do always sets the current one.
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := io.WriteString(c, `{"v":99,"op":"ping"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "unsupported protocol version 99") {
		t.Fatalf("reply = %s", b)
	}
}

func TestConcurrentStatusDuringTicks(t *testing.T) {
	path := sockPath(t)
	d := serve(t, path)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 20 {
				d.Tick(ctx)
			}
		})
	}
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, err := client.Status(ctx, path, time.Second); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := d.Snapshot().Seq; got != 80 {
		t.Fatalf("seq = %d, want 80", got)
	}
}

func TestListenRefusesLiveDaemon(t *testing.T) {
	path := sockPath(t)
	serve(t, path)
	if _, err := daemon.Listen(path); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
}

func TestListenRefusesWhileDaemonHoldsLock(t *testing.T) {
	path := sockPath(t)
	ln, err := daemon.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Even with the live daemon's socket file gone (or its dial timing out),
	// a second daemon must not start.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ln2, err := daemon.Listen(path); err == nil {
		_ = ln2.Close()
		t.Fatal("second Listen succeeded while the first daemon is running")
	}

	// Once the first daemon stops, the lock is free again.
	_ = ln.Close()
	ln3, err := daemon.Listen(path)
	if err != nil {
		t.Fatalf("Listen after the first daemon stopped: %v", err)
	}
	_ = ln3.Close()
}

// flakyListener fails its first Accept with EMFILE, as under fd exhaustion.
type flakyListener struct {
	net.Listener
	failed bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, &net.OpError{Op: "accept", Net: "unix", Err: syscall.EMFILE}
	}
	return l.Listener.Accept()
}

func TestServeSurvivesTemporaryAcceptError(t *testing.T) {
	path := sockPath(t)
	d, err := daemon.New(nil, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := daemon.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, &flakyListener{Listener: ln}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	if _, err := client.Do(context.Background(), path, 2*time.Second, protocol.Request{Op: protocol.OpPing}); err != nil {
		t.Fatalf("ping after EMFILE: %v", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	path := sockPath(t)
	// Leave a socket file behind with nothing listening, as after a crash.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}

	serve(t, path)
	if _, err := client.Do(context.Background(), path, time.Second, protocol.Request{Op: protocol.OpPing}); err != nil {
		t.Fatal(err)
	}
}

func TestListenRefusesNonSocket(t *testing.T) {
	path := sockPath(t)
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Listen(path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("err = %v", err)
	}
}

func TestShutdownRemovesSocket(t *testing.T) {
	path := sockPath(t)
	d, err := daemon.New(nil, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := daemon.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, ln) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket still present after shutdown: %v", err)
	}
}

func TestClientFailsFastWithoutDaemon(t *testing.T) {
	start := time.Now()
	if _, err := client.Status(context.Background(), sockPath(t), 500*time.Millisecond); err == nil {
		t.Fatal("want error with no daemon")
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("took %s", el)
	}
}

func TestPingReportsThePID(t *testing.T) {
	path := sockPath(t)
	serve(t, path)
	pid, err := client.Ping(context.Background(), path, time.Second)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid = %d, %v; want %d", pid, err, os.Getpid())
	}
}
