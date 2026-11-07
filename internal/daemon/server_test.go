package daemon_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
func serve(t *testing.T, path string) *daemon.Daemon { return serveWith(t, path, nil) }

// serveWith is serve with setup run before the daemon serves.
func serveWith(t *testing.T, path string, setup func(*daemon.Daemon)) *daemon.Daemon {
	t.Helper()
	d, err := daemon.New(nil, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(d)
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

func TestCheckRoundTrip(t *testing.T) {
	path := sockPath(t)
	var got *protocol.CheckRequest
	serveWith(t, path, func(d *daemon.Daemon) {
		d.SetCheck(func(r *protocol.CheckRequest, _ *protocol.Snapshot) protocol.Decision {
			got = r
			return protocol.Decision{Allow: false, Retry: true, Message: "no room", Reasons: []protocol.Reason{{Code: "headroom", Retry: true}}}
		})
	})
	rep, err := client.Do(context.Background(), path, time.Second, protocol.Request{Op: protocol.OpCheck,
		Check: &protocol.CheckRequest{Worktree: "w1", Kind: "container", Command: "docker run x", CostBytes: 1 << 30}})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != "w1" || got.CostBytes != 1<<30 {
		t.Fatalf("check saw %+v", got)
	}
	if rep.Decision == nil || rep.Decision.Allow || rep.Decision.Message != "no room" || !rep.Decision.Retry {
		t.Fatalf("reply %+v", rep.Decision)
	}
}

func TestCheckWithoutAChecker(t *testing.T) {
	path := sockPath(t)
	serve(t, path)
	_, err := client.Do(context.Background(), path, time.Second, protocol.Request{Op: protocol.OpCheck, Check: &protocol.CheckRequest{}})
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("err = %v", err)
	}
}

// The daemon that takes the lock writes its PID into the lock file,
// replacing what an earlier holder wrote, so a daemon refused can name the
// holder (#194).
func TestListenRecordsTheHoldersPID(t *testing.T) {
	path := sockPath(t)
	if err := os.WriteFile(path+".lock", []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := daemon.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if b, err := os.ReadFile(path + ".lock"); err != nil || string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock holds %q, %v; want this PID", b, err)
	}
	if pid, err := daemon.LockHolder(path); err != nil || pid != os.Getpid() {
		t.Fatalf("LockHolder = %d, %v; want %d", pid, err, os.Getpid())
	}
}

// undone records what the daemon undid after a check's reply.
type undone struct {
	mu                sync.Mutex
	released, dropped []string
	done              chan struct{}
}

func (u *undone) got() (released, dropped []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.released, u.dropped
}

// serveUndoing serves a daemon whose checks answer dec, each once its
// client is ready (ready is closed), and records the leases it releases
// and the tickets it drops.
func serveUndoing(t *testing.T, path string, dec protocol.Decision, ready <-chan struct{}) *undone {
	t.Helper()
	u := &undone{done: make(chan struct{}, 2)}
	serveWith(t, path, func(d *daemon.Daemon) {
		d.SetCheck(func(*protocol.CheckRequest, *protocol.Snapshot) protocol.Decision {
			<-ready
			return dec
		})
		d.SetRelease(func(id string) bool {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.released = append(u.released, id)
			u.done <- struct{}{}
			return true
		})
		d.SetDropTicket(func(ticket string) bool {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.dropped = append(u.dropped, ticket)
			u.done <- struct{}{}
			return true
		})
	})
	return u
}

// loseReply sends check and closes the connection before reading its
// reply, then lets the slow check answer: the daemon's write fails.
func loseReply(t *testing.T, path, check string, ready chan<- struct{}) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(c, `{"v":`+strconv.Itoa(protocol.Version)+`,"op":"check","check":`+check+`}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	close(ready)
}

func waitUndone(t *testing.T, u *undone) {
	t.Helper()
	select {
	case <-u.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon undid nothing")
	}
}

// A labelled check whose client closed before reading its reply was
// never gated: the shim ran it without the lease's label, so its lease is
// released, and holds no cost to the timeout (#271).
func TestALostReplyReleasesItsLease(t *testing.T) {
	path := sockPath(t)
	ready := make(chan struct{})
	u := serveUndoing(t, path, protocol.Decision{Allow: true, LeaseID: "lease-a-1"}, ready)
	loseReply(t, path, `{"worktree":"w1","command":"docker run x","labelled":true}`, ready)
	waitUndone(t, u)
	if released, dropped := u.got(); !slices.Equal(released, []string{"lease-a-1"}) || len(dropped) != 0 {
		t.Fatalf("released %v, dropped %v; want lease-a-1 released", released, dropped)
	}
}

// A waiting call's lost reply drops its ticket, so the calls behind it
// move up now, not after TicketTTL (#271).
func TestALostReplyDropsItsTicket(t *testing.T) {
	path := sockPath(t)
	ready := make(chan struct{})
	u := serveUndoing(t, path, protocol.Decision{Retry: true, Ticket: "ticket-a-1"}, ready)
	loseReply(t, path, `{"worktree":"w1","command":"docker run x","labelled":true,"wait":true}`, ready)
	waitUndone(t, u)
	if released, dropped := u.got(); len(released) != 0 || !slices.Equal(dropped, []string{"ticket-a-1"}) {
		t.Fatalf("released %v, dropped %v; want ticket-a-1 dropped", released, dropped)
	}
}

// An unlabelled check's lost reply keeps its lease: it binds by name what
// the shim's fail-open call starts (a compose up's stack), so it still
// holds that room (#271, plan amendment 1).
func TestALostReplyKeepsALeaseThatBindsByName(t *testing.T) {
	path := sockPath(t)
	ready := make(chan struct{})
	var u *undone
	// Registered before serveUndoing's, so it runs after the daemon has
	// stopped and the lost reply has been handled.
	t.Cleanup(func() {
		if u == nil {
			return
		}
		if released, dropped := u.got(); len(released) != 0 || len(dropped) != 0 {
			t.Errorf("released %v, dropped %v; want the lease kept", released, dropped)
		}
	})
	u = serveUndoing(t, path, protocol.Decision{Allow: true, LeaseID: "lease-a-1"}, ready)
	loseReply(t, path, `{"worktree":"w1","kind":"compose","op":"up","command":"docker compose up","target":"app"}`, ready)
}

// A delivered reply undoes nothing.
func TestADeliveredReplyUndoesNothing(t *testing.T) {
	path := sockPath(t)
	ready := make(chan struct{})
	close(ready)
	var u *undone
	// Registered before serveUndoing's, so it runs after the daemon has
	// stopped and every reply has been written.
	t.Cleanup(func() {
		if u == nil {
			return
		}
		if released, dropped := u.got(); len(released) != 0 || len(dropped) != 0 {
			t.Errorf("released %v, dropped %v; want nothing undone", released, dropped)
		}
	})
	u = serveUndoing(t, path, protocol.Decision{Allow: true, LeaseID: "lease-a-1", Ticket: "ticket-a-1"}, ready)
	rep, err := client.Do(context.Background(), path, time.Second, protocol.Request{Op: protocol.OpCheck,
		Check: &protocol.CheckRequest{Worktree: "w1", Command: "docker run x"}})
	if err != nil || rep.Decision == nil || rep.Decision.LeaseID != "lease-a-1" {
		t.Fatalf("reply %+v, %v", rep, err)
	}
}
