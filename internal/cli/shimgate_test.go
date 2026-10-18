package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
	"github.com/cybagard/cyba-headroom/internal/lease"
	"github.com/cybagard/cyba-headroom/internal/policy"
	"github.com/cybagard/cyba-headroom/internal/protocol"
	"github.com/cybagard/cyba-headroom/internal/shim"
)

// shimRig runs the shim against a fake daemon (ask) and records what it
// would exec.
type shimRig struct {
	t        testing.TB
	dir      string // PATH dir with fake docker and tart, and the config dir
	env      []string
	ask      func(protocol.CheckRequest) (*protocol.Decision, error)
	asked    []protocol.CheckRequest
	execed   string
	execArgv []string
	execEnv  []string
	sleeps   int
	now      time.Time
	sleepFor time.Duration // how far the clock moves per wait
	stop     bool          // the wait is interrupted
	execErr  error         // what exec returns
	released []string
	raised   os.Signal
	pending  os.Signal // a signal that came during an ask
	// composeAsk stands in for docker compose config.
	composeAsk func(string, []string) ([]byte, error)
	// composeDry stands in for docker compose --dry-run.
	composeDry func(string, []string) ([]byte, error)
	// getwd stands in for os.Getwd when set.
	getwd func() (string, error)
	// machineAsk stands in for podman machine inspect.
	machineAsk func(string, []string) ([]byte, error)
}

func newShimRig(t testing.TB) *shimRig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, n := range []string{"docker", "podman", "tart"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!real\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &shimRig{t: t, dir: dir, env: []string{"PATH=" + dir, "HEADROOM_CONFIG_DIR=" + dir}, now: time.Unix(1e9, 0), sleepFor: 2 * time.Second}
}

// fakeSSH puts an ssh first on the rig's PATH whose "ssh -G" names
// 203.0.113.9 for the remote fixtures build-box and build.example, and any
// other host as itself, so the shim reads a remote ssh host without the real
// ssh, its config or DNS (#256).
func (r *shimRig) fakeSSH() {
	r.t.Helper()
	script := "#!/bin/sh\nfor a; do h=$a; done\ncase $h in build-box|build.example) h=203.0.113.9 ;; esac\necho \"hostname $h\"\n"
	if err := os.WriteFile(filepath.Join(r.dir, "ssh"), []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *shimRig) run(argv ...string) (code int, stderr string) {
	var errb strings.Builder
	var mu sync.Mutex
	code = Run(Env{Args: argv, Stdout: io.Discard, Stderr: &errb,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return r.env },
		exec: func(path string, argv, env []string) error {
			r.execed, r.execArgv, r.execEnv = path, argv, env
			return r.execErr
		},
		release: func(_ config.Config, id string) error {
			r.released = append(r.released, id)
			return nil
		},
		ask: func(_ config.Config, req protocol.CheckRequest) (*protocol.Decision, error) {
			mu.Lock()
			defer mu.Unlock()
			r.asked = append(r.asked, req)
			return r.ask(req)
		},
		ancestors: func() []int { return []int{4321, 1} },
		getwd:     r.getwd,
		now:       func() time.Time { return r.now },
		wait: func(d time.Duration) os.Signal {
			if d == 0 { // is a signal pending?
				return r.pending
			}
			r.sleeps++
			r.now = r.now.Add(r.sleepFor)
			if r.stop {
				return syscall.SIGINT
			}
			return nil
		},
		raise: func(s os.Signal) { r.raised = s },
		composeAsk: func(bin string, args []string) ([]byte, error) {
			if r.composeAsk != nil {
				return r.composeAsk(bin, args)
			}
			return nil, errors.New("no compose here")
		},
		composeDry: func(bin string, args []string) ([]byte, error) {
			if r.composeDry != nil {
				return r.composeDry(bin, args)
			}
			return nil, errors.New("no compose here")
		},
		machineAsk: func(bin string, args []string) ([]byte, error) {
			if r.machineAsk != nil {
				return r.machineAsk(bin, args)
			}
			return nil, errors.New("no podman machine here")
		},
	})
	return code, errb.String()
}

func allow(protocol.CheckRequest) (*protocol.Decision, error) {
	return &protocol.Decision{Allow: true, LeaseID: "lease-7", Message: "headroom: allowed"}, nil
}

func TestShimGateAllows(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=repo::/Users/dev/src/project-a")
	r.ask = allow
	code, stderr := r.run("docker", "run", "--rm", "-m", "2g", "-e", "TOKEN=x", "alpine", "true")
	if code != 0 || r.execed != filepath.Join(r.dir, "docker") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
	// The environment names the worktree: no cwd or ancestry needed.
	want := protocol.CheckRequest{Worktree: "repo::/Users/dev/src/project-a", Kind: "container", Command: "docker run alpine",
		CostBytes: 2 << 30}
	if len(r.asked) != 1 || !equalRequest(r.asked[0], want) {
		t.Fatalf("asked %+v, want %+v", r.asked, want)
	}
	// The marker is this process's PID, which the real binary keeps: only
	// a binary it execs in its place skips the check, not its children.
	if !slices.Contains(r.execEnv, "HEADROOM_SHIM_CHECKED="+shim.Self()) {
		t.Fatalf("child env lacks the checked marker: %q", r.execEnv)
	}
}

func TestShimGateSendsCwdAndAncestry(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	realDir := filepath.Join(r.dir, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(r.dir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	r.run("docker", "run", "alpine")
	got := r.asked[0]
	resolved, _ := filepath.EvalSymlinks(realDir)
	if !slices.Equal(got.Ancestors, []int{4321, 1}) || got.Cwd == "" || (got.Cwd != resolved && got.RealCwd != resolved) {
		t.Fatalf("asked %+v, want ancestry and the cwd resolved to %s", got, resolved)
	}
}

func TestShimGateReleasesTheLeaseIfExecFails(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	r.execErr = syscall.ENOENT
	if code, _ := r.run("docker", "run", "alpine"); code != 127 || !slices.Equal(r.released, []string{"lease-7"}) {
		t.Fatalf("exit %d, released %q", code, r.released)
	}
}

func equalRequest(a, b protocol.CheckRequest) bool {
	return a.Worktree == b.Worktree && a.Kind == b.Kind && a.Command == b.Command && a.CostBytes == b.CostBytes &&
		a.Cwd == b.Cwd && a.RealCwd == b.RealCwd && slices.Equal(a.Ancestors, b.Ancestors)
}

func TestShimGateHeadroomWorktreeOverridesOrca(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=orca-id", "HEADROOM_WORKTREE=override")
	r.ask = allow
	r.run("tart", "run", "ci-vm")
	if len(r.asked) != 1 || r.asked[0].Worktree != "override" || r.asked[0].Kind != "tart" || r.asked[0].Command != "tart run ci-vm" {
		t.Fatalf("asked %+v", r.asked)
	}
}

func TestShimGateDenies(t *testing.T) {
	r := newShimRig(t)
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "headroom: not starting `docker run alpine` (≈ 1.0 GB): only 0.5 GB headroom"}, nil
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != exitDenied || r.execed != "" || !strings.Contains(stderr, "only 0.5 GB headroom") || !strings.Contains(stderr, "BUDGET_WAIT=1") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
	// A deny waiting cannot fix says so.
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Message: "headroom: not starting it: over its cap"}, nil
	}
	_, stderr = r.run("docker", "run", "alpine")
	if strings.Contains(stderr, "BUDGET_WAIT") || !strings.Contains(stderr, "waiting will not help") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestShimGateDoesNotAsk(t *testing.T) {
	for name, c := range map[string]struct {
		argv []string
		env  []string
	}{
		"a call that starts nothing": {[]string{"docker", "ps"}, nil},
		"already checked":            {[]string{"docker", "run", "alpine"}, []string{"HEADROOM_SHIM_CHECKED=" + shim.Self()}},
		"a remote engine":            {[]string{"docker", "run", "alpine"}, []string{"DOCKER_HOST=ssh://dev@build-box"}},
		"a remote engine by flag":    {[]string{"docker", "-H", "tcp://10.0.0.5:2376", "run", "alpine"}, nil},
	} {
		r := newShimRig(t)
		r.fakeSSH()
		r.env = append(r.env, c.env...)
		r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
			t.Errorf("%s: asked the daemon", name)
			return nil, errors.New("no")
		}
		if code, _ := r.run(c.argv...); code != 0 || r.execed == "" {
			t.Errorf("%s: exit %d, exec %q", name, code, r.execed)
		}
	}
}

func TestShimGateChecksAChildOfACheckedCall(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "HEADROOM_SHIM_CHECKED=1") // another process's marker
	r.ask = allow
	if r.run("docker", "run", "alpine"); len(r.asked) != 1 {
		t.Fatalf("asked %d times", len(r.asked))
	}
}

func TestShimGateOldDaemon(t *testing.T) {
	r := newShimRig(t)
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return nil, daemonError{said: `unknown op "check"`}
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || !strings.Contains(stderr, "headroom install") || strings.Contains(stderr, "not reachable") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
}

func TestShimGateFailsOpen(t *testing.T) {
	r := newShimRig(t)
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) { return nil, errors.New("connection refused") }
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || !strings.Contains(stderr, "not gated") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
}

func TestShimGateFailsOpenOnBadConfig(t *testing.T) {
	r := newShimRig(t)
	if err := os.WriteFile(filepath.Join(r.dir, "config.toml"), []byte("[policy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.ask = allow
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || len(r.asked) != 0 || !strings.Contains(stderr, "not gated") {
		t.Fatalf("exit %d, exec %q, asked %d, stderr %q", code, r.execed, len(r.asked), stderr)
	}
}

func TestShimGateWaits(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	n := 0
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		if n++; n < 3 {
			return &protocol.Decision{Retry: true, Message: "headroom: not starting it: only 0.5 GB headroom",
				Reasons: []protocol.Reason{{Code: "headroom", Text: "only 0.5 GB headroom", Retry: true}}}, nil
		}
		return allow(protocol.CheckRequest{})
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || len(r.asked) != 3 || r.sleeps != 2 {
		t.Fatalf("exit %d, exec %q, asked %d, slept %d, stderr %q", code, r.execed, len(r.asked), r.sleeps, stderr)
	}
	if strings.Count(stderr, "waiting") != 1 || !strings.Contains(stderr, "only 0.5 GB headroom") {
		t.Fatalf("want one waiting line, stderr %q", stderr)
	}
}

// A waiting call asks to wait, and carries back the ticket each deny gave
// it, a new one replacing the last (#72). Against a daemon that gives
// none, it still waits by asking again.
func TestShimGateWaitsWithItsTicket(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	tickets := []string{"ticket-a-1", "ticket-a-1", "ticket-b-1", ""}
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		if n := len(r.asked); n <= len(tickets) {
			return &protocol.Decision{Retry: true, Ticket: tickets[n-1], Message: "headroom: not starting it: only 0.5 GB headroom",
				Reasons: []protocol.Reason{{Code: "headroom", Text: "only 0.5 GB headroom", Retry: true}}}, nil
		}
		return allow(protocol.CheckRequest{})
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != 0 || r.execed == "" || len(r.asked) != 5 {
		t.Fatalf("exit %d, exec %q, asked %d, stderr %q", code, r.execed, len(r.asked), stderr)
	}
	var sent []string
	for _, a := range r.asked {
		if !a.Wait {
			t.Fatalf("an ask without Wait: %+v", a)
		}
		sent = append(sent, a.Ticket)
	}
	if want := []string{"", "ticket-a-1", "ticket-a-1", "ticket-b-1", ""}; !slices.Equal(sent, want) {
		t.Fatalf("tickets sent %q, want %q", sent, want)
	}
	// Without BUDGET_WAIT it does not ask to wait.
	r.asked, r.env = nil, slices.DeleteFunc(r.env, func(e string) bool { return e == "BUDGET_WAIT=1" })
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "no"}, nil
	}
	if code, _ := r.run("docker", "run", "alpine"); code != exitDenied || len(r.asked) != 1 || r.asked[0].Wait || r.asked[0].Ticket != "" {
		t.Fatalf("exit %d, asked %+v", code, r.asked)
	}
}

func TestShimGateWaitsOnlyWhenItHelps(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Message: "headroom: not starting it: over its cap"}, nil
	}
	if code, _ := r.run("docker", "run", "alpine"); code != exitDenied || len(r.asked) != 1 || r.sleeps != 0 {
		t.Fatalf("exit %d, asked %d, slept %d", code, len(r.asked), r.sleeps)
	}
}

func TestShimGateWaitGivesUp(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	r.sleepFor = 4 * time.Minute // the 10-minute default runs out on the third wait
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "headroom: not starting it: only 0.5 GB headroom"}, nil
	}
	code, stderr := r.run("docker", "run", "alpine")
	if code != exitDenied || r.execed != "" || !strings.Contains(stderr, "gave up") || !strings.Contains(stderr, "only 0.5 GB headroom") {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execed, stderr)
	}
}

func TestShimGateWaitInterrupted(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	r.stop = true
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Retry: true, Message: "headroom: not starting it"}, nil
	}
	// It dies of the signal, so a shell loop around it stops too.
	if code, _ := r.run("docker", "run", "alpine"); code != 130 || r.execed != "" || r.raised != syscall.SIGINT {
		t.Fatalf("exit %d, exec %q, raised %v", code, r.execed, r.raised)
	}
}

// End to end against the daemon's own wiring: of two calls that do not both
// fit, one runs and the other is denied (R5, R10).
func TestShimGateAgainstTheDaemon(t *testing.T) {
	envMap, _ := serveDaemonWith(t, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), discardLog(), nil)
	})
	r := newShimRig(t)
	r.env = []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			code := Run(Env{Args: []string{"docker", "run", "-m", "40g", "alpine"}, Stdout: io.Discard, Stderr: io.Discard,
				Getenv: func(string) string { return "" }, Environ: func() []string { return r.env },
				exec: func(string, []string, []string) error { return nil }})
			codes <- code
		})
	}
	wg.Wait()
	close(codes)
	var got []int
	for c := range codes {
		got = append(got, c)
	}
	slices.Sort(got)
	if !slices.Equal(got, []int{0, exitDenied}) {
		t.Fatalf("exit codes %v, want one run and one deny", got)
	}
}

// roomSource is a host whose memory the test sets: growing it makes room.
type roomSource struct {
	mu    sync.Mutex
	total uint64
}

func (*roomSource) Name() string { return "host" }
func (h *roomSource) Collect(context.Context) (daemon.Reading, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return roomReading(h.total), nil
}

type roomReading uint64

func (r roomReading) Apply(s *protocol.Snapshot) {
	s.Host = &protocol.Host{TotalBytes: uint64(r), Pressure: "normal"}
}

// End to end against the daemon's own wiring: waiting calls are allowed
// first come first (#72). A (8 GB) waits, then B and C (1 GB each), with
// room for B alone; a call that does not wait may not take A's room. Room
// grows in steps, and each time the later calls ask first: A runs, then
// B, then C. status --json lists the queue.
func TestShimGateWaitersRunInArrivalOrder(t *testing.T) {
	host := &roomSource{total: 64 << 30}
	cfg := config.Defaults("/x")
	envMap, d := serveDaemonFrom(t, []daemon.Source{host}, func(d *daemon.Daemon) { wireGate(d, cfg, discardLog(), nil) })
	minFree := int64(cfg.PolicyConfig().MinHeadroomBytes)
	// free sets the room above min_headroom to f.
	free := func(f int64) {
		h := *d.Snapshot().Budget.HeadroomBytes
		host.mu.Lock()
		host.total = uint64(int64(host.total) + f - (h - minFree))
		host.mu.Unlock()
		d.Tick(context.Background())
		if got := *d.Snapshot().Budget.HeadroomBytes - minFree; got != f {
			t.Fatalf("free %d, want %d", got, f)
		}
	}
	const gb = int64(1 << 30)
	free(gb)
	r := newShimRig(t)
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w", "BUDGET_WAIT=1"}
	var mu sync.Mutex
	var ran []string
	type waiter struct {
		polled, again chan struct{}
		done          chan int
	}
	start := func(name, mem string) *waiter {
		w := &waiter{polled: make(chan struct{}), again: make(chan struct{}), done: make(chan int, 1)}
		go func() {
			w.done <- Run(Env{Args: []string{"docker", "run", "-m", mem, "alpine"}, Stdout: io.Discard, Stderr: io.Discard,
				Getenv: func(string) string { return "" }, Environ: func() []string { return env },
				exec: func(string, []string, []string) error {
					mu.Lock()
					ran = append(ran, name)
					mu.Unlock()
					return nil
				},
				wait: func(dur time.Duration) os.Signal {
					if dur > 0 {
						w.polled <- struct{}{}
						<-w.again
					}
					return nil
				}})
		}()
		return w
	}
	// asked waits for w's ask: whether it was allowed.
	asked := func(w *waiter) bool {
		select {
		case <-w.polled:
			return false
		case code := <-w.done:
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			return true
		case <-time.After(10 * time.Second):
			t.Fatal("no ask")
		}
		return false
	}
	// asks has w ask again.
	asks := func(w *waiter) bool {
		w.again <- struct{}{}
		return asked(w)
	}
	a := start("A", "8g")
	var b, c *waiter
	if asked(a) {
		t.Fatal("A ran with 1 GB of room")
	}
	if b = start("B", "1g"); asked(b) {
		t.Fatal("B took A's room")
	}
	if c = start("C", "1g"); asked(c) {
		t.Fatal("C took A's room")
	}
	d.Tick(context.Background())
	if q := d.Snapshot().Waiting; len(q) != 3 || q[0].Bytes != 8<<30 || q[1].Bytes != 1<<30 {
		t.Fatalf("queue %+v", q)
	}
	code, out, _ := run(t, envMap, "headroom", "status", "--json")
	if code != 0 || strings.Count(out, `"ticket"`) != 3 {
		t.Fatalf("status --json: exit %d, %s", code, out)
	}
	var stderr strings.Builder
	plain := Run(Env{Args: []string{"docker", "run", "-m", "1g", "alpine"}, Stdout: io.Discard, Stderr: &stderr,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env[:3] },
		exec: func(string, []string, []string) error { return nil }})
	if plain != exitDenied || !strings.Contains(stderr.String(), "held for 3 waiting calls ahead") || !strings.Contains(stderr.String(), "BUDGET_WAIT=1") {
		t.Fatalf("a call that does not wait: exit %d, stderr %q", plain, stderr.String())
	}
	free(8*gb + gb/2) // A's room, not B's beside it
	if asks(c) || asks(b) || !asks(a) {
		t.Fatalf("with room for A alone, ran %q", ran)
	}
	free(9*gb + gb/2) // and B's
	if asks(c) || !asks(b) {
		t.Fatalf("with room for A and B, ran %q", ran)
	}
	free(10*gb + gb/2)
	if !asks(c) {
		t.Fatalf("with room for all, ran %q", ran)
	}
	if !slices.Equal(ran, []string{"A", "B", "C"}) {
		t.Fatalf("ran %q, want A, B, C", ran)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestShimGateDebugNamesTheWorktree(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "HEADROOM_SHIM_DEBUG=1")
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		return &protocol.Decision{Allow: true, Worktree: "repo::/Users/dev/src/project-a", IdentifiedBy: protocol.IdentifiedByProcess}, nil
	}
	_, stderr := r.run("docker", "run", "alpine")
	if !strings.Contains(stderr, "headroom: allowed for repo::/Users/dev/src/project-a (by process)") {
		t.Fatalf("stderr %q", stderr)
	}
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) { return &protocol.Decision{Allow: true}, nil }
	if _, stderr = r.run("docker", "run", "alpine"); !strings.Contains(stderr, "headroom: allowed as a manual call") {
		t.Fatalf("stderr %q", stderr)
	}
}

// A call whose exec fails hands its lease back to the daemon.
func TestShimGateReleaseAgainstTheDaemon(t *testing.T) {
	envMap, d := serveDaemonWith(t, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), discardLog(), nil)
	})
	r := newShimRig(t)
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	code := Run(Env{Args: []string{"docker", "run", "-m", "40g", "alpine"}, Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env },
		exec: func(string, []string, []string) error { return syscall.ENOENT }})
	d.Tick(context.Background())
	if s := d.Snapshot(); code != 127 || len(s.Leases) != 0 {
		t.Fatalf("exit %d, leases %+v", code, s.Leases)
	}
}

// A Ctrl-C during the ask that finds room still cancels the call.
func TestShimGateWaitCancelledDuringTheAsk(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	n := 0
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		if n++; n == 1 {
			return &protocol.Decision{Retry: true, Message: "headroom: not starting it"}, nil
		}
		r.pending = syscall.SIGINT
		return allow(protocol.CheckRequest{})
	}
	code, _ := r.run("docker", "run", "alpine")
	if code != 130 || r.execed != "" || r.raised != syscall.SIGINT || !slices.Equal(r.released, []string{"lease-7"}) {
		t.Fatalf("exit %d, exec %q, raised %v, released %q", code, r.execed, r.raised, r.released)
	}
}

// tart run asks with the VM's own memory and whether it takes a macOS
// slot; tart clone runs nothing and is not asked about (#29).
func TestShimGateTartVMs(t *testing.T) {
	r := newShimRig(t)
	tartHome := filepath.Join(r.dir, "tarthome")
	if err := os.MkdirAll(filepath.Join(tartHome, "vms", "mac-ci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tartHome, "vms", "mac-ci", "config.json"), []byte(`{"os":"darwin","memorySize":17179869184}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r.env = append(r.env, "TART_HOME="+tartHome)
	r.ask = allow
	r.run("tart", "run", "--no-graphics", "mac-ci")
	if len(r.asked) != 1 || !r.asked[0].MacOS || r.asked[0].CostBytes != 16<<30 || r.asked[0].Kind != "tart" {
		t.Fatalf("asked %+v", r.asked)
	}
	r.env = append(r.env, "HEADROOM_SHIM_DEBUG=1")
	_, stderr := r.run("tart", "run", "not-here")
	if len(r.asked) != 2 || !r.asked[1].MacOS || !r.asked[1].VMUnknown || r.asked[1].CostBytes != 0 || !strings.Contains(stderr, "no config for the VM in `tart run not-here`") {
		t.Fatalf("unknown VM asked %+v, stderr %q", r.asked[1], stderr)
	}
	if r.asked[0].PID != os.Getpid() {
		t.Fatalf("a tart run must name its process, which becomes tart's: %+v", r.asked[0])
	}
	if code, _ := r.run("tart", "clone", "mac-ci", "mac-ci-2"); code != 0 || len(r.asked) != 2 || r.execed == "" {
		t.Fatalf("clone: exit %d, asked %d", code, len(r.asked))
	}
}

// podman machine start asks with the machine's memory, from podman machine
// inspect (a recorded capture), and names the machine: the default one's
// name when the call gives none. A machine is this Mac's whatever
// connection the call names. When inspect fails, the start is not gated
// (R7) (#230).
func TestShimGatePodmanMachineStart(t *testing.T) {
	r := newShimRig(t)
	inspect, err := os.ReadFile(filepath.Join("testdata", "podman-machine-inspect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var inspected [][]string
	r.machineAsk = func(bin string, args []string) ([]byte, error) {
		if bin != filepath.Join(r.dir, "podman") {
			t.Errorf("inspect ran %q, not the real podman", bin)
		}
		inspected = append(inspected, args)
		return inspect, nil
	}
	r.env = append(r.env, "HEADROOM_WORKTREE=w")
	r.ask = allow
	if code, stderr := r.run("podman", "machine", "start"); code != 0 || !slices.Equal(r.execArgv, []string{"podman", "machine", "start"}) {
		t.Fatalf("exit %d, exec %q, stderr %q", code, r.execArgv, stderr)
	}
	want := protocol.CheckRequest{Worktree: "w", Kind: "machine", Op: "start", Command: "podman machine start", CostBytes: 2 << 30, Target: "podman-machine-default"}
	if len(r.asked) != 1 || !equalRequest(r.asked[0], want) || r.asked[0].Op != want.Op || r.asked[0].Target != want.Target || r.asked[0].PID != 0 {
		t.Fatalf("asked %+v, want %+v", r.asked, want)
	}
	if !slices.Equal(inspected[0], []string{"machine", "inspect"}) {
		t.Fatalf("inspected %q", inspected)
	}
	if code, _ := r.run("podman", "--connection", "remote", "machine", "start", "-q", "podman-machine-default"); code != 0 || len(r.asked) != 2 ||
		!slices.Equal(inspected[1], []string{"machine", "inspect", "podman-machine-default"}) {
		t.Fatalf("exit %d, asked %d, inspected %q", code, len(r.asked), inspected)
	}
	// Past a flag the parser does not know, the machine is a guess: not
	// gated, rather than sized and keyed as the default machine (plan
	// amendment 4).
	r.execed = ""
	code, stderr := r.run("podman", "machine", "start", "--log-level", "debug", "big")
	if code != 0 || r.execed == "" || len(r.asked) != 2 || len(inspected) != 2 ||
		!strings.Contains(stderr, "headroom: not gated (podman machine start: an option the shim does not know); running `podman machine start` anyway") {
		t.Fatalf("guessed: exit %d, exec %q, asked %d, inspected %q, stderr %q", code, r.execed, len(r.asked), inspected, stderr)
	}
	r.machineAsk = func(string, []string) ([]byte, error) { return nil, errors.New("exit status 125") }
	r.execed = ""
	code, stderr = r.run("podman", "machine", "start", "gone")
	if code != 0 || r.execed == "" || len(r.asked) != 2 || !strings.Contains(stderr, "headroom: not gated (podman machine inspect: exit status 125); running `podman machine start gone` anyway") {
		t.Fatalf("inspect failed: exit %d, exec %q, asked %d, stderr %q", code, r.execed, len(r.asked), stderr)
	}
}

// End to end: podman machine start is denied when the budget cannot fit
// the machine's memory, from inspect, and allowed when it can; the
// allowed start's lease holds that memory (#230).
func TestShimGatePodmanMachineAgainstTheDaemon(t *testing.T) {
	inspect, err := os.ReadFile(filepath.Join("testdata", "podman-machine-inspect.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults("/x")
	base := cfg.Budget.Params().HostBaselineBytes + cfg.PolicyConfig().MinHeadroomBytes
	host := &sizedHost{total: base + 1<<30} // room for 1 GiB: not the machine's 2
	envMap, d := serveDaemonFrom(t, []daemon.Source{host}, func(d *daemon.Daemon) {
		wireGate(d, cfg, discardLog(), nil)
	})
	r := newShimRig(t)
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	var stderr strings.Builder
	execed := false
	shimEnv := Env{Args: []string{"podman", "machine", "start"}, Stdout: io.Discard, Stderr: &stderr,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env },
		exec:       func(string, []string, []string) error { execed = true; return nil },
		machineAsk: func(string, []string) ([]byte, error) { return inspect, nil }}
	if code := Run(shimEnv); code != exitDenied || execed || !strings.Contains(stderr.String(), "not starting `podman machine start`") {
		t.Fatalf("exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
	host.set(base + 3<<30)
	d.Tick(context.Background())
	if code := Run(shimEnv); code != 0 || !execed {
		t.Fatalf("with room: exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
	d.Tick(context.Background())
	s := d.Snapshot()
	if len(s.Leases) != 1 || s.Leases[0].Kind != "machine" || s.Leases[0].Bytes != 2<<30 || s.Leases[0].Command != "podman machine start" {
		t.Fatalf("leases %+v", s.Leases)
	}
}

// podman machine init --now asks with the memory its own --memory gives
// (MiB), or Podman's default of 2048 MiB, and names the machine it makes:
// podman-machine-default when it names none. It asks no inspect: the
// machine does not exist yet. Without --now it starts nothing. Past an
// option the parser does not know, a machine init, pod restart or clone,
// or container clone is not gated (R7) (#241).
func TestShimGatePodmanMachineInit(t *testing.T) {
	r := newShimRig(t)
	r.machineAsk = func(_ string, args []string) ([]byte, error) {
		t.Errorf("asked inspect %q", args)
		return nil, errors.New("no machine")
	}
	r.env = append(r.env, "HEADROOM_WORKTREE=w")
	r.ask = allow
	for _, c := range []struct {
		argv []string
		want protocol.CheckRequest
	}{
		{[]string{"podman", "machine", "init", "--now", "--memory", "4096", "vm-a"},
			protocol.CheckRequest{Worktree: "w", Kind: "machine", Op: "init", Command: "podman machine init vm-a", CostBytes: 4 << 30, Target: "vm-a"}},
		{[]string{"podman", "machine", "init", "--now"},
			protocol.CheckRequest{Worktree: "w", Kind: "machine", Op: "init", Command: "podman machine init", CostBytes: 2 << 30, Target: "podman-machine-default"}},
	} {
		r.asked = nil
		code, stderr := r.run(c.argv...)
		if code != 0 || !slices.Equal(r.execArgv, c.argv) {
			t.Fatalf("%q: exit %d, exec %q, stderr %q", c.argv, code, r.execArgv, stderr)
		}
		if len(r.asked) != 1 || !equalRequest(r.asked[0], c.want) || r.asked[0].Op != c.want.Op || r.asked[0].Target != c.want.Target {
			t.Fatalf("%q: asked %+v, want %+v", c.argv, r.asked, c.want)
		}
	}
	r.asked, r.execed = nil, ""
	if code, _ := r.run("podman", "machine", "init", "--memory", "4096", "vm-a"); code != 0 || r.execed == "" || len(r.asked) != 0 {
		t.Fatalf("without --now: exit %d, exec %q, asked %+v", code, r.execed, r.asked)
	}
	for _, argv := range [][]string{
		{"podman", "machine", "init", "--now", "--future", "x", "vm-a"},
		{"podman", "pod", "restart", "--future", "web"},
		{"podman", "pod", "clone", "--start", "--future", "x", "src", "new"},
		{"podman", "container", "clone", "--run", "--future", "x", "c", "new"},
	} {
		r.execed = ""
		code, stderr := r.run(argv...)
		cmd := strings.Join(argv[:3], " ")
		if code != 0 || r.execed == "" || len(r.asked) != 0 ||
			!strings.Contains(stderr, "headroom: not gated ("+cmd+": an option the shim does not know); running `"+cmd+"` anyway") {
			t.Fatalf("%q guessed: exit %d, exec %q, asked %+v, stderr %q", argv, code, r.execed, r.asked, stderr)
		}
	}
	// A container clone with no new name has nothing to key its lease:
	// not gated either (#241 plan amendment 1).
	r.execed = ""
	code, stderr := r.run("podman", "container", "clone", "--run", "c")
	if code != 0 || r.execed == "" || len(r.asked) != 0 ||
		!strings.Contains(stderr, "headroom: not gated (podman container clone: no name for the new container); running `podman container clone` anyway") {
		t.Fatalf("nameless clone: exit %d, exec %q, asked %+v, stderr %q", code, r.execed, r.asked, stderr)
	}
}

// End to end: podman machine init --now --memory 4096 is denied when the
// budget cannot fit 4 GiB, and allowed when it can; the allowed init's
// lease holds the 4 GiB (#241).
func TestShimGatePodmanMachineInitAgainstTheDaemon(t *testing.T) {
	cfg := config.Defaults("/x")
	base := cfg.Budget.Params().HostBaselineBytes + cfg.PolicyConfig().MinHeadroomBytes
	host := &sizedHost{total: base + 3<<30} // room for 3 GiB: not 4
	envMap, d := serveDaemonFrom(t, []daemon.Source{host}, func(d *daemon.Daemon) {
		wireGate(d, cfg, discardLog(), nil)
	})
	r := newShimRig(t)
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w"}
	var stderr strings.Builder
	execed := false
	shimEnv := Env{Args: []string{"podman", "machine", "init", "--now", "--memory", "4096", "vm-a"}, Stdout: io.Discard, Stderr: &stderr,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env },
		exec:       func(string, []string, []string) error { execed = true; return nil },
		machineAsk: func(string, []string) ([]byte, error) { return nil, errors.New("no machine") }}
	if code := Run(shimEnv); code != exitDenied || execed || !strings.Contains(stderr.String(), "not starting `podman machine init vm-a`") {
		t.Fatalf("exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
	host.set(base + 5<<30)
	d.Tick(context.Background())
	if code := Run(shimEnv); code != 0 || !execed {
		t.Fatalf("with room: exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
	d.Tick(context.Background())
	s := d.Snapshot()
	if len(s.Leases) != 1 || s.Leases[0].Kind != "machine" || s.Leases[0].Bytes != 4<<30 || s.Leases[0].Command != "podman machine init" {
		t.Fatalf("leases %+v", s.Leases)
	}
}

// sizedHost is a host source whose total memory a test can change.
type sizedHost struct {
	mu    sync.Mutex
	total uint64
}

func (h *sizedHost) set(total uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total = total
}

func (h *sizedHost) Name() string { return "host" }
func (h *sizedHost) Collect(context.Context) (daemon.Reading, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sizedHostReading{h.total}, nil
}

type sizedHostReading struct{ total uint64 }

func (r sizedHostReading) Apply(s *protocol.Snapshot) {
	s.Host = &protocol.Host{TotalBytes: r.total, Pressure: "normal"}
}

// fakeTart is a Tart source whose running VMs a test can change.
type fakeTart struct {
	mu  sync.Mutex
	vms []protocol.TartVM
}

func (f *fakeTart) Name() string { return "tart" }
func (f *fakeTart) Collect(context.Context) (daemon.Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := protocol.Tart{Installed: true, VMs: slices.Clone(f.vms)}
	for _, vm := range t.VMs {
		if vm.OS == "darwin" {
			t.MacOSRunning++
		}
	}
	return tartReading{t}, nil
}

type tartReading struct{ t protocol.Tart }

func (r tartReading) Apply(s *protocol.Snapshot) { s.Tart = &r.t }

// End to end: a third macOS VM is denied naming both holders; waiting, it
// starts once one of them stops (R6).
func TestShimGateMacOSSlotsAgainstTheDaemon(t *testing.T) {
	tart := &fakeTart{vms: []protocol.TartVM{{Name: "a-mac", OS: "darwin"}, {Name: "b-mac", OS: "darwin"}}}
	envMap, d := serveDaemonFrom(t, []daemon.Source{hostSource{}, tart}, func(d *daemon.Daemon) {
		wireGate(d, config.Defaults("/x"), discardLog(), nil)
	})
	r := newShimRig(t)
	tartHome := filepath.Join(r.dir, "tarthome")
	if err := os.MkdirAll(filepath.Join(tartHome, "vms", "c-mac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tartHome, "vms", "c-mac", "config.json"), []byte(`{"os":"darwin","memorySize":8589934592}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + r.dir, "HEADROOM_CONFIG_DIR=" + envMap["HEADROOM_CONFIG_DIR"], "HEADROOM_WORKTREE=w", "TART_HOME=" + tartHome}
	var stderr strings.Builder
	execed := false
	shimEnv := Env{Args: []string{"tart", "run", "c-mac"}, Stdout: io.Discard, Stderr: &stderr,
		Getenv: func(string) string { return "" }, Environ: func() []string { return env },
		exec: func(string, []string, []string) error { execed = true; return nil }}
	if code := Run(shimEnv); code != exitDenied || execed || !strings.Contains(stderr.String(), "a-mac (manual)") ||
		!strings.Contains(stderr.String(), "b-mac (manual)") {
		t.Fatalf("exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
	env = append(env, "BUDGET_WAIT=1")
	shimEnv.wait = func(dur time.Duration) os.Signal {
		if dur == 0 {
			return nil // no signal pending
		}
		tart.mu.Lock()
		tart.vms = tart.vms[1:] // a-mac stops
		tart.mu.Unlock()
		d.Tick(context.Background())
		return nil
	}
	if code := Run(shimEnv); code != 0 || !execed {
		t.Fatalf("waiting: exit %d, exec %v, stderr %q", code, execed, stderr.String())
	}
}

func TestDenyHintFitsTheReason(t *testing.T) {
	for _, c := range []struct {
		d    protocol.Decision
		want string
	}{
		{protocol.Decision{Retry: true, Reasons: []protocol.Reason{{Code: policy.Headroom, Retry: true}}}, "BUDGET_WAIT=1 to wait for room"},
		{protocol.Decision{Retry: true, Reasons: []protocol.Reason{{Code: policy.VMSlots, Retry: true}}}, "BUDGET_WAIT=1 to wait for a slot"},
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.VMSlots}}}, "budget.max_macos_vms"},
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.WorktreeCap}}}, "reuse or stop"},
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.IdleHolder}}}, "reuse or stop"},
		// Full slots can wait; the cap that also fails cannot.
		{protocol.Decision{Reasons: []protocol.Reason{{Code: policy.VMSlots, Retry: true}, {Code: policy.WorktreeCap}}}, "reuse or stop"},
	} {
		if got := denyHint(&c.d); !strings.Contains(got, c.want) {
			t.Errorf("%+v: hint %q, want %q", c.d.Reasons, got, c.want)
		}
	}
}

// Only a daemon that does not know the check is called another version;
// any other daemon error is shown as it is.
func TestDaemonCause(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{daemonError{said: `unknown op "check"`}, "is another version: restart it with this build: headroom install"},
		{daemonError{said: "check: not supported by this daemon"}, "gave no decision (check: not supported by this daemon); restart it"},
		{daemonError{}, "gave no decision; restart it"},
		{daemonError{said: "bad request: unexpected EOF"}, "gave no decision (bad request: unexpected EOF); restart it"},
	} {
		if got := daemonCause(c.err, time.Second, "/nonexistent/d.sock"); !strings.Contains(got, c.want) {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

// The shim reads its environment as the child will: the first of
// duplicates, as libc's getenv does.
func TestEnvOfReadsTheFirstOfDuplicates(t *testing.T) {
	_, getenv := Env{Environ: func() []string { return []string{"A=1", "A=2"} }}.envOf()
	if got := getenv("A"); got != "1" {
		t.Fatalf("A = %q", got)
	}
}

// An allowed run carries its lease, so the daemon tells its container from
// one started past the shim (#33).
func TestShimLabelsAllowedRuns(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	if code, stderr := r.run("docker", "run", "--rm", "alpine", "true"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := []string{"docker", "run", "--rm", "--label", protocol.LeaseLabel + "=lease-7", "alpine", "true"}
	if !slices.Equal(r.execArgv, want) {
		t.Fatalf("argv = %q, want %q", r.execArgv, want)
	}
	if !r.asked[0].Labelled {
		t.Fatal("the check did not say the call is labelled")
	}
}

func TestShimDoesNotLabelStart(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	r.run("docker", "start", "db")
	if !slices.Equal(r.execArgv, []string{"docker", "start", "db"}) || r.asked[0].Labelled {
		t.Fatalf("argv = %q, labelled %v", r.execArgv, r.asked[0].Labelled)
	}
}

func TestDockerEndpointFollowsTheCLI(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"DOCKER_CONFIG": dir}
	get := func(k string) string { return env[k] }
	context := func(name, host string) {
		t.Helper()
		sum := sha256.Sum256([]byte(name))
		meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(meta, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `"}}}`
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	use := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"`+name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := dockerEndpointIn(get, shim.Call{}); got != "unix:///var/run/docker.sock" {
		t.Fatalf("no config: %q, want Docker's default socket", got)
	}
	context("desktop-linux", "unix:///Users/dev/.docker/run/docker.sock")
	use("desktop-linux")
	if got := dockerEndpointIn(get, shim.Call{}); got != "unix:///Users/dev/.docker/run/docker.sock" {
		t.Fatalf("desktop-linux: %q", got)
	}
	use("missing")
	if got := dockerEndpointIn(get, shim.Call{}); got != "" {
		t.Fatalf("a context with no metadata: %q, want unknown", got)
	}
	env["DOCKER_CONTEXT"] = "desktop-linux"
	if got := dockerEndpointIn(get, shim.Call{}); got != "unix:///Users/dev/.docker/run/docker.sock" {
		t.Fatalf("DOCKER_CONTEXT: %q", got)
	}
	env["DOCKER_HOST"] = "unix:///x.sock"
	if got := dockerEndpointIn(get, shim.Call{}); got != "unix:///x.sock" {
		t.Fatalf("DOCKER_HOST: %q", got)
	}
	// --context or -H on the call: before DOCKER_HOST, as the CLI takes it.
	for flags, want := range map[string]string{
		"--context desktop-linux": "unix:///Users/dev/.docker/run/docker.sock",
		"--context default":       "unix:///x.sock", // the default context is DOCKER_HOST
		"-H unix:///y.sock":       "unix:///y.sock",
		"-H tcp://host:2375":      "tcp://host:2375",
	} {
		if got := dockerEndpointIn(get, shim.Parse("docker", strings.Fields(flags+" run alpine"))); got != want {
			t.Errorf("%s: %q, want %q", flags, got, want)
		}
	}
}

// A bare host:port is -H's, not a context's (a context name has no ':'):
// docker reads it as TCP, and DOCKER_HOST does not count.
func TestABareHostIsTCP(t *testing.T) {
	get := func(k string) string {
		return map[string]string{"DOCKER_CONFIG": "/tmp/none", "DOCKER_HOST": "ssh://dev@build.example"}[k]
	}
	for endpoint, want := range map[string]string{
		"localhost:2375":     "tcp://localhost:2375",
		"build.example:2376": "tcp://build.example:2376",
	} {
		if got := dockerEndpointIn(get, shim.Call{Host: endpoint, HasHost: true}); got != want {
			t.Errorf("-H %s: %q, want %q", endpoint, got, want)
		}
	}
}

// docker --config DIR reads its context from DIR.
func TestDockerEndpointHonoursConfigDir(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("colima"))
	meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"unix:///c.sock"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"colima"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(k string) string {
		if k == "HOME" {
			return "/Users/dev"
		}
		return ""
	}
	if got := dockerEndpointIn(get, shim.Call{ConfigDir: dir}); got != "unix:///c.sock" {
		t.Fatalf("endpoint = %q", got)
	}
	c := shim.Parse("docker", []string{"--config", dir, "start", "x"})
	if c.ConfigDir != dir {
		t.Fatalf("ConfigDir = %q", c.ConfigDir)
	}
}

// An unreadable config is the default context, as the docker CLI takes it
// (it warns and goes on with defaults).
func TestAnUnreadableDockerConfigIsTheDefaultContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o700); err != nil { // a directory: unreadable as a file
		t.Fatal(err)
	}
	if got := dockerEndpointIn(func(string) string { return "" }, shim.Call{ConfigDir: dir}); got != "unix:///var/run/docker.sock" {
		t.Fatalf("endpoint = %q, want the default socket", got)
	}
}

// Podman connections for the tests: remote-a and default on another
// machine, podman-machine-default on this Mac's Podman machine.
const (
	podmanFar     = "ssh://dev@198.51.100.7:22/run/podman/podman.sock"
	podmanMachine = "ssh://core@127.0.0.1:50123/run/user/501/podman/podman.sock"
)

// podmanConnections is a podman-connections.json with those connections,
// and def as its default.
func podmanConnections(def string) string {
	return `{"Connection":{"Default":"` + def + `","Connections":{` +
		`"remote-a":{"URI":"` + podmanFar + `","Identity":"/Users/dev/.ssh/id"},` +
		`"default":{"URI":"` + podmanFar + `"},` +
		`"podman-machine-default":{"URI":"` + podmanMachine + `","IsMachine":true}}}}`
}

// A malformed connections file, and one with no default but a far
// connection named "".
const (
	podmanMalformed = `{"Connection":`
	podmanNameless  = `{"Connection":{"Default":"","Connections":{"":{"URI":"` + podmanFar + `"}}}}`
)

// Podman's engine is the first of -c, --url, --context (unless default),
// CONTAINER_CONNECTION, CONTAINER_HOST and the default connection, as
// podman 6.1.3 picks it; a name is its URI in the connections file, and
// one that file does not have is unknown.
func TestPodmanEndpointFollowsTheCLI(t *testing.T) {
	const (
		remote  = podmanFar
		machine = podmanMachine
		url     = "tcp://198.51.100.8:8080"
		host    = "tcp://198.51.100.9:8080"
	)
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	conf := filepath.Join(dir, "podman-connections.json")
	write := func(path, def string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(podmanConnections(def)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(conf, "podman-machine-default")
	for _, c := range []struct {
		name  string
		flags string
		env   map[string]string
		want  string
	}{
		{"the default connection", "", nil, machine},
		{"-c", "-c remote-a", nil, remote},
		{"--connection", "--connection remote-a", nil, remote},
		{"-c before --url", "-c remote-a --url " + url, nil, remote},
		{"-c after --url", "--url " + url + " -c remote-a", nil, remote},
		{"--url", "--url " + url, map[string]string{"CONTAINER_CONNECTION": "remote-a"}, url},
		{"CONTAINER_CONNECTION", "", map[string]string{"CONTAINER_CONNECTION": "remote-a", "CONTAINER_HOST": host}, remote},
		{"CONTAINER_HOST", "", map[string]string{"CONTAINER_HOST": host}, host},
		{"an empty --url", "--url=", map[string]string{"CONTAINER_HOST": host}, host},
		{"-c over CONTAINER_HOST", "-c podman-machine-default", map[string]string{"CONTAINER_HOST": host}, machine},
		{"an unknown -c", "-c ghost", map[string]string{"CONTAINER_HOST": host}, ""},
		{"an unknown CONTAINER_CONNECTION", "", map[string]string{"CONTAINER_CONNECTION": "ghost", "CONTAINER_HOST": host}, ""},
		// --context is not -c: it ranks below --url, and default is no
		// connection (Plan amendment 1, measured on podman 6.1.3).
		{"an empty --url over CONTAINER_CONNECTION", "--url=", map[string]string{"CONTAINER_CONNECTION": "remote-a", "CONTAINER_HOST": machine}, machine},
		{"an empty --url, no CONTAINER_HOST", "--url=", map[string]string{"CONTAINER_CONNECTION": "remote-a"}, ""},
		{"--url after --context", "--context remote-a --url " + machine, nil, machine},
		{"--url before --context", "--url " + machine + " --context remote-a", nil, machine},
		{"--context default", "--context default", nil, machine},
		{"--context default, CONTAINER_CONNECTION", "--context default", map[string]string{"CONTAINER_CONNECTION": "remote-a"}, remote},
		{"--context over CONTAINER_HOST", "--context remote-a", map[string]string{"CONTAINER_HOST": machine}, remote},
		{"--context over CONTAINER_CONNECTION", "--context remote-a", map[string]string{"CONTAINER_CONNECTION": "podman-machine-default"}, remote},
		{"-c after -H", "-H " + machine + " -c remote-a", nil, remote},
	} {
		env := map[string]string{"PODMAN_CONNECTIONS_CONF": conf}
		for k, v := range c.env {
			env[k] = v
		}
		got := podmanEndpointIn(lookupMap(env), shim.Parse("podman", strings.Fields(c.flags+" run alpine")))
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// The file is PODMAN_CONNECTIONS_CONF's, else XDG_CONFIG_HOME's, else
	// HOME's; one that is missing or malformed is unknown, never
	// CONTAINER_HOST's.
	xdg, home := filepath.Join(dir, "xdg"), filepath.Join(dir, "home")
	write(filepath.Join(xdg, "containers", "podman-connections.json"), "remote-a")
	write(filepath.Join(home, ".config", "containers", "podman-connections.json"), "podman-machine-default")
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(podmanMalformed), 0o600); err != nil {
		t.Fatal(err)
	}
	// No default is no connection: podman never looks one up named "".
	nameless := filepath.Join(dir, "nameless.json")
	if err := os.WriteFile(nameless, []byte(podmanNameless), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"PODMAN_CONNECTIONS_CONF", map[string]string{"PODMAN_CONNECTIONS_CONF": conf, "XDG_CONFIG_HOME": xdg, "HOME": home}, machine},
		{"XDG_CONFIG_HOME", map[string]string{"XDG_CONFIG_HOME": xdg, "HOME": home}, remote},
		{"HOME", map[string]string{"HOME": home}, machine},
		{"a missing file", map[string]string{"PODMAN_CONNECTIONS_CONF": filepath.Join(dir, "none.json"), "CONTAINER_CONNECTION": "remote-a"}, ""},
		{"a malformed file", map[string]string{"PODMAN_CONNECTIONS_CONF": bad, "CONTAINER_CONNECTION": "remote-a"}, ""},
		{"no file at all", map[string]string{}, ""},
		{"no default", map[string]string{"PODMAN_CONNECTIONS_CONF": nameless}, ""},
	} {
		got := podmanEndpointIn(lookupMap(c.env), shim.Parse("podman", []string{"run", "alpine"}))
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// #255's measured tables, podman 6.1.3: which file it reads, and which
	// contents it reads a connection from. Each file's connection a has its
	// own URI; a relative path is read from the working directory.
	uri := func(n int) string { return "tcp://127.0.0.1:" + strconv.Itoa(41000+n) }
	conn := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	file := func(n int) string {
		return `{"Connection":{"Default":"a","Connections":{"a":{"URI":"` + uri(n) + `"}}}}`
	}
	m := filepath.Join(dir, "m")
	mhome, work := filepath.Join(m, "home"), filepath.Join(m, "work")
	abs, mxdg := filepath.Join(m, "abs", "c.json"), filepath.Join(m, "xdg")
	conn(abs, file(1))
	conn(filepath.Join(mxdg, "containers", "podman-connections.json"), file(2))
	conn(filepath.Join(mhome, ".config", "containers", "podman-connections.json"), file(3))
	conn(filepath.Join(work, "containers", "podman-connections.json"), file(4))
	conn(filepath.Join(work, "x", "containers", "podman-connections.json"), file(5))
	conn(filepath.Join(work, "rel.json"), file(6))
	t.Chdir(work)
	missing := filepath.Join(m, "abs", "none.json")
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set", nil, uri(3)},
		{"PODMAN_CONNECTIONS_CONF=/abs", map[string]string{"PODMAN_CONNECTIONS_CONF": abs}, uri(1)},
		{"PODMAN_CONNECTIONS_CONF= (empty)", map[string]string{"PODMAN_CONNECTIONS_CONF": ""}, ""},
		{"PODMAN_CONNECTIONS_CONF=rel.json", map[string]string{"PODMAN_CONNECTIONS_CONF": "rel.json"}, uri(6)},
		{"PODMAN_CONNECTIONS_CONF=/abs/missing", map[string]string{"PODMAN_CONNECTIONS_CONF": missing}, ""},
		{"PODMAN_CONNECTIONS_CONF=/abs/missing, XDG_CONFIG_HOME=/abs", map[string]string{"PODMAN_CONNECTIONS_CONF": missing, "XDG_CONFIG_HOME": mxdg}, ""},
		{"XDG_CONFIG_HOME=/abs", map[string]string{"XDG_CONFIG_HOME": mxdg}, uri(2)},
		{"XDG_CONFIG_HOME= (empty)", map[string]string{"XDG_CONFIG_HOME": ""}, uri(4)},
		{"XDG_CONFIG_HOME=x", map[string]string{"XDG_CONFIG_HOME": "x"}, uri(5)},
		{"PODMAN_CONNECTIONS_CONF= and XDG_CONFIG_HOME=/abs", map[string]string{"PODMAN_CONNECTIONS_CONF": "", "XDG_CONFIG_HOME": mxdg}, ""},
	} {
		env := map[string]string{"HOME": mhome}
		for k, v := range c.env {
			env[k] = v
		}
		got := podmanEndpointIn(lookupMap(env), shim.Parse("podman", []string{"run", "alpine"}))
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// A file podman cannot decode as a whole has no connections, for the
	// default and for -c alike; past its first value it reads nothing.
	const a = `"Connections":{"a":{"URI":"tcp://127.0.0.1:41001"}}`
	content := filepath.Join(m, "abs", "t.json")
	for _, c := range []struct {
		name, body, want string
	}{
		{"valid", `{"Connection":{"Default":"a",` + a + `}}`, uri(1)},
		{"Farm:5", `{"Connection":{"Default":"a",` + a + `},"Farm":5}`, ""},
		{"Farm.Default:5", `{"Connection":{"Default":"a",` + a + `},"Farm":{"Default":5}}`, ""},
		{`Farm.List:"x"`, `{"Connection":{"Default":"a",` + a + `},"Farm":{"List":"x"}}`, ""},
		{"Identity:5", `{"Connection":{"Default":"a","Connections":{"a":{"URI":"tcp://127.0.0.1:41001","Identity":5}}}}`, ""},
		{`IsMachine:"yes"`, `{"Connection":{"Default":"a","Connections":{"a":{"URI":"tcp://127.0.0.1:41001","IsMachine":"yes"}}}}`, ""},
		{"another connection's URI:5", `{"Connection":{"Default":"a","Connections":{"a":{"URI":"tcp://127.0.0.1:41001"},"b":{"URI":5}}}}`, ""},
		{"Default:5", `{"Connection":{"Default":5,` + a + `}}`, ""},
		{"an unknown key", `{"Connection":{"Default":"a",` + a + `},"Foo":1}`, uri(1)},
		{"an unknown connection key", `{"Connection":{"Default":"a","Connections":{"a":{"URI":"tcp://127.0.0.1:41001","Bogus":1}}}}`, uri(1)},
		{"lowercase keys", `{"connection":{"default":"a","connections":{"a":{"uri":"tcp://127.0.0.1:41001"}}}}`, uri(1)},
		{"trailing data", `{"Connection":{"Default":"a",` + a + `}} x`, uri(1)},
		{"two objects", `{"Connection":{"Default":"a",` + a + `}} {}`, uri(1)},
		{"null", `null`, ""},
		{"an empty file", ``, ""},
		{"Connection:5", `{"Connection":5}`, ""},
	} {
		conn(content, c.body)
		env := lookupMap(map[string]string{"HOME": mhome, "PODMAN_CONNECTIONS_CONF": content})
		for _, args := range [][]string{{"run", "alpine"}, {"-c", "a", "run", "alpine"}} {
			got := podmanEndpointIn(env, shim.Parse("podman", args))
			if got != c.want {
				t.Errorf("%s, %v: %q, want %q", c.name, args[:len(args)-2], got, c.want)
			}
		}
	}
}

// lookupMap is a lookup in env that tells an empty value from none.
func lookupMap(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

// A compose call's project is what Compose itself names it: docker compose
// config, run with the call's own -f, --project-directory and --env-file,
// in its own environment and directory. -p needs no asking.
func TestComposeProjectAsksCompose(t *testing.T) {
	var asked [][]string
	ask := func(bin string, args []string) ([]byte, error) {
		asked = append(asked, append([]string{bin}, args...))
		return []byte(`{"name":"shop","services":{"web":{},"db":{}}}`), nil
	}
	call := []string{"compose", "-f", "a.yml", "-f", "b.yml", "--project-directory", "/srv", "--env-file", "x.env", "up", "-d"}
	// Compose's name wins over its defaults, which it has already weighed.
	env := envMap(map[string]string{"COMPOSE_PROJECT_NAME": "other"})
	wd := func() (string, error) { return "/Users/dev/src/project-a", nil }
	if got := composeProject("/usr/local/bin/docker", call, shim.Parse("docker", call), env, wd, ask); got != "shop" {
		t.Fatalf("project = %q", got)
	}
	want := []string{"/usr/local/bin/docker", "compose", "-f", "a.yml", "-f", "b.yml", "--project-directory", "/srv", "--env-file", "x.env", "config", "--format", "json"}
	if len(asked) != 1 || !slices.Equal(asked[0], want) {
		t.Fatalf("asked %q, want %q", asked, want)
	}
	if got := composeProject("/usr/local/bin/docker", []string{"compose", "-p", "p", "up"}, shim.Call{Target: "p"}, noEnv, noWd, ask); got != "p" || len(asked) != 1 {
		t.Fatalf("-p: %q, asked again: %v", got, len(asked) != 1)
	}
	failing := func(string, []string) ([]byte, error) { return nil, errors.New("exit status 1") }
	if got := composeProject("/usr/local/bin/docker", []string{"compose", "up"}, shim.Call{}, noEnv, noWd, failing); got != "" {
		t.Fatalf("neither Compose nor its defaults could say: %q, want no key", got)
	}
}

// noEnv and noWd are a call with no environment and no working directory.
func noEnv(string) (string, bool) { return "", false }
func noWd() (string, error)       { return "", errors.New("no working directory") }

// envMap is a call's environment, m.
func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// When Compose cannot name the project (its config fails or times out), the
// project is the one Compose names by default: COMPOSE_PROJECT_NAME, else
// the project directory's name, normalised.
func TestComposeProjectWhenConfigFails(t *testing.T) {
	asks := map[string]func(string, []string) ([]byte, error){
		"fails":     func(string, []string) ([]byte, error) { return nil, errors.New("exit status 1") },
		"times out": func(string, []string) ([]byte, error) { return nil, context.DeadlineExceeded },
	}
	// A stack whose compose file is in a parent of the working directory.
	root := t.TempDir()
	stack, deeper := filepath.Join(root, "My App"), filepath.Join(root, "My App", "sub", "deeper")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stack, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		wd   string
		want string
	}{
		{"COMPOSE_PROJECT_NAME wins", []string{"compose", "--project-directory", "/srv/api", "-f", "/srv/web/compose.yaml", "up"},
			map[string]string{"COMPOSE_PROJECT_NAME": "Shop"}, "", "shop"},
		{"--project-directory before -f", []string{"compose", "--project-directory", "/srv/My App", "-f", "/srv/web/compose.yaml", "up"}, nil, "", "myapp"},
		{"a relative --project-directory", []string{"compose", "--project-directory", "../api", "up"}, nil, "", "api"},
		{"the first -f file's directory", []string{"compose", "-f", "/srv/web/compose.yaml", "-f", "/srv/other/x.yaml", "up"}, nil, "", "web"},
		{"a relative -f file", []string{"compose", "-f", "sub/Web.Site/compose.yaml", "up"}, nil, "", "website"},
		{"the first -f file that is not stdin", []string{"compose", "-f", "-", "-f", "/srv/web/compose.yaml", "up"}, nil, "", "web"},
		{"-f before COMPOSE_FILE", []string{"compose", "-f", "/srv/web/compose.yaml", "up"},
			map[string]string{"COMPOSE_FILE": "/srv/x/a.yaml"}, "", "web"},
		{"COMPOSE_FILE's first file", []string{"compose", "up"},
			map[string]string{"COMPOSE_FILE": "/srv/Stack/a.yaml:/srv/b/b.yaml"}, "", "stack"},
		{"COMPOSE_FILE split by COMPOSE_PATH_SEPARATOR", []string{"compose", "up"},
			map[string]string{"COMPOSE_FILE": "/srv/c/a.yaml;/srv/b/b.yaml", "COMPOSE_PATH_SEPARATOR": ";"}, "", "c"},
		{"the working directory, with no compose file found", []string{"compose", "up"}, nil, "", "project-a"},
		{"the directory of a compose file found in a parent", []string{"compose", "up"}, nil, deeper, "myapp"},
		{"the working directory, with its own compose file", []string{"compose", "up"}, nil, stack, "myapp"},
	} {
		c := shim.Parse("docker", tc.args)
		getenv := envMap(tc.env)
		if tc.wd == "" {
			tc.wd = "/Users/dev/src/project-a"
		}
		wd := func() (string, error) { return tc.wd, nil }
		for how, ask := range asks {
			if got := composeProject("/usr/local/bin/docker", tc.args, c, getenv, wd, ask); got != tc.want {
				t.Errorf("%s, config %s: project = %q, want %q", tc.name, how, got, tc.want)
			}
		}
	}
}

// When Compose's config fails, the fallback reads COMPOSE_PROJECT_NAME from
// the env files as Compose does (#85): the --env-files, else .env in the
// project directory; with no -f or --project-directory, the working
// directory's .env, then that of the compose file found in a parent, the
// first winning. The name is still a guess.
func TestComposeProjectWhenConfigFailsReadsEnvFiles(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stack/compose.yaml", "services: {}\n")
	write("stack/.env", "COMPOSE_PROJECT_NAME=from-stack\n")
	write("stack/deeper/x", "")
	write("stack/own/.env", "COMPOSE_PROJECT_NAME=from-own\n")
	write("stack/empty/.env", "COMPOSE_PROJECT_NAME=\n")
	write("stack/unknown/.env", "COMPOSE_PROJECT_NAME=$UNSET_X\n")
	write("plain/.env", "COMPOSE_PROJECT_NAME=from-plain\n")
	write("web/compose.yaml", "services: {}\n")
	write("web/.env", "COMPOSE_PROJECT_NAME=from-web\n")
	write("x.env", "COMPOSE_PROJECT_NAME=from-x\n")
	write("dollar/.env", "COMPOSE_PROJECT_NAME=$OTHER\n")
	failing := func(string, []string) ([]byte, error) { return nil, errors.New("exit status 1") }
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		wd   string
		want string
	}{
		{"the working directory's .env", []string{"compose", "up"}, nil, "plain", "from-plain"},
		{"--project-directory's .env", []string{"compose", "--project-directory", "plain", "up"}, nil, "", "from-plain"},
		{"the first -f file's directory's .env", []string{"compose", "-f", "web/compose.yaml", "up"}, nil, "", "from-web"},
		{"COMPOSE_FILE's directory's .env", []string{"compose", "up"}, map[string]string{"COMPOSE_FILE": "web/compose.yaml"}, "", "from-web"},
		{"--env-file, relative to the working directory", []string{"compose", "--project-directory", "plain", "--env-file", "x.env", "up"}, nil, "", "from-x"},
		{"a missing --env-file: unknown", []string{"compose", "--project-directory", "plain", "--env-file", "x.env", "--env-file", "missing.env", "up"}, nil, "", "plain"},
		{"the found compose file's .env", []string{"compose", "up"}, nil, "stack/deeper", "from-stack"},
		{"the working directory's .env before the found one's", []string{"compose", "up"}, nil, "stack/own", "from-own"},
		// The first .env that sets it wins, even to a name the shim cannot know.
		{"the working directory's .env, set empty, before the found one's", []string{"compose", "up"}, nil, "stack/empty", "stack"},
		{"the working directory's .env, interpolated, before the found one's", []string{"compose", "up"}, nil, "stack/unknown", "stack"},
		{"the process environment beats .env", []string{"compose", "up"}, map[string]string{"COMPOSE_PROJECT_NAME": "Env"}, "plain", "env"},
		{"an empty COMPOSE_PROJECT_NAME in the environment: the directory's, not .env", []string{"compose", "up"}, map[string]string{"COMPOSE_PROJECT_NAME": ""}, "plain", "plain"},
		{"an interpolated name: the directory's", []string{"compose", "up"}, nil, "dollar", "dollar"},
	} {
		c := shim.Parse("docker", tc.args)
		getenv := envMap(tc.env)
		wd := func() (string, error) { return filepath.Join(root, tc.wd), nil }
		got, guessed := composeKey("/usr/local/bin/docker", tc.args, c, getenv, wd, failing, time.Now, os.Stat)
		if got != tc.want || !guessed {
			t.Errorf("%s: project = %q, guessed %v, want %q, guessed", tc.name, got, guessed, tc.want)
		}
	}
}

// The search for a compose file in the parents stops within what is left of
// config's budget (#84): a parent may be a mount that hangs (autofs's /net),
// and the call has already waited for config. Cut short, the project is the
// working directory's, as when no file is found.
func TestComposeFileSearchStopsWithinTheBudget(t *testing.T) {
	wd := func() (string, error) { return "/Users/dev/src/project-a/sub", nil }
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	for _, tc := range []struct {
		name  string
		took  time.Duration // what config took of composeTimeout
		stat  func(string) (fs.FileInfo, error)
		want  string
		stats bool // whether the search may stat at all
	}{
		{"time left: found in a parent", time.Second, func(p string) (fs.FileInfo, error) {
			if p == "/Users/dev/src/project-a/compose.yaml" {
				return nil, nil
			}
			return nil, fs.ErrNotExist
		}, "project-a", true},
		{"no time left", composeTimeout, nil, "sub", false},
		{"a parent hangs", composeTimeout - 50*time.Millisecond, func(string) (fs.FileInfo, error) { <-hung; return nil, fs.ErrNotExist }, "sub", true},
	} {
		now := time.Unix(1e9, 0)
		clock := func() time.Time { return now }
		ask := func(string, []string) ([]byte, error) { now = now.Add(tc.took); return nil, context.DeadlineExceeded }
		stat := func(p string) (fs.FileInfo, error) {
			if !tc.stats {
				t.Errorf("%s: stat %s", tc.name, p)
				return nil, fs.ErrNotExist
			}
			return tc.stat(p)
		}
		type key struct {
			project string
			guessed bool
		}
		done := make(chan key, 1)
		go func() {
			p, g := composeKey("/usr/local/bin/docker", []string{"compose", "up"}, shim.Call{}, noEnv, wd, ask, clock, stat)
			done <- key{p, g}
		}()
		select {
		case got := <-done:
			if got != (key{tc.want, true}) {
				t.Errorf("%s: %+v, want %q, guessed", tc.name, got, tc.want)
			}
		case <-time.After(time.Second):
			t.Errorf("%s: still searching after a second", tc.name)
		}
	}
}

// The fallback name is normalised as compose-go's NormalizeProjectName does
// it: lower case (Unicode's), then only a-z, 0-9, - and _, with no leading
// - or _. With nothing left, no key.
func TestComposeProjectNormalisedAsCompose(t *testing.T) {
	failing := func(string, []string) ([]byte, error) { return nil, errors.New("exit status 1") }
	wd := func() (string, error) { return "/Users/dev/src/project-a", nil }
	for dir, want := range map[string]string{
		"My App":       "myapp",
		".hidden":      "hidden",
		"Web.Site_2-x": "website_2-x",
		"__-api":       "api",
		"9lives":       "9lives",
		"Café":         "caf",
		"\u212Aelvin":  "kelvin", // the Kelvin sign: lower case, it is k
		"___":          "",
		"Éü":           "",
	} {
		args := []string{"compose", "--project-directory", "/srv/" + dir, "up"}
		if got := composeProject("/usr/local/bin/docker", args, shim.Parse("docker", args), noEnv, wd, failing); got != want {
			t.Errorf("%q: project = %q, want %q", dir, got, want)
		}
	}
}

// The real thing, when Docker Compose is here: it names a project from an
// override file and an interpolated .env, as no reimplementation kept up.
func TestComposeProjectFromRealCompose(t *testing.T) {
	bin, err := exec.LookPath("docker")
	if err != nil || exec.Command(bin, "compose", "version").Run() != nil {
		t.Skip("no docker compose")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", "services:\n  web:\n    image: alpine\n")
	write("compose.override.yaml", "name: ${STACK}-shop\n")
	write(".env", "STACK=blue\n")
	ask := func(bin string, args []string) ([]byte, error) { return askCompose(bin, args, os.Environ(), dir) }
	if got := composeProject(bin, []string{"compose", "up"}, shim.Call{}, noEnv, noWd, ask); got != "blue-shop" {
		t.Fatalf("project = %q, want blue-shop", got)
	}
}

func TestComposeProjectForwardsDockersConfig(t *testing.T) {
	var asked []string
	ask := func(_ string, args []string) ([]byte, error) { asked = args; return []byte(`{"name":"x"}`), nil }
	composeProject("/d", []string{"--config", "/work/.docker", "compose", "up"}, shim.Call{}, noEnv, noWd, ask)
	if len(asked) < 3 || asked[0] != "--config" || asked[1] != "/work/.docker" || asked[2] != "compose" {
		t.Fatalf("asked %q", asked)
	}
}

// Compose's config is asked only when it must name the project: not for
// -p, not for podman. Its dry run is asked for an up or restart.
func TestComposeIsAskedOnlyWhenNeeded(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	asked, dry := 0, 0
	r.composeAsk = func(string, []string) ([]byte, error) { asked++; return []byte(`{"name":"x"}`), nil }
	r.composeDry = func(string, []string) ([]byte, error) { dry++; return []byte(" Container x-a-1 Running \n"), nil }
	r.run("docker", "compose", "-p", "shop", "run", "web")
	if asked != 0 || dry != 0 || r.asked[0].Target != "shop" {
		t.Fatalf("asked %d, dry %d, target %q", asked, dry, r.asked[0].Target)
	}
	r.run("docker", "compose", "up", "-d")
	if asked != 2 || dry != 1 || r.asked[1].Target != "x" || !r.asked[1].Idle { // name, then providers
		t.Fatalf("asked %d, dry %d, %+v", asked, dry, r.asked[1])
	}
	r.run("podman", "compose", "up", "-d")
	if asked != 2 || dry != 1 {
		t.Fatalf("podman: asked %d, dry %d", asked, dry)
	}
}

// The name the shim falls back on when Compose's config fails is sent as a
// guess (#84). Not -p, not Compose's own name, and not stdin's, which the
// shim names as Compose does but for a name: in the piped file (#85).
func TestAGuessedProjectIsSentAsAGuess(t *testing.T) {
	r := newShimRig(t)
	base := append(slices.Clone(r.env), "ORCA_WORKTREE_ID=w")
	named := "COMPOSE_PROJECT_NAME=Shop"
	envFile := filepath.Join(r.dir, "x.env")
	if err := os.WriteFile(envFile, []byte("COMPOSE_PROJECT_NAME=from-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.ask = allow
	config := []byte(`{"name":"x"}`)
	r.composeAsk = func(string, []string) ([]byte, error) {
		if config == nil {
			return nil, errors.New("exit status 1")
		}
		return config, nil
	}
	for _, tc := range []struct {
		name    string
		fails   bool
		env     string
		call    []string
		target  string
		guessed bool
	}{
		{"config fails", true, named, []string{"docker", "compose", "up", "-d"}, "shop", true},
		{"config names it", false, named, []string{"docker", "compose", "up", "-d"}, "x", false},
		{"-p", true, named, []string{"docker", "compose", "-p", "p", "up", "-d"}, "p", false},
		{"stdin, the project directory's", true, "", []string{"docker", "compose", "--project-directory", "/srv/api", "-f", "-", "up", "-d"}, "api", false},
		{"stdin, from the environment", true, named, []string{"docker", "compose", "-f", "-", "up", "-d"}, "shop", false},
		{"stdin, from an env file", true, "", []string{"docker", "compose", "--env-file", envFile, "-f", "-", "up", "-d"}, "from-x", false},
		{"stdin, -p", true, named, []string{"docker", "compose", "-p", "p", "-f", "-", "up", "-d"}, "p", false},
	} {
		config = []byte(`{"name":"x"}`)
		if tc.fails {
			config = nil
		}
		r.env = slices.Clone(base)
		if tc.env != "" {
			r.env = append(r.env, tc.env)
		}
		r.asked = nil
		r.run(tc.call...)
		if len(r.asked) != 1 || r.asked[0].Target != tc.target || r.asked[0].Guessed != tc.guessed {
			t.Errorf("%s: asked %+v, want target %q, guessed %v", tc.name, r.asked, tc.target, tc.guessed)
		}
	}
}

// A repeat stdin up of a running stack in a plain directory sends the
// directory's name as exact, as before #85: the lease book takes the first
// up's lease over instead of reserving the estimate again beside it.
func TestARepeatStdinUpTakesTheFirstLeaseOver(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=w")
	plain, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(plain) })
	book := lease.New(time.Minute, time.Now, discardLog())
	check := gateCheckOn(book, config.Defaults("/x").PolicyConfig(), nil, "")
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Budget: &protocol.Budget{HeadroomBytes: &headroom}}
	r.ask = func(req protocol.CheckRequest) (*protocol.Decision, error) {
		d := check(&req, s)
		return &d, nil
	}
	for range 2 {
		if code, stderr := r.run("docker", "compose", "--project-directory", plain, "-f", "-", "up", "-d"); code != 0 {
			t.Fatalf("exit %d, %s", code, stderr)
		}
	}
	if l := book.List(); len(l) != 1 {
		t.Fatalf("leases = %+v, want the first taken over", l)
	}
}

// A repeat stdin up whose .env names its project with a value the shim
// cannot know sends no name but its dir (#122): the lease book takes the
// first up's lease over, by that dir, instead of reserving again beside it.
func TestARepeatUnnamedStdinUpTakesTheFirstLeaseOver(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=w")
	plain, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(plain) })
	if err := os.WriteFile(filepath.Join(plain, ".env"), []byte("COMPOSE_PROJECT_NAME=$X\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	book := lease.New(time.Minute, time.Now, discardLog())
	check := gateCheckOn(book, config.Defaults("/x").PolicyConfig(), nil, "")
	headroom := int64(64 << 30)
	s := &protocol.Snapshot{Budget: &protocol.Budget{HeadroomBytes: &headroom}}
	r.ask = func(req protocol.CheckRequest) (*protocol.Decision, error) {
		d := check(&req, s)
		return &d, nil
	}
	for range 2 {
		if code, stderr := r.run("docker", "compose", "--project-directory", plain, "-f", "-", "up", "-d"); code != 0 {
			t.Fatalf("exit %d, %s", code, stderr)
		}
	}
	if l := book.List(); len(l) != 1 || !r.asked[0].Unnamed {
		t.Fatalf("asked %+v, leases = %+v, want the first taken over", r.asked, l)
	}
}

// compose -f - reads its file from stdin, which the shim must not consume:
// the project is what Compose names it then, COMPOSE_PROJECT_NAME or the
// project directory's name (the working directory's).
func TestComposeProjectOfStdin(t *testing.T) {
	wd := func() (string, error) { return "/Users/dev/src/My_App", nil }
	none := noEnv
	if got := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, none, wd); got != "my_app" {
		t.Fatalf("project = %q, want my_app", got)
	}
	env := envMap(map[string]string{"COMPOSE_PROJECT_NAME": "Piped"})
	if got := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, env, wd); got != "piped" {
		t.Fatalf("project = %q, want piped", got)
	}
	if got := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}, ComposeProjectDir: "/srv/_api"}, none, wd); got != "api" {
		t.Fatalf("project = %q, want api", got)
	}
}

// compose -f - with no working directory: the project directory is still
// known when Compose's GetWorkingDir needs none, an absolute
// --project-directory or first -f file.
func TestComposeProjectOfStdinWithoutAWorkingDirectory(t *testing.T) {
	wd := func() (string, error) { return "", errors.New("getwd: no such file or directory") }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"compose", "--project-directory", "/srv/_api", "-f", "-", "up", "-d"}, "api"},
		{[]string{"compose", "-f", "-", "-f", "/srv/web/base.yml", "up", "-d"}, "web"},
		{[]string{"compose", "--project-directory", "api", "-f", "-", "up", "-d"}, ""},
		{[]string{"compose", "-f", "-", "up", "-d"}, ""},
	} {
		if got := composeStdinProject(shim.Parse("docker", tc.args), noEnv, wd); got != tc.want {
			t.Errorf("%q: project = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// compose -f - takes COMPOSE_PROJECT_NAME from the env files Compose loads
// (#85): the --env-files, relative to the working directory, a later one
// winning; else .env in the project directory.
func TestComposeProjectOfStdinFromEnvFiles(t *testing.T) {
	root := t.TempDir()
	app, sub := filepath.Join(root, "app"), filepath.Join(root, "app", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(app, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "COMPOSE_PROJECT_NAME=Dotted\n")
	write("x.env", "COMPOSE_PROJECT_NAME=from-x\n")
	write("y.env", "OTHER=1\nCOMPOSE_PROJECT_NAME=from-y\n")
	write("none.env", "OTHER=1\n")
	write("sub/.env", "COMPOSE_PROJECT_NAME=from-sub\n")
	write("locked.env", "COMPOSE_PROJECT_NAME=from-locked\n")
	if err := os.Chmod(filepath.Join(app, "locked.env"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(app, "~"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("~/p.env", "COMPOSE_PROJECT_NAME=from-cwd-tilde\n")
	write("~/t.env", "COMPOSE_PROJECT_NAME=from-cwd-tilde\n")
	for _, f := range []string{"p.env", "h.env"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("COMPOSE_PROJECT_NAME=from-home\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wd := func() (string, error) { return app, nil }
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{".env in the working directory", []string{"compose", "-f", "-", "up", "-d"}, nil, "dotted"},
		{".env in --project-directory", []string{"compose", "--project-directory", "sub", "-f", "-", "up", "-d"}, nil, "from-sub"},
		{"--env-file instead of .env", []string{"compose", "--env-file", "x.env", "-f", "-", "up", "-d"}, nil, "from-x"},
		{"the last --env-file wins", []string{"compose", "--env-file", "x.env", "--env-file", "y.env", "-f", "-", "up", "-d"}, nil, "from-y"},
		{"an earlier --env-file, when a later one does not set it", []string{"compose", "--env-file", "x.env", "--env-file", "none.env", "-f", "-", "up", "-d"}, nil, "from-x"},
		{"--env-file relative to the working directory, not the project's", []string{"compose", "--project-directory", "sub", "--env-file", "x.env", "-f", "-", "up", "-d"}, nil, "from-x"},
		{"an --env-file that does not set it: the directory's name, not .env", []string{"compose", "--env-file", "none.env", "-f", "-", "up", "-d"}, nil, "app"},
		// Compose fails on an --env-file it cannot read ("couldn't find env
		// file"): unknown, whatever the others set.
		{"a missing --env-file: unknown", []string{"compose", "--env-file", "missing.env", "--env-file", "x.env", "-f", "-", "up", "-d"}, nil, "app"},
		{"a missing --env-file after one that sets it: unknown", []string{"compose", "--env-file", "x.env", "--env-file", "missing.env", "-f", "-", "up", "-d"}, nil, "app"},
		{"an --env-file that cannot be read: unknown", []string{"compose", "--env-file", "x.env", "--env-file", "locked.env", "-f", "-", "up", "-d"}, nil, "app"},
		{"an --env-file that is a directory: unknown", []string{"compose", "--env-file", "x.env", "--env-file", "sub", "-f", "-", "up", "-d"}, nil, "app"},
		{"a missing COMPOSE_ENV_FILES entry: unknown", []string{"compose", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_ENV_FILES": "x.env,missing.env"}, "app"},
		// Compose reads a ~ path first as it is, in the working directory
		// (setEnvWithDotEnv), then in the home directory (paths.ExpandUser):
		// it needs both, and the first's name wins.
		{"~/p.env in both the working and the home directory: the working directory's", []string{"compose", "--env-file=~/p.env", "-f", "-", "up", "-d"},
			map[string]string{"HOME": root}, "from-cwd-tilde"},
		{"--env-file=~/h.env only in the home directory: unknown", []string{"compose", "--env-file=~/h.env", "-f", "-", "up", "-d"},
			map[string]string{"HOME": root}, "app"},
		{"a COMPOSE_ENV_FILES entry ~/h.env only in the home directory: unknown", []string{"compose", "-f", "-", "up", "-d"},
			map[string]string{"HOME": root, "COMPOSE_ENV_FILES": "x.env,~/h.env"}, "app"},
		{"--env-file=~/t.env only in the working directory: unknown", []string{"compose", "--env-file=~/t.env", "-f", "-", "up", "-d"},
			map[string]string{"HOME": root}, "app"},
		{"~/h.env without a HOME: relative, so missing", []string{"compose", "--env-file=~/h.env", "-f", "-", "up", "-d"}, nil, "app"},
		{"COMPOSE_ENV_FILES instead of .env", []string{"compose", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_ENV_FILES": "x.env,y.env"}, "from-y"},
		{"--env-file before COMPOSE_ENV_FILES", []string{"compose", "--env-file", "x.env", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_ENV_FILES": "y.env"}, "from-x"},
		{"COMPOSE_DISABLE_ENV_FILE: no .env", []string{"compose", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_DISABLE_ENV_FILE": "true"}, "app"},
		{"COMPOSE_DISABLE_ENV_FILE: --env-file still read", []string{"compose", "--env-file", "x.env", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_DISABLE_ENV_FILE": "1"}, "from-x"},
		{"the process environment beats the env files", []string{"compose", "--env-file", "x.env", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_PROJECT_NAME": "Env"}, "env"},
		{"the process environment beats .env", []string{"compose", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_PROJECT_NAME": "Env"}, "env"},
		{"an empty COMPOSE_PROJECT_NAME in the environment: the directory's, not .env", []string{"compose", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_PROJECT_NAME": ""}, "app"},
		{"-p beats both", []string{"compose", "-p", "flag", "--env-file", "x.env", "-f", "-", "up", "-d"},
			map[string]string{"COMPOSE_PROJECT_NAME": "Env"}, "flag"},
	} {
		if got := composeStdinProject(shim.Parse("docker", tc.args), envMap(tc.env), wd); got != tc.want {
			t.Errorf("%s: project = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// compose -f - with another -f file: the project directory is that file's,
// as Compose's GetWorkingDir has it (the first -f that is not -), for its
// .env and its name (#85).
func TestComposeProjectOfStdinWithAnotherFile(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	for name, body := range map[string]string{
		".env":            "COMPOSE_PROJECT_NAME=from-cwd\n",
		"ops/base.yml":    "services: {}\n",
		"opsenv/base.yml": "services: {}\n",
		"opsenv/.env":     "COMPOSE_PROJECT_NAME=from-ops\n",
	} {
		p := filepath.Join(app, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wd := func() (string, error) { return app, nil }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"compose", "-f", "ops/base.yml", "-f", "-", "up", "-d"}, "ops"},
		{[]string{"compose", "-f", "-", "-f", "ops/base.yml", "up", "-d"}, "ops"},
		{[]string{"compose", "-f", "opsenv/base.yml", "-f", "-", "up", "-d"}, "from-ops"},
	} {
		if got := composeStdinProject(shim.Parse("docker", tc.args), noEnv, wd); got != tc.want {
			t.Errorf("%q: project = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// An env file is read only if it is a regular file, and only up to
// envFileMax (#85): a .env linked to /dev/zero, or a FIFO, costs nothing and
// never blocks; one past the limit sets a name the shim cannot know. Either
// way, the project is the directory's.
func TestComposeEnvFileReadIsBounded(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}
	dotenv := filepath.Join(app, ".env")
	wd := func() (string, error) { return app, nil }
	pad := strings.Repeat("# padding\n", envFileMax/10+1)
	for _, tc := range []struct {
		name string
		make func() error
	}{
		{"named past the limit", func() error {
			return os.WriteFile(dotenv, []byte(pad+"COMPOSE_PROJECT_NAME=late\n"), 0o644)
		}},
		{"named, then past the limit", func() error {
			return os.WriteFile(dotenv, []byte("COMPOSE_PROJECT_NAME=early\n"+pad), 0o644)
		}},
		{"a FIFO", func() error { return syscall.Mkfifo(dotenv, 0o644) }},
		{"a link to /dev/zero", func() error { return os.Symlink("/dev/zero", dotenv) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.RemoveAll(dotenv); err != nil {
				t.Fatal(err)
			}
			if err := tc.make(); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			got := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, noEnv, wd)
			if took := time.Since(start); took > time.Second {
				t.Errorf("took %v", took)
			}
			if got != "app" {
				t.Errorf("project = %q, want app", got)
			}
		})
	}
}

// An env file is read as compose-go's dotenv parser reads it; each want is
// what Docker Compose 5.5.1 named the project (with OTHER=o in its
// environment), except that a value Compose interpolates, and a file
// Compose fails on, are unknown, and the directory's name follows.
func TestComposeEnvFileSyntaxAsCompose(t *testing.T) {
	app := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := func() (string, error) { return app, nil }
	for _, tc := range []struct{ env, want string }{
		{"export COMPOSE_PROJECT_NAME=exp\n", "exp"},
		{"COMPOSE_PROJECT_NAME='single'\n", "single"},
		{"COMPOSE_PROJECT_NAME=\"double\"\n", "double"},
		{"# COMPOSE_PROJECT_NAME=commented\n", "app"},
		{"COMPOSE_PROJECT_NAME=inline # comment\n", "inline"},
		{"  COMPOSE_PROJECT_NAME = spaced  \n", "spaced"},
		{"COMPOSE_PROJECT_NAME: yaml\n", "yaml"},
		{"COMPOSE_PROJECT_NAME=one\nCOMPOSE_PROJECT_NAME=two\n", "two"},
		{"COMPOSE_PROJECT_NAME=first\nCOMPOSE_PROJECT_NAME\n", "first"}, // the bare key looks itself up
		{"COMPOSE_PROJECT_NAME=\n", "app"},
		{"COMPOSE_PROJECT_NAME=crlf\r\n", "crlf"},
		{"\xef\xbb\xbfCOMPOSE_PROJECT_NAME=bom\n", "bom"},
		// A quoted value spans lines: what it holds is not a statement.
		{"A=\"x\nCOMPOSE_PROJECT_NAME=fake\n\"\n", "app"},
		{"A='x\nCOMPOSE_PROJECT_NAME=fake\n'\nCOMPOSE_PROJECT_NAME=real\n", "real"},
		{"A=\"x\\\"\nCOMPOSE_PROJECT_NAME=fake\n\"\n", "app"}, // an escaped quote does not end it
		// Interpolated by Compose ("o", "o-a"): unknown here.
		{"COMPOSE_PROJECT_NAME=$OTHER\n", "app"},
		{"COMPOSE_PROJECT_NAME=\"${OTHER}-a\"\n", "app"},
		{"COMPOSE_PROJECT_NAME=named\nCOMPOSE_PROJECT_NAME=$OTHER\n", "app"},
		// An export with nothing after it but spaces: Compose fails ("zero
		// length string"), so unknown here. Before a newline, or with no
		// space, it is a statement of its own.
		{"COMPOSE_PROJECT_NAME=x\nexport ", "app"},
		{"COMPOSE_PROJECT_NAME=x\nexport\t \r", "app"},
		{"COMPOSE_PROJECT_NAME=x\nexport \n", "x"},
		{"COMPOSE_PROJECT_NAME=x\nexport", "x"},
	} {
		if err := os.WriteFile(filepath.Join(app, ".env"), []byte(tc.env), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, noEnv, wd); got != tc.want {
			t.Errorf("%q: project = %q, want %q", tc.env, got, tc.want)
		}
	}
}

// Compose's dry run decides: an up is idle only when every container it
// names already runs.
func TestComposeIdleIsComposesOwnPlan(t *testing.T) {
	for out, want := range map[string]bool{
		" Container app-db-1 Running \n Container app-web-1 Running \n": true,
		// depends_on service_healthy, or --wait: waiting starts nothing.
		" Container app-db-1 Running \n Container app-web-1 Running \n Container app-db-1 Waiting \n Container app-db-1 Healthy \n": true,
		// Older Compose v2 prefixes its plain lines; a tty one colours them.
		"DRY-RUN MODE -  Container app-db-1 Running \n":                                          true,
		"\x1b[32m\u2714\x1b[0m Container app-db-1 \x1b[32mRunning\x1b[0m\n":                      true,
		"DRY-RUN MODE -  Container app-db-1 Starting \n":                                         false,
		" Container app-db-1 Running \n Container app-web-1 Recreate \n":                         false,
		" Network app_default Created \n Container app-db-1 Creating \n":                         false,
		" Container app-db-1 Restarting \n Container app-db-1 Started \n":                        false,
		" Image busybox Pulling \n Container app-db-1 Running \n Container app-db-2 Starting \n": false,
		"":                   false,
		"no such service: x": false,
	} {
		dry := func(_ string, args []string) ([]byte, error) {
			if !slices.Contains(args, "--dry-run") {
				t.Fatalf("args %q", args)
			}
			return []byte(out), nil
		}
		if got := composeIdle("/d", []string{"compose", "up", "-d"}, dry); got != want {
			t.Errorf("%q: idle = %v, want %v", out, got, want)
		}
	}
	failing := func(string, []string) ([]byte, error) {
		return []byte(" Container a Running \n"), errors.New("exit status 1")
	}
	if composeIdle("/d", []string{"compose", "up"}, failing) {
		t.Fatal("a failed dry run says nothing")
	}
}

// BUDGET_WAIT: each ask asks Compose again. A stack that stopped during the
// wait is no idle up.
func TestAWaitingComposeUpAsksComposeEachTime(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "BUDGET_WAIT=1")
	plans := []string{" Container x-a-1 Running \n", " Container x-a-1 Starting \n"}
	r.composeAsk = func(string, []string) ([]byte, error) { return []byte(`{"name":"x"}`), nil }
	r.composeDry = func(string, []string) ([]byte, error) { p := plans[0]; plans = plans[1:]; return []byte(p), nil }
	n := 0
	r.ask = func(protocol.CheckRequest) (*protocol.Decision, error) {
		if n++; n < 2 {
			return &protocol.Decision{Retry: true, Message: "headroom: not starting it: memory pressure",
				Reasons: []protocol.Reason{{Code: "pressure", Text: "memory pressure", Retry: true}}}, nil
		}
		return allow(protocol.CheckRequest{})
	}
	r.run("docker", "compose", "up", "-d")
	if len(r.asked) != 2 || !r.asked[0].Idle || r.asked[1].Idle {
		t.Fatalf("asked %+v: want idle, then not", r.asked)
	}
}

// A project with model providers is never dry-run: Compose's dry run stubs
// the Docker API but runs a provider for real.
func TestComposeWithProvidersIsNotDryRun(t *testing.T) {
	for _, cfg := range []string{
		`{"name":"x","services":{"llm":{"provider":{"type":"model"}}}}`,
		`{"name":"x","services":{"app":{"models":["llm"]}},"models":{"llm":{"model":"ai/smollm2"}}}`,
		`{"name":"x","models":{"llm":{"model":"ai/smollm2"}}}`,
	} {
		r := newShimRig(t)
		r.ask = allow
		dry := 0
		r.composeAsk = func(string, []string) ([]byte, error) { return []byte(cfg), nil }
		r.composeDry = func(string, []string) ([]byte, error) { dry++; return []byte(" Container x-a-1 Running \n"), nil }
		r.run("docker", "compose", "-p", "x", "up", "-d")
		if dry != 0 || r.asked[0].Idle || r.asked[0].Target != "x" {
			t.Errorf("%s: dry run %d times, %+v", cfg, dry, r.asked[0])
		}
	}
}

// An attached up's dry run stops before Compose would start anything
// ("interactive run is not supported in dry-run mode"), and a restart's
// lists every container: only a detached up is dry-run.
func TestAnAttachedUpIsNotDryRun(t *testing.T) {
	for args, want := range map[string]int{"up": 0, "up --abort-on-container-exit": 0, "up -d": 1, "up --wait": 1, "restart": 0} {
		r := newShimRig(t)
		r.ask = allow
		dry := 0
		r.composeAsk = func(string, []string) ([]byte, error) { return []byte(`{"name":"x"}`), nil }
		r.composeDry = func(string, []string) ([]byte, error) { dry++; return []byte(" Container x-a-1 Running \n"), nil }
		r.run(append([]string{"docker", "compose"}, strings.Fields(args)...)...)
		if dry != want {
			t.Errorf("%s: dry run %d times, want %d", args, dry, want)
		}
	}
}

// The config the shim reads for a dry run is the call's own project: its
// global flags (-p, -f, --env-file, --config) as given, which Compose
// interpolates (include: ${COMPOSE_PROJECT_NAME}.yml).
func TestComposeConfigIsTheCallsOwnProject(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	var asked []string
	r.composeAsk = func(_ string, args []string) ([]byte, error) { asked = args; return []byte(`{"name":"evil"}`), nil }
	r.composeDry = func(string, []string) ([]byte, error) { return []byte(" Container evil-a-1 Running \n"), nil }
	r.run("docker", "--config", "/work/.docker", "compose", "-p", "evil", "-f", "c.yml", "up", "-d", "web")
	want := []string{"--config", "/work/.docker", "compose", "-p", "evil", "-f", "c.yml", "--profile", "*", "config", "--format", "json"}
	if !slices.Equal(asked, want) {
		t.Fatalf("config asked with %q, want %q", asked, want)
	}
}

// A pull or build the dry run does not do may recreate: no dry run.
func TestAPullOrBuildIsNotDryRun(t *testing.T) {
	for _, tc := range []struct {
		args, cfg string
	}{
		{"up -d --pull always", `{"name":"x"}`},
		{"up -d --pull=always", `{"name":"x"}`},
		{"up -d --build", `{"name":"x"}`},
		{"up -d --build=true", `{"name":"x"}`},
		{"up -d --build=1", `{"name":"x"}`},
		{"up -d --build=T", `{"name":"x"}`},
		{"up -d --build=yes", `{"name":"x"}`}, // Compose rejects it: unsure
		{"up -d", `{"name":"x","services":{"a":{"pull_policy":"always"}}}`},
		{"up -d", `{"name":"x","services":{"a":{"pull_policy":"daily"}}}`},
		{"up -d", `{"name":"x","services":{"a":{"pull_policy":"build"}}}`},
	} {
		r := newShimRig(t)
		r.ask = allow
		dry := 0
		r.composeAsk = func(string, []string) ([]byte, error) { return []byte(tc.cfg), nil }
		r.composeDry = func(string, []string) ([]byte, error) { dry++; return []byte(" Container x-a-1 Running \n"), nil }
		r.run(append([]string{"docker", "compose"}, strings.Fields(tc.args)...)...)
		if dry != 0 || r.asked[0].Idle {
			t.Errorf("%s %s: dry run %d times, idle %v", tc.args, tc.cfg, dry, r.asked[0].Idle)
		}
	}
}

// --build=false builds nothing: the dry run tells.
func TestBuildFalseIsDryRun(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	r.composeAsk = func(string, []string) ([]byte, error) { return []byte(`{"name":"x"}`), nil }
	r.composeDry = func(string, []string) ([]byte, error) { return []byte(" Container x-a-1 Running \n"), nil }
	r.run("docker", "compose", "up", "-d", "--build=false")
	if !r.asked[0].Idle {
		t.Fatalf("%+v", r.asked[0])
	}
}

// A failing all-profiles config costs the name nothing: it comes from the
// call's own profiles.
func TestTheNameDoesNotNeedEveryProfile(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	r.composeAsk = func(_ string, args []string) ([]byte, error) {
		if slices.Contains(args, "*") {
			return nil, errors.New("env file ./prod.env not found")
		}
		return []byte(`{"name":"shop"}`), nil
	}
	r.run("docker", "compose", "up", "-d")
	if r.asked[0].Target != "shop" || r.asked[0].Idle {
		t.Fatalf("%+v", r.asked[0])
	}
}

// The lease label is headroom's: a call that sets it could pass another
// worktree's lease off as its own (past an unknown flag, the shim's label
// goes first, and a later one wins).
func TestARunThatSetsTheLeaseLabelIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--new-flag", "x", "--label", "dev.headroom.lease=lease-abc-7", "alpine"},
		{"create", "-l=dev.headroom.lease=lease-abc-7", "alpine"},
		{"run", "--label", "dev.headroom.lease=lease-abc-7", "alpine"},
		{"run", "--label=dev.headroom.lease=lease-abc-7", "alpine"},
		{"run", "-dl", "dev.headroom.lease=lease-abc-7", "alpine"},
		{"run", "-ldev.headroom.lease=lease-abc-7", "alpine"},
		{"create", "-ql", "dev.headroom.lease=lease-abc-7", "alpine"},
		{"run", "-tqdl", "dev.headroom.lease=lease-abc-7", "alpine"},
		{"run", "-Zl", "dev.headroom.lease=lease-abc-7", "alpine"}, // a shorthand the table lacks
	} {
		r := newShimRig(t)
		r.ask = allow
		code, stderr := r.run(append([]string{"docker"}, args...)...)
		if code != exitDenied || r.execed != "" || len(r.asked) != 0 || !strings.Contains(stderr, "dev.headroom.lease") {
			t.Errorf("%q: code %d, execed %q, asked %d, stderr %q", args, code, r.execed, len(r.asked), stderr)
		}
	}
}

// Only a label is refused: the string elsewhere sets none.
func TestTheLeaseLabelNameElsewhereIsAllowed(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--rm", "alpine", "grep", "dev.headroom.lease", "/x"},
		{"run", "-e", "NOTE=dev.headroom.lease", "alpine"},
		{"run", "-el", "dev.headroom.lease", "alpine"}, // -e's value is "l"
		{"run", "--label", "note=x", "alpine", "echo", "dev.headroom.lease"},
	} {
		r := newShimRig(t)
		r.ask = allow
		if code, stderr := r.run(append([]string{"docker"}, args...)...); code != 0 || r.execed == "" {
			t.Errorf("%q: code %d, stderr %q", args, code, stderr)
		}
	}
}

// The engine is resolved as the docker CLI resolves it, before anything
// else: a call whose engine is this Mac's, or unknown, is asked about,
// even when DOCKER_HOST, which the CLI ignores for it, is remote (#90).
func TestTheCallsOwnEngineDecidesWhetherItIsGated(t *testing.T) {
	for name, args := range map[string][]string{
		"an unreadable --context": {"--context", "ghost", "run", "alpine"},
		"a bare local -H":         {"-H", "localhost:2375", "run", "alpine"},
	} {
		r := newShimRig(t)
		r.ask = allow
		if err := os.MkdirAll(filepath.Join(r.dir, "dc"), 0o700); err != nil {
			t.Fatal(err)
		}
		r.env = append(r.env, "DOCKER_CONFIG="+filepath.Join(r.dir, "dc"), "DOCKER_HOST=ssh://dev@build.example")
		if code, _ := r.run(append([]string{"docker"}, args...)...); code != 0 || len(r.asked) != 1 {
			t.Errorf("%s: code %d, asked %d, want gated", name, code, len(r.asked))
		}
	}
}

// Each way the docker CLI picks its engine, local and remote, as before #90.
func TestTheEngineFollowsTheCLIAtTheGate(t *testing.T) {
	for name, c := range map[string]struct {
		args  []string
		env   []string
		gated bool
	}{
		"a local -H":                        {[]string{"-H", "unix:///var/run/docker.sock"}, []string{"DOCKER_HOST=ssh://dev@build.example"}, true},
		"a remote -H":                       {[]string{"-H", "ssh://dev@build.example"}, nil, false},
		"a local DOCKER_HOST":               {nil, []string{"DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONTEXT=remote"}, true},
		"a remote DOCKER_HOST":              {nil, []string{"DOCKER_HOST=tcp://10.0.0.5:2376", "DOCKER_CONTEXT=local"}, false},
		"a local DOCKER_CONTEXT":            {nil, []string{"DOCKER_CONTEXT=local"}, true},
		"a remote DOCKER_CONTEXT":           {nil, []string{"DOCKER_CONTEXT=remote"}, false},
		"a local currentContext":            {nil, []string{"CURRENT=local"}, true},
		"a remote currentContext":           {nil, []string{"CURRENT=remote"}, false},
		"DOCKER_CONTEXT over current":       {nil, []string{"DOCKER_CONTEXT=local", "CURRENT=remote"}, true},
		"a local --context":                 {[]string{"--context", "local"}, []string{"DOCKER_HOST=ssh://dev@build.example"}, true},
		"--context over DOCKER_CONTEXT":     {[]string{"--context", "remote"}, []string{"DOCKER_CONTEXT=local"}, false},
		"the default context":               {[]string{"--context", "default"}, []string{"CURRENT=remote"}, true},
		"the default context's DOCKER_HOST": {[]string{"--context", "default"}, []string{"DOCKER_HOST=ssh://dev@build.example"}, false},
	} {
		r := newShimRig(t)
		r.fakeSSH()
		r.ask = allow
		dc := filepath.Join(r.dir, "dc")
		for ctx, host := range map[string]string{"local": "unix:///var/run/docker.sock", "remote": "ssh://dev@build.example"} {
			sum := sha256.Sum256([]byte(ctx))
			meta := filepath.Join(dc, "contexts", "meta", hex.EncodeToString(sum[:]))
			if err := os.MkdirAll(meta, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"`+host+`"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r.env = append(r.env, "DOCKER_CONFIG="+dc)
		for _, kv := range c.env {
			if current, ok := strings.CutPrefix(kv, "CURRENT="); ok {
				if err := os.WriteFile(filepath.Join(dc, "config.json"), []byte(`{"currentContext":"`+current+`"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				continue
			}
			r.env = append(r.env, kv)
		}
		argv := append(append([]string{"docker"}, c.args...), "run", "alpine")
		if code, _ := r.run(argv...); code != 0 || r.execed == "" || (len(r.asked) == 1) != c.gated {
			t.Errorf("%s: code %d, execed %q, asked %d, want gated %v", name, code, r.execed, len(r.asked), c.gated)
		}
	}
}

// A context whose engine is remote: its memory is not this Mac's, so the
// call is not asked about, as with -H tcp://….
func TestARemoteContextIsNotGated(t *testing.T) {
	for _, args := range [][]string{
		{"--context", "builder", "run", "alpine"},
		{"run", "alpine"}, // currentContext
	} {
		r := newShimRig(t)
		r.ask = allow
		sum := sha256.Sum256([]byte("builder"))
		meta := filepath.Join(r.dir, "dc", "contexts", "meta", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(meta, 0o700); err != nil {
			t.Fatal(err)
		}
		// A remote tcp name must resolve now (#118), so the test uses a documentation address.
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"tcp://203.0.113.5:2376"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.dir, "dc", "config.json"), []byte(`{"currentContext":"builder"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r.env = append(r.env, "DOCKER_CONFIG="+filepath.Join(r.dir, "dc"))
		if code, _ := r.run(append([]string{"docker"}, args...)...); code != 0 || len(r.asked) != 0 || r.execed == "" {
			t.Errorf("%q: code %d, asked %d, execed %q", args, code, len(r.asked), r.execed)
		}
	}
}

// A remote engine is not this Mac's, whichever way the call names it: a
// lease label on it is not headroom's concern, as with -H ssh://… (#90).
func TestARemoteContextMaySetTheLeaseLabel(t *testing.T) {
	for _, args := range [][]string{
		{"-H", "ssh://dev@build.example", "run", "-l", "dev.headroom.lease=x", "alpine"},
		// A remote tcp name must resolve now (#118), so the test uses a documentation address.
		{"-H", "203.0.113.5:2376", "run", "-l", "dev.headroom.lease=x", "alpine"},
		{"--context", "builder", "run", "-l", "dev.headroom.lease=x", "alpine"},
	} {
		r := newShimRig(t)
		r.fakeSSH()
		r.ask = allow
		sum := sha256.Sum256([]byte("builder"))
		meta := filepath.Join(r.dir, "dc", "contexts", "meta", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(meta, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"ssh://dev@build.example"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r.env = append(r.env, "DOCKER_CONFIG="+filepath.Join(r.dir, "dc"))
		if code, stderr := r.run(append([]string{"docker"}, args...)...); code != 0 || len(r.asked) != 0 || r.execed == "" {
			t.Errorf("%q: code %d, asked %d, execed %q, stderr %q", args, code, len(r.asked), r.execed, stderr)
		}
	}
}

// -H tcp://127.1:2375 is this Mac's loopback, as the docker CLI dials it:
// the call is gated, and its lease label refused (#118).
func TestALoopbackTCPHostIsGated(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	if code, stderr := r.run("docker", "-H", "tcp://127.1:2375", "run", "-l", "dev.headroom.lease=x", "alpine"); code != exitDenied || r.execed != "" {
		t.Errorf("code %d, execed %q, stderr %q, want the label refused", code, r.execed, stderr)
	}
}

// ssh://core@127.1 is this Mac's loopback too, a number rather than an ssh
// alias: the call is gated, and its lease label refused (#118).
func TestALoopbackSSHHostIsGated(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	if code, stderr := r.run("docker", "-H", "ssh://core@127.1:2222", "run", "-l", "dev.headroom.lease=x", "alpine"); code != exitDenied || r.execed != "" {
		t.Errorf("code %d, execed %q, stderr %q, want the label refused", code, r.execed, stderr)
	}
}

// -H "" is the default socket, as the docker CLI reads it (ParseHost in
// docker/cli's opts/hosts.go trims it and takes DefaultHost), not
// DOCKER_HOST: the call is asked about, and its lease label refused.
func TestAnEmptyHostIsThisMacs(t *testing.T) {
	for _, args := range [][]string{
		{"-H", "", "run", "alpine"},
		{"-H=", "run", "-l", "dev.headroom.lease=x", "alpine"},
	} {
		r := newShimRig(t)
		r.ask = allow
		r.env = append(r.env, "DOCKER_CONFIG="+filepath.Join(r.dir, "dc"), "DOCKER_HOST=ssh://dev@build.example")
		code, _ := r.run(append([]string{"docker"}, args...)...)
		if labelled := slices.Contains(args, "-l"); labelled && code != exitDenied || !labelled && len(r.asked) != 1 {
			t.Errorf("%q: code %d, asked %d, want gated", args, code, len(r.asked))
		}
	}
}

// A bare -H name is tcp://name:2375, as the docker CLI dials it
// (parseDockerDaemonHost and ParseTCPAddr in docker/cli's opts/hosts.go),
// never a context of that name.
func TestABareHostNameIsTCP(t *testing.T) {
	for _, c := range []struct {
		args  []string
		gated bool
	}{
		{[]string{"-H", "localhost", "run", "-l", "dev.headroom.lease=x", "alpine"}, true},
		// A remote tcp name must resolve now (#118), so the test uses a documentation address.
		{[]string{"-H", "203.0.113.5", "run", "alpine"}, false},
	} {
		r := newShimRig(t)
		r.ask = allow
		dc := filepath.Join(r.dir, "dc")
		for _, ctx := range []string{"localhost", "203.0.113.5"} {
			sum := sha256.Sum256([]byte(ctx))
			meta := filepath.Join(dc, "contexts", "meta", hex.EncodeToString(sum[:]))
			if err := os.MkdirAll(meta, 0o700); err != nil {
				t.Fatal(err)
			}
			host := map[string]string{"localhost": "ssh://dev@build.example", "203.0.113.5": "unix:///var/run/docker.sock"}[ctx]
			if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"`+host+`"}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		r.env = append(r.env, "DOCKER_CONFIG="+dc)
		code, _ := r.run(append([]string{"docker"}, c.args...)...)
		if gated := code == exitDenied || len(r.asked) == 1; gated != c.gated {
			t.Errorf("%q: code %d, asked %d, want gated %v", c.args, code, len(r.asked), c.gated)
		}
	}
	if got := dockerEndpointIn(func(string) string { return "" }, shim.Parse("docker", []string{"-H", "build.example", "run", "alpine"})); got != "tcp://build.example:2375" {
		t.Errorf("-H build.example: %q, want tcp://build.example:2375", got)
	}
}

// Podman's engine is the one podman picks (#71): a call to a remote
// connection, named with -c, CONTAINER_CONNECTION or as the default, is
// not asked about; one to the machine's, or to one podman cannot find, is.
func TestPodmanCallsAreGatedByTheEnginePodmanPicks(t *testing.T) {
	const (
		far     = podmanFar
		near    = "tcp://127.0.0.1:7"
		machine = podmanMachine
	)
	// The connections file a call finds: PODMAN_CONNECTIONS_CONF's (the
	// default), XDG_CONFIG_HOME's, or PODMAN_CONNECTIONS_CONF naming one
	// that is missing, malformed or has no default.
	type connFile int
	const (
		confFile connFile = iota
		xdgFile
		missingFile
		malformedFile
		namelessFile
	)
	for name, c := range map[string]struct {
		args  []string
		env   []string
		def   string // the file's default connection
		file  connFile
		gated bool
	}{
		"-c remote-a":                          {args: []string{"-c", "remote-a"}, gated: false},
		"--connection remote-a":                {args: []string{"--connection", "remote-a"}, gated: false},
		"CONTAINER_CONNECTION":                 {env: []string{"CONTAINER_CONNECTION=remote-a"}, gated: false},
		"the default connection":               {def: "remote-a", gated: false},
		"the machine's default":                {def: "podman-machine-default", gated: true},
		"-c the machine":                       {args: []string{"-c", "podman-machine-default"}, gated: true},
		"-c remote-a before a local --url":     {args: []string{"-c", "remote-a", "--url", near}, gated: false},
		"-c remote-a after a local --url":      {args: []string{"--url", near, "-c", "remote-a"}, gated: false},
		"-c the machine before a far --url":    {args: []string{"-c", "podman-machine-default", "--url", far}, gated: true},
		"-c the machine after a far --url":     {args: []string{"--url", far, "-c", "podman-machine-default"}, gated: true},
		"a far --url over the machine's":       {args: []string{"--url", far}, env: []string{"CONTAINER_CONNECTION=podman-machine-default"}, gated: false},
		"CONTAINER_CONNECTION over the host":   {env: []string{"CONTAINER_CONNECTION=podman-machine-default", "CONTAINER_HOST=" + far}, gated: true},
		"-c over CONTAINER_HOST":               {args: []string{"-c", "podman-machine-default"}, env: []string{"CONTAINER_HOST=" + far}, gated: true},
		"a far CONTAINER_HOST":                 {env: []string{"CONTAINER_HOST=" + far}, gated: false},
		"an unknown -c":                        {args: []string{"-c", "ghost"}, env: []string{"CONTAINER_HOST=" + far}, gated: true},
		"an unknown CONTAINER_CONNECTION":      {env: []string{"CONTAINER_CONNECTION=ghost", "CONTAINER_HOST=" + far}, gated: true},
		"a missing file":                       {args: []string{"-c", "remote-a"}, env: []string{"CONTAINER_HOST=" + far}, file: missingFile, gated: true},
		"a malformed file":                     {args: []string{"-c", "remote-a"}, env: []string{"CONTAINER_HOST=" + far}, file: malformedFile, gated: true},
		"XDG_CONFIG_HOME's file":               {def: "remote-a", file: xdgFile, gated: false},
		"no default, a far \"\" connection":    {file: namelessFile, gated: true},
		"an empty --url, a far CONTAINER_HOST": {args: []string{"--url="}, env: []string{"CONTAINER_HOST=" + far}, gated: false},
		// Plan amendment 1's measured table.
		"an empty --url, the machine's CONTAINER_HOST": {args: []string{"--url="}, env: []string{"CONTAINER_CONNECTION=remote-a", "CONTAINER_HOST=" + machine}, gated: true},
		"an empty --url, no CONTAINER_HOST":            {args: []string{"--url="}, env: []string{"CONTAINER_CONNECTION=remote-a"}, gated: true},
		"the machine's --url after --context":          {args: []string{"--context", "remote-a", "--url", machine}, gated: true},
		"the machine's --url before --context":         {args: []string{"--url", machine, "--context", "remote-a"}, gated: true},
		"--context default":                            {args: []string{"--context", "default"}, def: "podman-machine-default", gated: true},
		"--context default, CONTAINER_CONNECTION":      {args: []string{"--context", "default"}, env: []string{"CONTAINER_CONNECTION=remote-a"}, def: "podman-machine-default", gated: false},
		"--context over the machine's CONTAINER_HOST":  {args: []string{"--context", "remote-a"}, env: []string{"CONTAINER_HOST=" + machine}, gated: false},
		"--context over CONTAINER_CONNECTION":          {args: []string{"--context", "remote-a"}, env: []string{"CONTAINER_CONNECTION=podman-machine-default"}, gated: false},
		"-c remote-a after the machine's -H":           {args: []string{"-H", machine, "-c", "remote-a"}, gated: false},
	} {
		r := newShimRig(t)
		r.fakeSSH()
		r.ask = allow
		r.env = append(r.env, c.env...)
		conf, body := filepath.Join(r.dir, "podman-connections.json"), podmanConnections(c.def)
		switch c.file {
		case confFile:
			r.env = append(r.env, "PODMAN_CONNECTIONS_CONF="+conf)
		case xdgFile:
			conf = filepath.Join(r.dir, "xdg", "containers", "podman-connections.json")
			r.env = append(r.env, "XDG_CONFIG_HOME="+filepath.Join(r.dir, "xdg"))
		case missingFile:
			r.env = append(r.env, "PODMAN_CONNECTIONS_CONF="+filepath.Join(r.dir, "none.json"))
		case malformedFile:
			r.env = append(r.env, "PODMAN_CONNECTIONS_CONF="+conf)
			body = podmanMalformed
		case namelessFile:
			r.env = append(r.env, "PODMAN_CONNECTIONS_CONF="+conf)
			body = podmanNameless
		}
		if err := os.MkdirAll(filepath.Dir(conf), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		argv := append(append([]string{"podman"}, c.args...), "run", "alpine")
		if code, _ := r.run(argv...); code != 0 || r.execed == "" || (len(r.asked) == 1) != c.gated {
			t.Errorf("%s: code %d, execed %q, asked %d, want gated %v", name, code, r.execed, len(r.asked), c.gated)
		}
	}
}

// docker --config DIR --context X compose reads X from DIR, as Compose
// does: it runs as a docker CLI plugin, under the CLI's global options.
func TestComposeReadsItsContextFromTheCallsConfig(t *testing.T) {
	r := newShimRig(t)
	r.ask = allow
	for dir, host := range map[string]string{"a": "ssh://dev@build.example", "b": "unix:///var/run/docker.sock"} {
		sum := sha256.Sum256([]byte("foo"))
		meta := filepath.Join(r.dir, dir, "contexts", "meta", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(meta, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"`+host+`"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.env = append(r.env, "DOCKER_CONFIG="+filepath.Join(r.dir, "a"))
	if code, _ := r.run("docker", "--config", filepath.Join(r.dir, "b"), "--context", "foo", "compose", "-p", "proj", "up"); code != 0 || len(r.asked) != 1 {
		t.Errorf("code %d, asked %d, want gated", code, len(r.asked))
	}
}

// composeDir is the working dir Compose labels a project with, as probed on
// Compose v5 (#158): --project-directory, else the dir of the first -f that
// is not -, else of COMPOSE_FILE's first entry, else of the compose file
// found in the working dir or a parent, else (-f - alone) the working dir;
// cleaned, absolute, symlinks kept. Unsure is "", never a guess.
func TestComposeDir(t *testing.T) {
	const cwd = "/Users/dev/project-a/sub"
	wd := func() (string, error) { return cwd, nil }
	noWd := func() (string, error) { return "", errors.New("getwd: gone") }
	found := func(p string) (fs.FileInfo, error) {
		if p == "/Users/dev/project-a/compose.yaml" || p == "/tmp/link/compose.yaml" {
			return nil, nil
		}
		return nil, fs.ErrNotExist
	}
	none := func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		wd   func() (string, error)
		stat func(string) (fs.FileInfo, error)
		left time.Duration
		want string
	}{
		{"--project-directory, relative", []string{"--project-directory", "../b", "-f", "x.yaml"}, nil, wd, none, time.Second, "/Users/dev/project-a/b"},
		{"--project-directory, absolute", []string{"--project-directory", "/srv/api/"}, nil, wd, none, time.Second, "/srv/api"},
		{"--project-directory beats COMPOSE_FILE", []string{"--project-directory", "/srv/api"}, map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml"}, wd, none, time.Second, "/srv/api"},
		{"-f", []string{"-f", "../other/x.yaml"}, nil, wd, none, time.Second, "/Users/dev/project-a/other"},
		{"-f, cleaned", []string{"-f", "a/../b/x.yaml"}, nil, wd, none, time.Second, "/Users/dev/project-a/sub/b"},
		{"the first -f", []string{"-f", "/srv/c/x.yaml", "-f", "/srv/d/y.yaml"}, nil, wd, none, time.Second, "/srv/c"},
		{"the first -f not stdin's", []string{"-f", "-", "-f", "/srv/c/x.yaml"}, nil, wd, none, time.Second, "/srv/c"},
		{"-f - alone", []string{"-f", "-"}, nil, wd, none, time.Second, cwd},
		{"-f beats COMPOSE_FILE", []string{"-f", "/srv/c/x.yaml"}, map[string]string{"COMPOSE_FILE": "/srv/d/y.yaml"}, wd, none, time.Second, "/srv/c"},
		{"COMPOSE_FILE's first entry", nil, map[string]string{"COMPOSE_FILE": "../c/x.yaml:y.yaml"}, wd, found, time.Second, "/Users/dev/project-a/c"},
		{"COMPOSE_FILE, COMPOSE_PATH_SEPARATOR", nil, map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml;y.yaml", "COMPOSE_PATH_SEPARATOR": ";"}, wd, found, time.Second, "/srv/c"},
		{"found in a parent", nil, nil, wd, found, time.Second, "/Users/dev/project-a"},
		{"found through a symlink, kept", nil, nil, func() (string, error) { return "/tmp/link/sub", nil }, found, time.Second, "/tmp/link"},
		{"none found", nil, nil, wd, none, time.Second, ""},
		{"the search out of time", nil, nil, wd, found, 0, ""},
		{"--workdir", []string{"--workdir", "/srv/api", "-f", "/srv/c/x.yaml"}, nil, wd, none, time.Second, ""},
		{"--workdir=", []string{"--workdir=/srv/api", "-f", "/srv/c/x.yaml"}, nil, wd, none, time.Second, ""},
		{"no working dir, -f relative", []string{"-f", "x.yaml"}, nil, noWd, none, time.Second, ""},
		{"no working dir, -f absolute", []string{"-f", "/srv/c/x.yaml"}, nil, noWd, none, time.Second, "/srv/c"},
		{"no working dir, --project-directory absolute", []string{"--project-directory", "/srv/api"}, nil, noWd, none, time.Second, "/srv/api"},
		{"no working dir, no -f", nil, nil, noWd, found, time.Second, ""},
		{"no working dir, -f - alone", []string{"-f", "-"}, nil, noWd, none, time.Second, ""},
		{"a remote -f", []string{"-f", "https://github.com/org/repo.git"}, nil, wd, none, time.Second, ""},
		{"an OCI -f", []string{"-f", "oci://registry.example/app:1"}, nil, wd, none, time.Second, ""},
		{"a remote COMPOSE_FILE", nil, map[string]string{"COMPOSE_FILE": "git@github.com:org/repo.git;x.yaml", "COMPOSE_PATH_SEPARATOR": ";"}, wd, found, time.Second, ""},
	} {
		args := slices.Concat([]string{"compose"}, tc.args, []string{"up", "-d"})
		got := composeDir(args, shim.Parse("docker", args), envMap(tc.env), tc.wd, tc.left, tc.stat)
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A COMPOSE_FILE that an env file sets moves the label (the probe's .env and
// --env-file cases): the shim does not follow it, and sends no dir. One in
// the call's environment beats the env files, and -f beats both (#158). So
// does a COMPOSE_PATH_SEPARATOR an env file sets, which splits the
// environment's COMPOSE_FILE, and a COMPOSE_FILE entry of stdin (review
// round 1).
func TestComposeDirWithEnvFiles(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", "services: {}\n")
	write("other.env", "COMPOSE_FILE=sub/y.yaml\n")
	write("sep.env", "COMPOSE_PATH_SEPARATOR=;\n")
	wd := func() (string, error) { return dir, nil }
	for _, tc := range []struct {
		name string
		dot  string // .env, none when ""
		args []string
		env  map[string]string
		want string
	}{
		{".env sets COMPOSE_FILE", "COMPOSE_FILE=sub/y.yaml\n", nil, nil, ""},
		{".env sets something else", "COMPOSE_PROJECT_NAME=x\n", nil, nil, dir},
		{"--env-file sets COMPOSE_FILE", "", []string{"--env-file", "other.env"}, nil, ""},
		{"COMPOSE_ENV_FILES sets COMPOSE_FILE", "", nil, map[string]string{"COMPOSE_ENV_FILES": "other.env"}, ""},
		{".env disabled", "COMPOSE_FILE=sub/y.yaml\n", nil, map[string]string{"COMPOSE_DISABLE_ENV_FILE": "true"}, dir},
		{"the environment's COMPOSE_FILE beats .env's", "COMPOSE_FILE=sub/y.yaml\n", nil, map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml"}, "/srv/c"},
		{"-f beats .env's", "COMPOSE_FILE=sub/y.yaml\n", []string{"-f", "/srv/c/x.yaml"}, nil, "/srv/c"},
		{"an --env-file that does not exist", "", []string{"--env-file", "missing.env"}, nil, ""},
		{".env sets COMPOSE_PATH_SEPARATOR", "COMPOSE_PATH_SEPARATOR=;\n", nil, map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml;b"}, ""},
		{"--env-file sets COMPOSE_PATH_SEPARATOR", "", []string{"--env-file", "sep.env"}, map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml;b"}, ""},
		{"the environment's COMPOSE_PATH_SEPARATOR beats .env's", "COMPOSE_PATH_SEPARATOR=,\n", nil,
			map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml;b", "COMPOSE_PATH_SEPARATOR": ";"}, "/srv/c"},
		{"COMPOSE_FILE is stdin", "", nil, map[string]string{"COMPOSE_FILE": "-"}, ""},
		{"a COMPOSE_FILE entry is stdin", "", nil, map[string]string{"COMPOSE_FILE": "/srv/c/x.yaml" + string(filepath.ListSeparator) + "-"}, ""},
	} {
		_ = os.Remove(filepath.Join(dir, ".env"))
		if tc.dot != "" {
			write(".env", tc.dot)
		}
		args := slices.Concat([]string{"compose"}, tc.args, []string{"up", "-d"})
		if got := composeDir(args, shim.Parse("docker", args), envMap(tc.env), wd, time.Second, os.Stat); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The working dir is the shell's, through a symlink, as Compose labels it
// (the probe's symlinked cwd): Go's os.Getwd keeps $PWD (#158).
func TestComposeDirKeepsALogicalWorkingDir(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target, link := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(link, "sub"))
	t.Setenv("PWD", filepath.Join(link, "sub")) // as a shell's cd sets it
	args := []string{"compose", "up", "-d"}
	if got := composeDir(args, shim.Parse("docker", args), noEnv, os.Getwd, time.Second, os.Stat); got != link {
		t.Errorf("%q, want %q", got, link)
	}
}

// The shim sends Compose's working dir with every docker compose check:
// when config names the project, with -p, with a guess, and with stdin's
// file; "" when it cannot be sure (#158).
func TestTheComposeDirIsSent(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=w")
	r.ask = allow
	proj := filepath.Join(r.dir, "proj")
	if err := os.MkdirAll(filepath.Join(proj, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.getwd = func() (string, error) { return filepath.Join(proj, "sub"), nil }
	var config []byte
	r.composeAsk = func(string, []string) ([]byte, error) {
		if config == nil {
			return nil, errors.New("exit status 1")
		}
		return config, nil
	}
	for _, tc := range []struct {
		name   string
		config []byte
		call   []string
		want   string
	}{
		{"config names it", []byte(`{"name":"x"}`), []string{"docker", "compose", "up", "-d"}, proj},
		{"-p", nil, []string{"docker", "compose", "-p", "p", "--project-directory", "/srv/api", "up", "-d"}, "/srv/api"},
		{"a guess", nil, []string{"docker", "compose", "up", "-d"}, proj},
		{"stdin", nil, []string{"docker", "compose", "-f", "-", "up", "-d"}, filepath.Join(proj, "sub")},
		{"--workdir", []byte(`{"name":"x"}`), []string{"docker", "compose", "--workdir", "/srv/api", "up", "-d"}, ""},
	} {
		config = tc.config
		r.asked = nil
		r.run(tc.call...)
		if len(r.asked) != 1 || r.asked[0].ComposeDir != tc.want {
			t.Errorf("%s: asked %+v, want dir %q", tc.name, r.asked, tc.want)
		}
	}
}

// A stdin up whose env file sets COMPOSE_PROJECT_NAME to a value the shim
// cannot know (#122) is sent marked unnamed, with the dir Compose labels
// its stack with, which keys its lease, and the dir's name as its Target
// for an older daemon, which a newer one ignores (#122 B2). A
// known name, one Compose does not take from the file, and an unknown one
// with no dir to send are as before.
func TestAnUnknowableStdinNameIsSentAsUnnamed(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "ORCA_WORKTREE_ID=w")
	r.ask = allow
	proj := filepath.Join(r.dir, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	r.getwd = func() (string, error) { return proj, nil }
	pad := strings.Repeat("# padding\n", envFileMax/10+1)
	for _, tc := range []struct {
		name, env string
		call      []string
		target    string
		unnamed   bool
		dir       string
	}{
		{"a $", "COMPOSE_PROJECT_NAME=$X\n", nil, "proj", true, proj},
		{"a backslash in double quotes", "COMPOSE_PROJECT_NAME=\"a\\tb\"\n", nil, "proj", true, proj},
		{"a .env past the limit", "COMPOSE_PROJECT_NAME=early\n" + pad, nil, "proj", true, proj},
		{"a known name", "COMPOSE_PROJECT_NAME=Named\n", nil, "named", false, proj},
		{"an empty name: the dir's", "COMPOSE_PROJECT_NAME=\n", nil, "proj", false, proj},
		{"a missing --env-file, which Compose fails on: the dir's", "COMPOSE_PROJECT_NAME=$X\n",
			[]string{"docker", "compose", "--project-directory", proj, "--env-file", "missing.env", "-f", "-", "up", "-d"}, "proj", false, proj},
		{"no dir to send: the dir's", "COMPOSE_PROJECT_NAME=$X\n",
			[]string{"docker", "compose", "--workdir", "/srv/api", "-f", "-", "up", "-d"}, "proj", false, ""},
	} {
		if err := os.WriteFile(filepath.Join(proj, ".env"), []byte(tc.env), 0o644); err != nil {
			t.Fatal(err)
		}
		call := tc.call
		if call == nil {
			call = []string{"docker", "compose", "--project-directory", proj, "-f", "-", "up", "-d"}
		}
		r.asked = nil
		r.run(call...)
		if len(r.asked) != 1 || r.asked[0].Target != tc.target || r.asked[0].Unnamed != tc.unnamed || r.asked[0].ComposeDir != tc.dir {
			t.Errorf("%s: asked %+v, want target %q, unnamed %v, dir %q", tc.name, r.asked, tc.target, tc.unnamed, tc.dir)
		}
	}
}

// A pod start names its pods, and a kube play the pods its kube files
// define (each Pod's metadata.name), read from the files: its lease binds
// their containers. Files the shim cannot read for sure (stdin, a URL,
// another workload kind, which Podman names itself) leave it no pod: it
// holds its cost. Neither is labelled (#230).
func TestShimGatePodStartAndKubePlay(t *testing.T) {
	r := newShimRig(t)
	r.env = append(r.env, "HEADROOM_WORKTREE=w")
	r.getwd = func() (string, error) { return r.dir, nil }
	r.ask = allow
	write := func(name, yaml string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(r.dir, name), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("web.yml", `# two pods and a claim
apiVersion: v1
kind: Pod
metadata:
  labels:
    app: web
  name: "web"  # the pod
spec:
  containers:
  - name: app
    image: alpine
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
---
kind: Pod
apiVersion: v1
metadata:
    name: db
spec:
  containers:
  - name: db
    image: postgres
`)
	write("deploy.yml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n")
	write("flow.yml", "kind: Pod\nmetadata: {name: web}\n")
	for _, c := range []struct {
		argv    []string
		target  string
		targets []string
	}{
		{[]string{"podman", "pod", "start", "web"}, "web", nil},
		{[]string{"podman", "pod", "start", "web", "db"}, "web", []string{"web", "db"}},
		{[]string{"podman", "kube", "play", "--replace", "web.yml"}, "web", []string{"web", "db"}},
		{[]string{"podman", "play", "kube", filepath.Join(r.dir, "web.yml")}, "web", []string{"web", "db"}},
		{[]string{"podman", "kube", "play", "-"}, "", nil},
		{[]string{"podman", "kube", "play", "https://example.com/web.yml"}, "", nil},
		{[]string{"podman", "kube", "play", "deploy.yml"}, "", nil},
		{[]string{"podman", "kube", "play", "flow.yml"}, "", nil},
		{[]string{"podman", "kube", "play", "web.yml", "missing.yml"}, "", nil},
	} {
		r.asked = nil
		code, stderr := r.run(c.argv...)
		if code != 0 || !slices.Equal(r.execArgv, c.argv) || len(r.asked) != 1 {
			t.Fatalf("%q: exit %d, exec %q, asked %d, stderr %q", c.argv, code, r.execArgv, len(r.asked), stderr)
		}
		a := r.asked[0]
		if a.Kind != "pod" || a.CostBytes != 0 || a.Target != c.target || !slices.Equal(a.Targets, c.targets) || a.MultiTarget != (c.targets != nil) || a.Labelled {
			t.Errorf("%q: asked %+v, want target %q, targets %q", c.argv, a, c.target, c.targets)
		}
	}
}
