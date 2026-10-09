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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cybagard/cyba-headroom/internal/config"
	"github.com/cybagard/cyba-headroom/internal/daemon"
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
}

func newShimRig(t testing.TB) *shimRig {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	for _, n := range []string{"docker", "tart"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!real\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &shimRig{t: t, dir: dir, env: []string{"PATH=" + dir, "HEADROOM_CONFIG_DIR=" + dir}, now: time.Unix(1e9, 0), sleepFor: 2 * time.Second}
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
// guess (#84), and so is stdin's when it is the project directory's: a
// name: in the piped file, unseen, beats it (#85). Not -p, not Compose's
// own name, not stdin's from the environment or an env file, which beat a
// name: (compose-go's cli/options.go, withNamePrecedenceLoad).
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
		{"stdin, the project directory's", true, "", []string{"docker", "compose", "--project-directory", "/srv/api", "-f", "-", "up", "-d"}, "api", true},
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

// compose -f - reads its file from stdin, which the shim must not consume:
// the project is what Compose names it then, COMPOSE_PROJECT_NAME or the
// project directory's name (the working directory's).
func TestComposeProjectOfStdin(t *testing.T) {
	wd := func() (string, error) { return "/Users/dev/src/My_App", nil }
	none := noEnv
	if got, _ := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, none, wd); got != "my_app" {
		t.Fatalf("project = %q, want my_app", got)
	}
	env := envMap(map[string]string{"COMPOSE_PROJECT_NAME": "Piped"})
	if got, _ := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, env, wd); got != "piped" {
		t.Fatalf("project = %q, want piped", got)
	}
	if got, _ := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}, ComposeProjectDir: "/srv/_api"}, none, wd); got != "api" {
		t.Fatalf("project = %q, want api", got)
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
		{"an unreadable --env-file is skipped", []string{"compose", "--env-file", "missing.env", "--env-file", "x.env", "-f", "-", "up", "-d"}, nil, "from-x"},
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
		getenv := envMap(tc.env)
		// Only the directory's name is a guess.
		got, guessed := composeStdinProject(shim.Parse("docker", tc.args), getenv, wd)
		if got != tc.want || guessed != (tc.want == "app") {
			t.Errorf("%s: project = %q, guessed %v, want %q", tc.name, got, guessed, tc.want)
		}
	}
}

// An env file is read as compose-go's dotenv parser reads it; each want is
// what Docker Compose 5.5.1 named the project (with OTHER=o in its
// environment), except that a value Compose interpolates is unknown, and
// the directory's name follows.
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
	} {
		if err := os.WriteFile(filepath.Join(app, ".env"), []byte(tc.env), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, _ := composeStdinProject(shim.Call{ComposeFiles: []string{"-"}}, noEnv, wd); got != tc.want {
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
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"tcp://build.example:2376"}}}`), 0o600); err != nil {
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
		{"-H", "build.example:2376", "run", "-l", "dev.headroom.lease=x", "alpine"},
		{"--context", "builder", "run", "-l", "dev.headroom.lease=x", "alpine"},
	} {
		r := newShimRig(t)
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
		{[]string{"-H", "build.example", "run", "alpine"}, false},
	} {
		r := newShimRig(t)
		r.ask = allow
		dc := filepath.Join(r.dir, "dc")
		for _, ctx := range []string{"localhost", "build.example"} {
			sum := sha256.Sum256([]byte(ctx))
			meta := filepath.Join(dc, "contexts", "meta", hex.EncodeToString(sum[:]))
			if err := os.MkdirAll(meta, 0o700); err != nil {
				t.Fatal(err)
			}
			host := map[string]string{"localhost": "ssh://dev@build.example", "build.example": "unix:///var/run/docker.sock"}[ctx]
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

// Podman's engine is the last of --url, -H, --connection and --context on
// the command line, as before #90; podman's own order is #71's.
func TestPodmansEngineIsItsLastEngineFlag(t *testing.T) {
	const far = "ssh://dev@build.example"
	for _, c := range []struct {
		args  []string
		gated bool
	}{
		{[]string{"--url", far, "--connection", "near", "run", "-l", "dev.headroom.lease=x", "alpine"}, true},
		{[]string{"--url", far, "-c", "near", "run", "alpine"}, true},
		{[]string{"-H", far, "--context", "near", "run", "alpine"}, true},
		{[]string{"--connection", "near", "--url", far, "run", "-l", "dev.headroom.lease=x", "alpine"}, false},
		{[]string{"-c", "near", "--url", far, "run", "alpine"}, false},
		{[]string{"--context", "near", "-H", far, "run", "alpine"}, false},
		{[]string{"--url", far, "run", "alpine"}, false},
		{[]string{"-c", "near", "run", "alpine"}, true},
	} {
		r := newShimRig(t)
		r.ask = allow
		if err := os.WriteFile(filepath.Join(r.dir, "podman"), []byte("#!real\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		r.env = append(r.env, "DOCKER_HOST="+far)
		code, _ := r.run(append([]string{"podman"}, c.args...)...)
		if gated := code == exitDenied || len(r.asked) == 1; gated != c.gated {
			t.Errorf("%q: code %d, asked %d, want gated %v", c.args, code, len(r.asked), c.gated)
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
